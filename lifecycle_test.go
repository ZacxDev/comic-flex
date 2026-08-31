package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/comic-flex/internal/control"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
)

// This file covers the process-lifecycle seams the round-1 suite never
// exercised. Each one had a mutant measured green at 200 PASS / 0 FAIL:
//
//	F9  `if true { return nil }` in startControlAPI — the whole API inert.
//	F12 reverting startSlideshow's locked interval read to the racy one.
//	F3  deleting the signal handling this PR added.
//	F4  deleting the synchronous scanning flag before the goroutine starts.
//	F6  the unbounded GTK work queue.

const lifecycleTestToken = "0123456789abcdef0123456789abcdef01234567" // 40 bytes

// freeLoopbackAddr picks a loopback address nothing is listening on.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// ---------------------------------------------------------------------------
// F9 — startControlAPI actually starts something
// ---------------------------------------------------------------------------

// TestStartControlAPIServesOnItsAddress is the LIVENESS half of the fail-closed
// decision, and it goes over a real socket.
//
// 🔴 Round 1 pinned only the refusal direction, and that is walkable in the
// worst possible way: `if true { return nil }` at the top of startControlAPI
// disables the entire control API — no listener, no endpoints, the feature this
// PR exists to add simply absent — and the suite stayed at 200 PASS / 0 FAIL,
// because every assertion about startControlAPI was an assertion that it
// returned nil.
func TestStartControlAPIServesOnItsAddress(t *testing.T) {
	t.Setenv(control.TokenEnvVar, lifecycleTestToken)
	addr := freeLoopbackAddr(t)
	iv := newControlTestViewer(37, "a/1.jpg", "b/2.jpg", "c/3.jpg")

	srv := startControlAPI(iv, addr)
	if srv == nil {
		t.Fatal("startControlAPI returned nil for a valid 40-byte token — the control API " +
			"does not start at all, and the slideshow runs with no control surface")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	if got := srv.Addr(); got != addr {
		t.Fatalf("srv.Addr() = %q, want %q", got, addr)
	}

	base := "http://" + addr
	client := &http.Client{Timeout: 2 * time.Second}

	// The listener is started in a goroutine, so poll rather than assume.
	var last error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /healthz -> %d (%s)", resp.StatusCode, body)
			}
			var health struct {
				OK      bool   `json:"ok"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(body, &health); err != nil {
				t.Fatalf("/healthz body is not JSON: %v (%s)", err, body)
			}
			if !health.OK {
				t.Fatalf("/healthz reports ok=false: %s", body)
			}
			if health.Version != version {
				t.Fatalf("/healthz version = %q, want %q", health.Version, version)
			}
			last = nil
			break
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("nothing ever answered on %s: %v — startControlAPI returned a server that "+
			"is not listening", addr, last)
	}

	// It is the REAL routed handler, wired to the REAL viewer: an authenticated
	// read must report this viewer's gallery, and an unauthenticated one must
	// be refused.
	req, _ := http.NewRequest("GET", base+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+lifecycleTestToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /api/state -> %d (%s)", resp.StatusCode, body)
	}
	var snap control.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("/api/state body is not JSON: %v (%s)", err, body)
	}
	if snap.Total != 3 || snap.SlideInterval != 37 {
		t.Fatalf("/api/state = %+v, want total 3 and slide_interval 37 — the server is not "+
			"reading the viewer it was handed", snap)
	}

	resp, err = client.Get(base + "/api/state")
	if err != nil {
		t.Fatalf("unauthenticated GET /api/state: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/state over the wire -> %d, want 401", resp.StatusCode)
	}

	// And Shutdown genuinely stops it, which is what main() relies on after
	// gtk.Main() returns.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := client.Get(base + "/healthz"); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the listener was still answering two seconds after Shutdown returned")
}

// ---------------------------------------------------------------------------
// F12 — startSlideshow reads the interval under the lock
// ---------------------------------------------------------------------------

// TestStartSlideshowReadsTheIntervalUnderTheLock is the race this PR claims to
// close, and until now nothing exercised startSlideshow at all: reverting the
// accessor to `iv.config.SlideInterval*1000` left the suite at 200 PASS / 0 FAIL.
//
// It is a genuine -race detection, not a structural check: POST /api/interval
// writes config.SlideInterval from an enqueued closure while the slideshow timer
// re-arms itself from the same field, and the race detector reports the
// unsynchronised pair. glib.TimeoutAdd registers a source without needing
// gtk.Init or a running main loop, so this needs no display.
func TestStartSlideshowReadsTheIntervalUnderTheLock(t *testing.T) {
	iv := newControlTestViewer(37, "a/1.jpg")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The values a POST /api/interval closure would write. None equals the
		// 37 the fixture starts with, so a stale read is a wrong read.
		for i := 0; i < 200; i++ {
			iv.setSlideInterval(uint(41 + i%13))
		}
	}()
	for i := 0; i < 50; i++ {
		iv.startSlideshow()
	}
	wg.Wait()

	// Retire the source the last startTimer armed, so the test leaves no
	// callback registered in the shared default main context.
	if previous := iv.swapTimeout(0); previous != 0 {
		glib.SourceRemove(previous)
	}

	if got := iv.slideInterval(); got < 41 || got > 53 {
		t.Fatalf("slideInterval = %d after the writer ran, want 41..53", got)
	}
}

// ---------------------------------------------------------------------------
// F3 — a second signal must be able to kill the process
// ---------------------------------------------------------------------------

// TestASecondSignalExitsImmediately pins the half of signal handling that turns
// an interception into a REGRESSION if it is missing.
//
// Before this PR, SIGTERM killed comic-flex instantly. The PR intercepts it, and
// the round-1 handler read the channel exactly ONCE and returned — so every
// later SIGINT/SIGTERM sat unread in the buffer and only SIGKILL could still
// stop the process. Deleting the whole block was measured at 200 PASS / 0 FAIL.
//
// 🔴 CORRECTED. The round-1 version of this comment said `systemctl stop` then
// "waits out TimeoutStopSec (90 s)". That is wrong and it is worth stating so:
// the read-once handler acted on the FIRST signal, and systemd sends exactly one
// SIGTERM before SIGKILLing at the timeout — it never re-sends, so the ordinary
// stop was never affected. The property this test actually pins is the SECOND
// signal: an operator's second Ctrl-C, or a second `kill`, must force an exit,
// which is what you need when the graceful quit cannot finish because the GTK
// loop is inside a 30 s S3 GET. That is a real regression to guard against —
// intercepting a signal must not make the process harder to stop than the
// default it replaced — it is just not a 90 second one.
func TestASecondSignalExitsImmediately(t *testing.T) {
	sigCh := make(chan os.Signal, 4)
	quits := make(chan struct{}, 4)
	exits := make(chan int, 4)

	installSignalHandler(sigCh, func() { quits <- struct{}{} }, func(code int) { exits <- code })

	sigCh <- syscall.SIGTERM
	// The quit is SCHEDULED on the GTK main loop, so turn the loop until it
	// runs. That is also what proves it was scheduled rather than called on the
	// signal goroutine, where it would touch GTK off the main thread.
	ctx := glib.MainContextDefault()
	deadline := time.Now().Add(3 * time.Second)
	for len(quits) == 0 && time.Now().Before(deadline) {
		ctx.Iteration(false)
	}
	select {
	case <-quits:
	case <-time.After(3 * time.Second):
		t.Fatal("the FIRST signal did not schedule a graceful quit — SIGTERM is being swallowed")
	}
	select {
	case code := <-exits:
		t.Fatalf("the first signal hard-exited with %d instead of quitting the main loop "+
			"gracefully; the display would die without the control API shutting down", code)
	default:
	}

	sigCh <- syscall.SIGTERM
	// The hard exit must NOT need the main loop — that is the whole point: it
	// happens even when the loop is wedged. So no Iteration here.
	select {
	case code := <-exits:
		if code == 0 {
			t.Fatal("the second signal exited with status 0 — systemd cannot tell a killed " +
				"process from a clean stop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a second SIGTERM did NOTHING. The handler read the channel once and returned, " +
			"so every later signal is buffered and dropped and only SIGKILL can still stop the " +
			"process. An operator whose first Ctrl-C found the GTK loop inside a 30 s S3 GET has " +
			"no second try — pressing it again is inert.")
	}
	select {
	case <-quits:
		t.Fatal("the second signal scheduled another graceful quit instead of exiting — if the " +
			"first one did not drain, neither will the second")
	default:
	}
}

// TestSignalHandlerSurvivesAClosedChannel: the goroutine must not panic or spin
// when Notify is stopped and the channel closed.
func TestSignalHandlerSurvivesAClosedChannel(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	installSignalHandler(sigCh, func() { close(done) }, func(int) { t.Error("hard exit on a closed channel") })
	close(sigCh)
	select {
	case <-done:
		t.Fatal("closing the channel was treated as a signal")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestQuitJumpsTheQueueOfPendingWork pins the OTHER consequence of intercepting
// SIGTERM, behaviourally.
//
// idleOnce schedules at G_PRIORITY_DEFAULT_IDLE, which is where every
// control-API mutation closure sits, and updateSingleImage inside one of those
// can block for 30 s on an S3 GET. A quit queued at the same priority is
// therefore LAST in line behind an arbitrary backlog.
//
// 🔴 It drives scheduleQuit — the function that MAKES the priority decision —
// and not idleHigh. An earlier version of this test called idleHigh directly;
// it proved that idleHigh is high-priority and nothing whatsoever about the
// shutdown path, and changing scheduleQuit's body from idleHigh to idleOnce was
// measured to leave the whole suite green.
func TestQuitJumpsTheQueueOfPendingWork(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	// A backlog of ordinary mutations, exactly as POST /api/next queues them.
	const backlog = 5
	for i := 0; i < backlog; i++ {
		i := i
		idleOnce(func() { note(fmt.Sprintf("mutation-%d", i)) })
	}
	// ...and then a shutdown arrives, through the production function.
	scheduleQuit(func() { note("quit") })

	ctx := glib.MainContextDefault()
	for i := 0; i < 500 && len(order) < backlog+1; i++ {
		ctx.Iteration(false)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != backlog+1 {
		t.Fatalf("only %d of %d callbacks ran: %v", len(order), backlog+1, order)
	}
	if order[0] != "quit" {
		t.Fatalf("the main loop ran %v — scheduleQuit put the quit BEHIND %d mutation closures, "+
			"each of which can sit on an S3 GET for 30s. systemctl stop would wait for all of "+
			"them, then TimeoutStopSec, then SIGKILL.", order, backlog)
	}
}

// TestTheSignalHandlerSchedulesThroughScheduleQuit closes the one seam the
// behavioural tests above cannot reach.
//
// TestQuitJumpsTheQueueOfPendingWork proves scheduleQuit is high-priority;
// TestASecondSignalExitsImmediately proves the handler schedules SOMETHING that
// the main loop runs. Neither can tell whether the handler reached the main loop
// through scheduleQuit or through idleOnce, because both end with the closure
// running — the difference is only the ORDER relative to a backlog this test
// cannot inject between the signal arriving and the handler scheduling.
//
// So it is asserted structurally, with a positive control below.
func TestTheSignalHandlerSchedulesThroughScheduleQuit(t *testing.T) {
	got := scanSchedulerCalls(t, "main.go", "installSignalHandler")
	if !got["scheduleQuit"] {
		t.Error("installSignalHandler does not call scheduleQuit. Whatever it schedules the " +
			"quit with, it is not the function whose priority is pinned by " +
			"TestQuitJumpsTheQueueOfPendingWork.")
	}
	if got["idleOnce"] {
		t.Error("installSignalHandler calls idleOnce. That is G_PRIORITY_DEFAULT_IDLE — the " +
			"same queue every control-API mutation closure sits in — so shutdown waits behind " +
			"a backlog of up to 30s image loads apiece.")
	}
}

// TestSchedulerCallScannerCanFire is the positive control for the guard above.
func TestSchedulerCallScannerCanFire(t *testing.T) {
	const mutant = `package main

func installSignalHandler(sigCh <-chan os.Signal, quit func(), hardExit func(int)) {
	go func() {
		for sig := range sigCh {
			_ = sig
			idleOnce(quit)
		}
	}()
}
`
	got := scanSchedulerCallsSource(t, "mutant.go", mutant, "installSignalHandler")
	if !got["idleOnce"] {
		t.Error("the scanner did NOT see idleOnce in a handler that plainly calls it — it " +
			"cannot observe the hazard, so its clean verdict on main.go means nothing")
	}
	if got["scheduleQuit"] {
		t.Error("the scanner reported scheduleQuit in source that does not call it")
	}

	// And it must not fire on the correct shape.
	const good = `package main

func installSignalHandler(sigCh <-chan os.Signal, quit func(), hardExit func(int)) {
	go func() {
		for sig := range sigCh {
			_ = sig
			scheduleQuit(quit)
		}
	}()
}
`
	got = scanSchedulerCallsSource(t, "good.go", good, "installSignalHandler")
	if !got["scheduleQuit"] || got["idleOnce"] {
		t.Errorf("the scanner misjudged correct source: %v", got)
	}

	// 🔴 The form scanImagesAsync uses: the scheduler is never CALLED here, it is
	// handed to a helper. A scanner that only records callee identifiers reports
	// the empty set for this, which reads exactly like "schedules nothing".
	const handedOn = `package main

func scanImagesAsync() bool {
	return scanImagesAsyncVia(idleOnce)
}
`
	got = scanSchedulerCallsSource(t, "handedon.go", handedOn, "scanImagesAsync")
	if !got["idleOnce"] {
		t.Error("the scanner did NOT see idleOnce passed as a value to scanImagesAsyncVia — it " +
			"cannot tell that form apart from `scanImagesAsyncVia(runInline)`, which would run " +
			"the rendering completion closure straight on the listing goroutine")
	}
	if got["idleHigh"] {
		t.Error("the scanner reported idleHigh in source that neither calls nor passes it")
	}

	const handedOnWrong = `package main

func scanImagesAsync() bool {
	return scanImagesAsyncVia(idleHigh)
}
`
	got = scanSchedulerCallsSource(t, "handedonwrong.go", handedOnWrong, "scanImagesAsync")
	if !got["idleHigh"] || got["idleOnce"] {
		t.Errorf("the scanner misjudged a handed-on idleHigh: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Layering — control_adapter.go is the only file in package main that names
// internal/control (round-2 finding 7)
// ---------------------------------------------------------------------------

const controlImportPath = "github.com/ZacxDev/comic-flex/internal/control"

// TestOnlyTheAdapterImportsTheControlPackage turns control_adapter.go's header
// comment into something that can fail.
//
// 🔴 That comment claimed the file was "the ONLY place that knows both about
// glib and about internal/control", and it was false the moment it was written:
// the same round added `control.DefaultAddr` to main.go, which imports gtk, gdk
// and glib. Nothing failed, because the only structural guard covered the
// CONVERSE direction (internal/control must not reach GTK). A comment is a claim
// too, and this is the claim.
//
// The port exists so the endpoint surface stays testable without a display; that
// property survives an extra import. What does not survive is a reader's ability
// to find every place the two layers meet by opening one file — which is what
// the comment promises, and now what this test enforces.
func TestOnlyTheAdapterImportsTheControlPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var scanned, importers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned = append(scanned, name)
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == controlImportPath {
				importers = append(importers, name)
			}
		}
	}

	// Positive control on the RESULT SET: a scan that found no files, or found
	// the import nowhere, would be vacuously clean. package main HAS to import
	// internal/control somewhere or the control API is not wired up at all.
	if len(scanned) < 3 {
		t.Fatalf("only %d non-test sources scanned (%v) — this guard is not looking at the "+
			"package it claims to check", len(scanned), scanned)
	}
	if len(importers) == 0 {
		t.Fatalf("no file in package main imports %s, across %v. Either the control API is not "+
			"wired into the binary at all, or this scan is broken; both make the assertion below "+
			"meaningless.", controlImportPath, scanned)
	}

	if len(importers) != 1 || importers[0] != "control_adapter.go" {
		t.Errorf("%v import %s. control_adapter.go's header says it is the only file in package "+
			"main that does, and a reader trusts that to find every place the GTK side and the "+
			"port meet. Re-export what the other file needs (see controlAddr) or correct the "+
			"comment — do not leave the comment claiming more than the code.",
			importers, controlImportPath)
	}
}

// scanSchedulerCalls reports which scheduling functions the named function
// CALLS or HANDS ON as a value.
//
// 🔴 Both, not just calls. `scanImagesAsyncVia(idleOnce)` schedules through
// idleOnce without ever containing a call to it, and the round-2 version of this
// scanner recorded only `call.Fun.(*ast.Ident)` — so it would have reported an
// empty set for the very function whose scheduler it exists to identify, and
// `scanImagesAsyncVia(runInline)` would have read exactly the same. The widening
// is free for the existing caller: installSignalHandler passes neither.
func scanSchedulerCalls(t *testing.T, path, fnName string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return scanSchedulerCallsSource(t, path, string(src), fnName)
}

func scanSchedulerCallsSource(t *testing.T, filename, src, fnName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	var target *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == fnName && fn.Body != nil {
			target = fn
		}
	}
	if target == nil {
		t.Fatalf("no func %s in %s — this guard now pins nothing", fnName, filename)
	}
	out := map[string]bool{}
	ast.Inspect(target.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			out[id.Name] = true
		}
		// A scheduler handed on as a VALUE schedules just as effectively as one
		// that is called here.
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// ---------------------------------------------------------------------------
// F4 — scanning is a COUNT, and it is set before the goroutine starts
// ---------------------------------------------------------------------------

// blockingStore is an ImageStore whose ListImages parks until released.
type blockingStore struct {
	release chan struct{}
	entered chan struct{}
	images  []string
}

func newBlockingStore(images ...string) *blockingStore {
	return &blockingStore{
		release: make(chan struct{}),
		entered: make(chan struct{}, 8),
		images:  images,
	}
}

func (b *blockingStore) ListImages() ([]string, error) {
	b.entered <- struct{}{}
	<-b.release
	out := make([]string, len(b.images))
	copy(out, b.images)
	return out, nil
}

// LoadImage returns an error rather than panicking. scanImagesAsync schedules
// onScanComplete on the SHARED default GTK main context, so this store's
// LoadImage really is reached — by whichever later test iterates that context
// next. Returning an error makes updateSingleImage log and return before it
// touches the (nil) window.
func (b *blockingStore) LoadImage(key string) (*gdk.Pixbuf, error) {
	return nil, fmt.Errorf("blockingStore does not serve images (%s)", key)
}

// drainMainContext runs whatever the default GTK main context has pending, so a
// test that schedules work through idleOnce does not leave it for the next one.
func drainMainContext(t *testing.T) {
	t.Helper()
	ctx := glib.MainContextDefault()
	for i := 0; i < 200 && ctx.Pending(); i++ {
		ctx.Iteration(false)
	}
}

// TestScanningIsACountNotAFlag is defect F4.
//
// 🔴 Two properties, and BOTH shipped unguarded:
//
//  1. scanning is true the instant scanImagesAsync returns, before the goroutine
//     has done anything. Deleting that synchronous set was 200 PASS / 0 FAIL —
//     the point where `scanning` is PRODUCED had no test at all.
//  2. scanning stays true while ANY listing is in flight. With a boolean, the
//     `defer` in whichever goroutine finished FIRST cleared it while the other
//     was still listing, and GET /api/state answered total 0 / scanning false —
//     the "no comics" answer — mid-scan. That is network-reachable: POST
//     /api/rescan twice, or once during the startup scan.
func TestScanningIsACountNotAFlag(t *testing.T) {
	store := newBlockingStore("a.jpg", "b.jpg")
	iv := newControlTestViewer(30)
	iv.store = store
	// scanImagesAsync schedules onScanComplete on the shared default main
	// context; leaving it there hands this store's LoadImage to a later test.
	t.Cleanup(func() { drainMainContext(t) })

	if iv.isScanning() {
		t.Fatal("a viewer that has never scanned reports scanning")
	}

	// First listing, as the startup scan does.
	iv.scanImagesAsync()
	if !iv.isScanning() {
		t.Fatal("scanImagesAsync returned with scanning false. The flag is set inside the " +
			"goroutine instead of before it, so there is a window in which GET /api/state " +
			"answers total 0 / scanning false — 'no comics' — for a gallery that is about to " +
			"be listed.")
	}
	if got := iv.snapshot(); !got.scanning {
		t.Fatalf("snapshot = %+v, want scanning true", got)
	}
	waitFor(t, store.entered, "the first listing to start")

	// Second listing, as POST /api/rescan does mid-scan.
	iv.scanImagesAsync()
	waitFor(t, store.entered, "the second listing to start")
	if !iv.isScanning() {
		t.Fatal("two concurrent listings and scanning is already false")
	}

	// Release ONE. The other is still listing, so scanning must stay true.
	//
	// 🔴 The slot comes back only when the completion closure that listing
	// scheduled has RUN, not when ListImages returned — see scanImagesAsyncVia —
	// so the main context has to be iterated for the count to move at all.
	store.release <- struct{}{}
	waitScanCount(t, iv, 1)
	if n := iv.scanCount(); n != 1 {
		t.Fatalf("after one of two listings finished, scansInFlight = %d, want 1", n)
	}
	if !iv.isScanning() {
		t.Fatal("one of two concurrent listings finished and scanning went FALSE. " +
			"GET /api/state now answers total 0 / scanning false — the 'scanned and empty', " +
			"'no comics' answer — while a listing is still running. That is the exact collapse " +
			"§4.2 says must never happen, and it is reachable with two POST /api/rescan calls.")
	}
	if got := iv.snapshot(); !got.scanning {
		t.Fatalf("mid-scan snapshot = %+v, want scanning true", got)
	}

	// Release the second: now it is genuinely over.
	store.release <- struct{}{}
	waitScanCount(t, iv, 0)
	if iv.isScanning() {
		t.Fatal("both listings finished and scanning is still true — the counter never " +
			"returns to zero, so a client renders 'indexing…' forever")
	}
	if n := iv.scanCount(); n != 0 {
		t.Fatalf("scansInFlight = %d after both listings finished, want 0", n)
	}
}

// TestEndScanDoesNotGoNegative: an unbalanced endScan must not make a later
// tryBeginScan look like nothing is happening.
func TestEndScanDoesNotGoNegative(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.endScan()
	iv.endScan()
	if n := iv.scanCount(); n != 0 {
		t.Fatalf("scansInFlight = %d after two unbalanced endScans, want 0", n)
	}
	if !iv.tryBeginScan() {
		t.Fatal("tryBeginScan was refused on an idle viewer — the counter went negative the " +
			"other way and the scan budget is permanently exhausted")
	}
	if !iv.isScanning() {
		t.Fatal("tryBeginScan after unbalanced endScans did not report scanning — the counter " +
			"went negative and a real scan is now invisible")
	}
}

// ---------------------------------------------------------------------------
// F6b — concurrent bucket listings are bounded (round-2 finding 1)
// ---------------------------------------------------------------------------

// TestConcurrentRescansAreBounded is the regression test for the hole the
// round-1 queue cap left open.
//
// 🔴 It drives the REAL adapter — gtkViewer.Rescan, which is what
// POST /api/rescan reaches — rather than tryBeginScan, because tryBeginScan was
// never the thing that was broken. The round-1 path went through
// enqueueBounded, whose closure spawns the listing onto a goroutine and returns
// in microseconds, so the slot was released before the next request arrived.
// Measured at c7d0a2de with this exact loop against the real enqueueBounded +
// gtkViewer.Rescan: attempts=500 refused=0 queueDepth=0 scansInFlight=500 —
// 500 concurrent MinIO ListObjects, each able to hold for the 2 min listTimeout,
// on a Raspberry Pi.
//
// 500 overshoots maxConcurrentScans by a long way and is not a multiple of it,
// so a mutant that admits "a few extra" is still visible.
func TestConcurrentRescansAreBounded(t *testing.T) {
	store := newBlockingStore("a.jpg", "b.jpg")
	iv := newControlTestViewer(30)
	iv.store = store
	g := gtkViewer{iv: iv}
	t.Cleanup(func() { drainMainContext(t) })

	const attempts = 500
	started, refused := 0, 0
	for i := 0; i < attempts; i++ {
		if g.Rescan() {
			started++
		} else {
			refused++
		}
	}

	if started != maxConcurrentScans {
		t.Fatalf("%d of %d rescans started; want exactly maxConcurrentScans (%d). Nothing is "+
			"bounding the listings: each one is a goroutine holding a MinIO ListObjects for up "+
			"to listTimeout (%s), on a Raspberry Pi.",
			started, attempts, maxConcurrentScans, listTimeout)
	}
	if refused != attempts-maxConcurrentScans {
		t.Fatalf("%d rescans were refused, want %d", refused, attempts-maxConcurrentScans)
	}
	if n := iv.scanCount(); n != maxConcurrentScans {
		t.Fatalf("scansInFlight = %d, want %d", n, maxConcurrentScans)
	}
	// The queue cap must NOT be what did the bounding — that is the illusion the
	// round-1 code sold. A rescan puts nothing on the GTK loop's mutation queue.
	if n := iv.queueDepth(); n != 0 {
		t.Fatalf("queueDepth = %d after %d rescans; a rescan must not consume a GTK mutation "+
			"slot, because that slot is freed in microseconds and bounds nothing", n, attempts)
	}

	// Release the admitted listings. Every ListImages has now returned.
	for i := 0; i < started; i++ {
		store.release <- struct{}{}
	}

	// 🔴 The budget must NOT come back yet, and this is the round-3 finding on
	// the REAL (idleOnce) path rather than through a stand-in scheduler. Until
	// round 3 the goroutine's `defer iv.endScan()` returned the slot here — a
	// scan whose completion closure had merely been SCHEDULED counted as over —
	// so 40 sequential rescans left 40 closures queued on the main context, each
	// one an updateImage and a permanent gotk3 callback-registry entry.
	//
	// A window rather than one sample: at 99a9dca the listing goroutines finish
	// in microseconds, so a single check could pass on timing alone.
	hold := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(hold) {
		if n := iv.scanCount(); n != maxConcurrentScans {
			t.Fatalf("scansInFlight fell to %d with every listing finished but NO completion "+
				"closure run. The slot is being returned when the closure is SCHEDULED rather "+
				"than when it RUNS, so maxConcurrentScans bounds concurrent listings and bounds "+
				"nothing about the closures they queue onto the GTK main loop.", n)
		}
		time.Sleep(time.Millisecond)
	}

	// Now run them, and prove the budget recovers — or the display is stuck on a
	// stale gallery forever after four rescans.
	waitScanCount(t, iv, 0)
	if n := iv.scanCount(); n != 0 {
		t.Fatalf("scansInFlight = %d after every listing returned and the main context was "+
			"drained, want 0", n)
	}
	if !g.Rescan() {
		t.Fatal("a rescan was refused after every listing finished — the bound is a permanent " +
			"wall rather than backpressure")
	}
	store.release <- struct{}{}
	waitScanCount(t, iv, 0)
}

// waitScanCount iterates the GTK main context — running any scan-completion
// closures it holds — until scansInFlight reaches want, or the deadline passes.
//
// It deliberately does NOT assert. The caller's own check is the one carrying
// the diagnosis, and a helper that fataled here would replace it.
func waitScanCount(t *testing.T, iv *ImageViewer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for iv.scanCount() != want && time.Now().Before(deadline) {
		drainMainContext(t)
		time.Sleep(time.Millisecond)
	}
}

// instantStore lists immediately and serves no images.
type instantStore struct{ images []string }

func (s *instantStore) ListImages() ([]string, error) {
	out := make([]string, len(s.images))
	copy(out, s.images)
	return out, nil
}

func (s *instantStore) LoadImage(key string) (*gdk.Pixbuf, error) {
	return nil, fmt.Errorf("instantStore serves no images (%s)", key)
}

// TestTheScanSlotIsHeldUntilTheCompletionClosureRuns is the round-3 regression
// test: maxConcurrentScans must bound OUTSTANDING SCANS, and a scan is not over
// when its completion closure has been scheduled — it is over when that closure
// has run.
//
// 🔴 Measured at 99a9dca, where `defer iv.endScan()` sat in the listing
// goroutine: 40 sequential admitted rescans gave `admitted=40 refused=0
// queueDepth=0` and left 40 completion closures queued on the GTK main context
// at once. Each of those re-enters onScanComplete -> updateImage -> LoadImage —
// a 30 s S3 GET apiece — and holds a gotk3 callback-registry entry, which is
// precisely the growth maxQueuedMutations exists to prevent and which
// maxConcurrentScans' comment claimed to prevent here.
//
// It drives scanImagesAsyncVia with a stand-in scheduler for the same reason
// TestEnqueueIsBounded does: the property is WHEN the slot comes back, and with
// the real idleOnce that is decided by a main loop the test would have to race.
// The real path is covered by TestConcurrentRescansAreBounded's hold window and
// by TestScanImagesAsyncSchedulesThroughTheGTKMainLoop below.
func TestTheScanSlotIsHeldUntilTheCompletionClosureRuns(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.store = &instantStore{images: []string{"a.jpg", "b.jpg"}}

	// The stand-in main loop: it CAPTURES closures and runs none of them.
	var pending []func()
	var mu sync.Mutex
	scheduled := make(chan struct{}, 128)
	schedule := func(fn func()) {
		mu.Lock()
		pending = append(pending, fn)
		mu.Unlock()
		scheduled <- struct{}{}
	}
	outstanding := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(pending)
	}

	const attempts = 40
	admitted, refused := 0, 0
	for i := 0; i < attempts; i++ {
		if iv.scanImagesAsyncVia(schedule) {
			admitted++
			// Wait for this listing to reach the scheduler, so the count below is
			// of closures genuinely outstanding rather than of goroutines that
			// have not started yet.
			waitFor(t, scheduled, "the admitted scan's completion closure to be scheduled")
		} else {
			refused++
		}
	}

	if admitted+refused != attempts {
		t.Fatalf("admitted %d + refused %d != %d attempts", admitted, refused, attempts)
	}
	if admitted != maxConcurrentScans {
		t.Fatalf("%d of %d rescans were admitted with NO completion closure yet run; want exactly "+
			"maxConcurrentScans (%d). The slot is being returned by the listing goroutine, so it "+
			"comes back as soon as the closure is SCHEDULED — %d closures are queued on the GTK "+
			"main loop at once, each one an updateImage and a permanent gotk3 callback-registry "+
			"entry, which is the growth this bound exists to prevent.",
			admitted, attempts, maxConcurrentScans, outstanding())
	}
	if n := outstanding(); n != maxConcurrentScans {
		t.Fatalf("%d completion closures outstanding, want %d", n, maxConcurrentScans)
	}
	if n := iv.scanCount(); n != maxConcurrentScans {
		t.Fatalf("scansInFlight = %d while %d closures are outstanding, want %d",
			n, outstanding(), maxConcurrentScans)
	}
	if n := iv.queueDepth(); n != 0 {
		t.Fatalf("queueDepth = %d; a rescan must not consume a GTK MUTATION slot — that slot is "+
			"freed in microseconds and bounds nothing", n)
	}

	// Running the closures gives the budget back, one slot per closure. A bound
	// that never recovers is worse than no bound.
	mu.Lock()
	queued := pending
	pending = nil
	mu.Unlock()
	for i, fn := range queued {
		fn()
		if n := iv.scanCount(); n != maxConcurrentScans-1-i {
			t.Fatalf("after running %d of %d completion closures scansInFlight = %d, want %d",
				i+1, len(queued), n, maxConcurrentScans-1-i)
		}
	}
	if !iv.scanImagesAsyncVia(schedule) {
		t.Fatal("a rescan was refused after every completion closure ran — the bound is a " +
			"permanent wall rather than backpressure, and the display can never be rescanned again")
	}
	waitFor(t, scheduled, "the recovery scan's completion closure to be scheduled")
}

// TestScanningStaysTrueUntilTheDisplayCallbackHasRun pins the CONTRACT the
// round-3 change created and the round-3 prose did not state.
//
// 🔴 It is a contract guard, not a regression test: it fails on either side.
// Move the slot release back into the listing goroutine and `scanning` goes
// false with the closure still queued (the round-3 defect); leave it where it is
// and `total > 0 && scanning == true` is reachable and lasts for the whole drain
// latency of the display queue — up to maxQueuedMutations × 30 s. Snapshot.Scanning
// in internal/control/viewer.go now documents exactly this, so this test is what
// stops that documentation drifting from the code again.
//
// Measured at d025210, before the docs were corrected: `snapshot with the
// completion closure QUEUED: total=3 scanning=true`, while three docstrings on
// the counter still said "in flight" and the 503 body still said "the
// bucket-listing budget is exhausted" for a refusal that can be held entirely by
// queued display work.
func TestScanningStaysTrueUntilTheDisplayCallbackHasRun(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.store = &instantStore{images: []string{"a.jpg", "b.jpg", "c.jpg"}}

	var mu sync.Mutex
	var pending []func()
	scheduled := make(chan struct{}, 8)
	schedule := func(fn func()) {
		mu.Lock()
		pending = append(pending, fn)
		mu.Unlock()
		scheduled <- struct{}{}
	}

	if !iv.scanImagesAsyncVia(schedule) {
		t.Fatal("the first scan was refused with nothing outstanding")
	}
	waitFor(t, scheduled, "the completion closure to be scheduled")

	// The listing has returned and setImages has already published its results,
	// but the display callback has not run.
	got := iv.snapshot()
	if got.total != 3 {
		t.Fatalf("total = %d with the listing finished, want 3 — this test is not in the state it "+
			"claims to be measuring", got.total)
	}
	if !got.scanning {
		t.Fatalf("scanning = false with %d display callbacks still queued. The scan slot is being "+
			"released before the work it bounds has run, which is the round-3 defect, and 40 "+
			"sequential rescans then queue 40 closures at once.", len(pending))
	}

	// ... and it is NOT stuck: running the callback ends the scan.
	mu.Lock()
	queued := pending
	pending = nil
	mu.Unlock()
	for _, fn := range queued {
		fn()
	}
	if got := iv.snapshot(); got.scanning {
		t.Errorf("scanning is still true after every display callback ran (total=%d). A flag that "+
			"never clears is not a flag, and a client keyed on it renders a spinner forever.",
			got.total)
	}
}

// TestAFailedListingReturnsItsScanSlot: the completion closure is only scheduled
// on the success path, so the error path has to return the slot itself. Four
// failed listings must not exhaust the budget permanently.
func TestAFailedListingReturnsItsScanSlot(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.store = failingStore{}

	schedule := func(fn func()) {
		t.Error("a listing that FAILED scheduled a completion closure; it has nothing to show")
	}
	for i := 0; i < maxConcurrentScans+2; i++ {
		if !iv.scanImagesAsyncVia(schedule) {
			t.Fatalf("rescan %d of %d was refused. A listing that returns an error schedules no "+
				"closure, so if the slot is only released inside that closure the budget is gone "+
				"for good after %d MinIO errors — every later POST /api/rescan answers 503 and the "+
				"display is stuck on whatever it last showed.",
				i+1, maxConcurrentScans+2, maxConcurrentScans)
		}
		waitScanCount(t, iv, 0)
		if n := iv.scanCount(); n != 0 {
			t.Fatalf("scansInFlight = %d after a failed listing, want 0", n)
		}
	}
}

type failingStore struct{}

func (failingStore) ListImages() ([]string, error) { return nil, fmt.Errorf("MinIO is unreachable") }
func (failingStore) LoadImage(key string) (*gdk.Pixbuf, error) {
	return nil, fmt.Errorf("MinIO is unreachable (%s)", key)
}

// TestScanImagesAsyncSchedulesThroughTheGTKMainLoop closes the seam the stand-in
// scheduler above opens: scanImagesAsyncVia is only correct for production if
// the production entry point hands it the REAL main-loop scheduler.
//
// `scanImagesAsyncVia(inline)`, where inline runs the closure immediately, would
// pass every behavioural test in this file and would run updateImage — a
// window.GetSize() and an image.SetFromPixbuf() — on the listing goroutine.
func TestScanImagesAsyncSchedulesThroughTheGTKMainLoop(t *testing.T) {
	got := scanSchedulerCalls(t, "main.go", "scanImagesAsync")
	if !got["idleOnce"] {
		t.Error("scanImagesAsync neither calls nor passes idleOnce. Whatever it schedules the " +
			"scan-completion closure with, it is not the program's single glib.IdleAdd call site, " +
			"so the closure — which renders — may run off the GTK main thread.")
	}
	if got["idleHigh"] {
		t.Error("scanImagesAsync schedules through idleHigh (G_PRIORITY_HIGH). That is the " +
			"shutdown priority: a scan completion would overtake every page turn already queued.")
	}
}

// TestRefusalLoggingIsCoalesced pins the counting, on a fake clock so it neither
// sleeps nor flakes.
func TestRefusalLoggingIsCoalesced(t *testing.T) {
	var r refusalLog
	base := time.Unix(1700000000, 0)

	n, since, report := r.note(base)
	if !report || n != 1 {
		t.Fatalf("first refusal reported (%d, %v), want (1, true) — the first one must always be "+
			"visible or an operator sees nothing at all", n, report)
	}
	if since != 0 {
		t.Errorf("the FIRST report claimed a %s window; there is no previous line for it to "+
			"span, and naming one invents a rate out of nothing", since)
	}
	for i := 1; i <= 5; i++ {
		if n, _, report := r.note(base.Add(time.Duration(i) * time.Second)); report {
			t.Fatalf("refusal %d inside the window reported (%d, true); it must be suppressed", i, n)
		}
	}
	// The window closes and the suppressed ones are accounted for, not lost.
	n, since, report = r.note(base.Add(refusalLogInterval))
	if !report {
		t.Fatal("no refusal was reported after the window closed — refusals are now invisible " +
			"forever, which is not coalescing, it is silence")
	}
	if n != 6 {
		t.Errorf("the report after the window named %d refusals, want 6 (the 5 suppressed plus "+
			"this one). A count that does not carry the suppressed ones hides the magnitude, "+
			"which is the only thing the line is for.", n)
	}
	if since != refusalLogInterval {
		t.Errorf("the second report spanned %s, want %s", since, refusalLogInterval)
	}
	if n, _, report := r.note(base.Add(refusalLogInterval + time.Second)); report {
		t.Errorf("the refusal right after a report was itself reported (%d) — the window did not "+
			"restart", n)
	}

	// 🔴 The nit this replaced: the span is the ACTUAL silence, not
	// refusalLogInterval. A run that goes quiet for an hour and then refuses once
	// must not be printed as "in the last 1m0s" — that overstates the rate 60x,
	// and it is the operator's only signal for how hard the API is being driven.
	// Two points, a boundary and a far one, because a constant satisfies either
	// alone: this is exactly the fixture-equals-the-constant trap.
	//
	// The gap is deliberately 1h0m7s rather than a whole hour: a duration that is
	// a round multiple of the interval cannot distinguish "the measured gap" from
	// "some arithmetic on refusalLogInterval".
	quiet := base.Add(refusalLogInterval + time.Hour + 7*time.Second)
	n, since, report = r.note(quiet)
	if !report || n != 2 {
		t.Fatalf("the refusal after an hour of silence reported (%d, %v), want (2, true)", n, report)
	}
	if want := time.Hour + 7*time.Second; since != want {
		t.Errorf("the report after an hour of near-silence spanned %s, want %s. Printing the "+
			"coalescing interval instead of the measured gap tells the operator a burst is "+
			"happening when one refusal happened.", since, want)
	}
	if got := refusalSpan(since); got != "in the last 1h0m7s" {
		t.Errorf("refusalSpan(%s) = %q, want %q", since, got, "in the last 1h0m7s")
	}
	if got := refusalSpan(0); got == "in the last 0s" || strings.Contains(got, "1m0s") {
		t.Errorf("refusalSpan(0) = %q — the first line of a run spans no window, so it must "+
			"neither name a duration of zero nor the coalescing interval", got)
	}
}

// TestRefusedRescansDoNotWriteALinePerRefusal is the behavioural half: this is
// the DoS-adjacent path, and round 2 wrote one journald line per refusal.
//
// 🔴 496 lines from a single run of TestConcurrentRescansAreBounded, on a
// Raspberry Pi whose journal is on the SD card.
func TestRefusedRescansDoNotWriteALinePerRefusal(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.store = &instantStore{images: []string{"a.jpg"}}
	g := gtkViewer{iv: iv}
	t.Cleanup(func() { drainMainContext(t) })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	const attempts = 500
	refused := 0
	for i := 0; i < attempts; i++ {
		if !g.Rescan() {
			refused++
		}
	}
	// Positive control on the INPUT: this test can only speak about log volume if
	// it actually drove a lot of refusals. It is deliberately NOT expressed as
	// attempts-maxConcurrentScans — that would couple it to WHEN the scan slot is
	// returned, which is a different guard's job, and it would then fail here for
	// that other reason instead.
	if refused < 100 {
		t.Fatalf("only %d of %d rescans were refused; this test cannot observe the log volume of "+
			"a refusal path it never drove", refused, attempts)
	}

	lines := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "rescan refused") {
			lines++
		}
	}
	// Positive control: the first refusal MUST be logged, or a zero here would
	// mean "the logger is wired to nothing" rather than "it is coalescing".
	if lines == 0 {
		t.Fatalf("%d refusals produced no log line at all. Backpressure that leaves no trace is "+
			"indistinguishable from a display nobody is talking to.", refused)
	}
	if lines > 1 {
		t.Errorf("%d refusals produced %d log lines. Refusals are caused by the CALLER, so one "+
			"line each lets an authenticated client that merely retries convert its own "+
			"backpressure into unbounded journald volume on a Pi.", refused, lines)
	}
}

