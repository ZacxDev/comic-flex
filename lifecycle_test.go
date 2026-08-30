package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
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
// the round-1 handler read the channel exactly ONCE and returned — so a second
// SIGTERM sat in the buffer forever and the process became unsignallable.
// `systemctl stop` then waits out TimeoutStopSec (90 s) and SIGKILLs, on a unit
// the deploy notes ask to run with Restart=always. Deleting the whole block was
// measured at 200 PASS / 0 FAIL.
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
			"so the process cannot be signalled dead: systemctl stop waits out TimeoutStopSec " +
			"(90 s) and then SIGKILLs, on every stop and every restart.")
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
}

// scanSchedulerCalls reports which scheduling functions the named function calls.
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
	store.release <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for iv.scanCount() > 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
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
	deadline = time.Now().Add(3 * time.Second)
	for iv.isScanning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if iv.isScanning() {
		t.Fatal("both listings finished and scanning is still true — the counter never " +
			"returns to zero, so a client renders 'indexing…' forever")
	}
	if n := iv.scanCount(); n != 0 {
		t.Fatalf("scansInFlight = %d after both listings finished, want 0", n)
	}
}

// TestEndScanDoesNotGoNegative: an unbalanced endScan must not make a later
// beginScan look like nothing is happening.
func TestEndScanDoesNotGoNegative(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.endScan()
	iv.endScan()
	if n := iv.scanCount(); n != 0 {
		t.Fatalf("scansInFlight = %d after two unbalanced endScans, want 0", n)
	}
	iv.beginScan()
	if !iv.isScanning() {
		t.Fatal("beginScan after unbalanced endScans did not report scanning — the counter " +
			"went negative and a real scan is now invisible")
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
func TestVersionIsNotEmpty(t *testing.T) {
	if strings.TrimSpace(version) == "" {
		t.Fatal("version is empty — GET /healthz would report no version at all")
	}
}