// TestRefusedMutationsDoNotWriteALinePerRefusal is the other half of the pair.
//
// 🔴 enqueueBounded's comment claims its refusal line is "symmetric with the
// scan-refusal line"; a claim in a comment is a claim, and this is what makes it
// checkable. Round 2 was asymmetric the other way — one line per refused rescan
// and nothing at all here.
func TestRefusedMutationsDoNotWriteALinePerRefusal(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg")
	schedule := func(fn func()) {} // never runs anything: the queue stays full

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	const attempts = 500
	refused := 0
	for i := 0; i < attempts; i++ {
		if !iv.enqueueBounded(schedule, func() {}) {
			refused++
		}
	}
	if refused < 100 {
		t.Fatalf("only %d of %d enqueues were refused; this test cannot observe the log volume of "+
			"a refusal path it never drove", refused, attempts)
	}

	lines := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "mutation refused") {
			lines++
		}
	}
	if lines == 0 {
		t.Fatalf("%d refused mutations produced no log line at all, while a refused RESCAN "+
			"produces one. That is the asymmetry the comment on enqueueBounded says is gone: an "+
			"operator watching the journal sees the display refusing rescans and never sees it "+
			"refusing page turns.", refused)
	}
	if lines > 1 {
		t.Errorf("%d refused mutations produced %d log lines; the refusal log must coalesce here "+
			"for the same reason it does for rescans", refused, lines)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ---------------------------------------------------------------------------
// F6 — the GTK work queue is bounded, and it recovers
// ---------------------------------------------------------------------------

// TestEnqueueIsBounded drives the adapter's real accounting through a stand-in
// for the GTK main loop, so BOTH the cap and the release are exercised.
func TestEnqueueIsBounded(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg")

	var mu sync.Mutex
	var pending []func()
	schedule := func(fn func()) { mu.Lock(); pending = append(pending, fn); mu.Unlock() }
	drain := func() {
		mu.Lock()
		batch := pending
		pending = nil
		mu.Unlock()
		for _, fn := range batch {
			fn()
		}
	}

	ran := 0
	for i := 0; i < maxQueuedMutations; i++ {
		if !iv.enqueueBounded(schedule, func() { ran++ }) {
			t.Fatalf("enqueue %d of %d was refused; the cap is lower than maxQueuedMutations",
				i+1, maxQueuedMutations)
		}
	}
	if got := iv.queueDepth(); got != maxQueuedMutations {
		t.Fatalf("queueDepth = %d, want %d", got, maxQueuedMutations)
	}
	if iv.enqueueBounded(schedule, func() { ran++ }) {
		t.Fatalf("the %dth enqueue was ACCEPTED. The GTK work queue is unbounded: it drains at "+
			"up to 30s per image while an authenticated caller can POST as fast as the Pi "+
			"accepts connections, and every accepted closure holds a gotk3 callback-registry "+
			"entry until it runs.", maxQueuedMutations+1)
	}
	if got := len(pending); got != maxQueuedMutations {
		t.Fatalf("%d closures were scheduled, want %d — the refused one was scheduled anyway",
			got, maxQueuedMutations)
	}

	// Draining the loop must give the slots back, or the cap is a permanent
	// wall rather than backpressure.
	drain()
	if ran != maxQueuedMutations {
		t.Fatalf("%d closures ran, want %d", ran, maxQueuedMutations)
	}
	if got := iv.queueDepth(); got != 0 {
		t.Fatalf("queueDepth = %d after the loop drained, want 0 — a slot is leaked per "+
			"request and the API bricks itself after %d calls", got, maxQueuedMutations)
	}
	if !iv.enqueueBounded(schedule, func() { ran++ }) {
		t.Fatal("enqueue was still refused after the loop drained")
	}
}

// TestAdapterEnqueueReportsSuccess: the gtkViewer path (which schedules through
// glib) must report true and account for the slot.
func TestAdapterEnqueueReportsSuccess(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg")
	g := gtkViewer{iv: iv}
	if !g.Enqueue(func() {}) {
		t.Fatal("the adapter refused the first enqueue on an empty queue")
	}
	if got := iv.queueDepth(); got != 1 {
		t.Fatalf("queueDepth = %d after one Enqueue, want 1 — the adapter is not going "+
			"through the bounded path", got)
	}
	// Run the GTK main context so the scheduled closure completes and the slot
	// comes back, proving the adapter wired the release into the real path.
	ctx := glib.MainContextDefault()
	deadline := time.Now().Add(3 * time.Second)
	for iv.queueDepth() != 0 && time.Now().Before(deadline) {
		ctx.Iteration(false)
	}
	if got := iv.queueDepth(); got != 0 {
		t.Fatalf("queueDepth = %d after the main loop ran the closure, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// F16 — /healthz reports something real
// ---------------------------------------------------------------------------

// TestResolveVersion pins the fallback chain. The constant it replaces was
// hardcoded "0.2.0" with nothing in the build bumping it, so /healthz would have
// gone on reporting 0.2.0 for every later deploy.
func TestResolveVersion(t *testing.T) {
	rev := "abcdef0123456789abcdef0123456789abcdef01"
	cases := []struct {
		name     string
		injected string
		settings []debug.BuildSetting
		want     string
	}{
		{"link-time value wins", "1.4.7", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}}, "1.4.7"},
		{"clean checkout", "", []debug.BuildSetting{
			{Key: "vcs.revision", Value: rev},
			{Key: "vcs.modified", Value: "false"},
		}, "abcdef012345"},
		{"dirty checkout", "", []debug.BuildSetting{
			{Key: "vcs.revision", Value: rev},
			{Key: "vcs.modified", Value: "true"},
		}, "abcdef012345-dirty"},
		{"short revision is not truncated", "", []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
		}, "abc123"},
		{"no stamp at all", "", nil, "unknown"},
		{"stamp without a revision", "", []debug.BuildSetting{{Key: "GOARCH", Value: "arm64"}}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.injected, tc.settings); got != tc.want {
				t.Fatalf("resolveVersion(%q, %v) = %q, want %q", tc.injected, tc.settings, got, tc.want)
			}
		})
	}
}

// TestVersionIsNotEmpty: whatever the build was, /healthz must report SOMETHING.
//
// 🔴 Note what this canNOT see, so nobody reads it as more: "unknown" is not
// empty, so this passes for a build that identifies itself as nothing in
// particular. That is not hypothetical — `nix build .#default` produced exactly
// that until flake.nix started injecting main.injectedVersion. A guard for the
// nix path has to look at flake.nix, which is what
// TestTheNixPackageInjectsAVersion below does; `go test` alone cannot observe
// how a different build system links the binary.
func TestVersionIsNotEmpty(t *testing.T) {
	if strings.TrimSpace(version) == "" {
		t.Fatal("version is empty — GET /healthz would report no version at all")
	}
}

// TestTheNixPackageInjectsAVersion pins the half TestVersionIsNotEmpty is blind
// to: the repo's own packaged build must link a value into injectedVersion.
//
// Without it, buildGoModule builds from a /nix/store copy with no .git,
// -buildvcs=auto stamps nothing, resolveVersion falls through to "unknown", and
// every nix-built Pi reports the same non-answer on /healthz. This is a textual
// check on flake.nix — the alternative is running `nix build` from a Go test,
// which is neither hermetic nor fast — so it is paired with the negative control
// below rather than trusted on its own.
func TestTheNixPackageInjectsAVersion(t *testing.T) {
	src, err := os.ReadFile("flake.nix")
	if err != nil {
		t.Fatalf("reading flake.nix: %v", err)
	}
	if !nixInjectsVersion(string(src)) {
		t.Errorf("flake.nix does not link a value into main.injectedVersion. buildGoModule builds "+
			"from a /nix/store copy with no .git, so -buildvcs stamps nothing and resolveVersion "+
			"answers %q for every nix-built binary — and TestVersionIsNotEmpty passes on that.",
			resolveVersion("", nil))
	}
	// The property the injection exists for, stated as a claim about the code
	// rather than about the build: with nothing injected and no VCS stamp,
	// resolveVersion really does produce "unknown".
	if got := resolveVersion("", nil); got != "unknown" {
		t.Fatalf("resolveVersion(\"\", nil) = %q, want \"unknown\" — the failure mode this "+
			"guard describes is not the one the code has", got)
	}
}

// TestNixVersionDetectorCanFire is the positive control: fed a flake that does
// NOT inject, the detector must say so, and fed one that does, it must not.
func TestNixVersionDetectorCanFire(t *testing.T) {
	const without = `{
  outputs = { self, nixpkgs }: {
    packages.default = pkgs.buildGoModule {
      pname = "comic-flex";
      vendorHash = null;
      env.CGO_ENABLED = 1;
    };
  };
}`
	if nixInjectsVersion(without) {
		t.Error("the detector claims a flake with no ldflags injects a version — it cannot " +
			"observe the hazard, so its verdict on the real flake.nix means nothing")
	}
	const with = `{
  outputs = { self, nixpkgs }: {
    packages.default = pkgs.buildGoModule {
      ldflags = [ "-X main.injectedVersion=${revision}" ];
    };
  };
}`
	if !nixInjectsVersion(with) {
		t.Error("the detector did not recognise an ldflags injection it should accept")
	}
	// And a near-miss: injecting some OTHER symbol must not count.
	const wrongSymbol = `ldflags = [ "-X main.buildDate=${revision}" ];`
	if nixInjectsVersion(wrongSymbol) {
		t.Error("the detector accepted an ldflags line that sets a different symbol — it is " +
			"matching on \"ldflags\" rather than on what gets injected")
	}
}

// nixInjectsVersion reports whether src links a value into main.injectedVersion.
func nixInjectsVersion(src string) bool {
	return strings.Contains(src, "-X main.injectedVersion=")
}
