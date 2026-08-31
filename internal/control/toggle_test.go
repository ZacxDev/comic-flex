package control

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// POST /api/toggle — the endpoint that exists because a read followed by a
// separate write is not atomic.
//
// The rows in authedRoutes, the endpoint table and enqueuedMutationPaths already
// drive the generic contracts (401 without the token, 202 with it, 503 on a full
// queue, JSON content type, method discipline). What is HERE is everything
// specific to this endpoint: which way the flip goes, that the flip is decided
// inside the closure rather than on the handler goroutine, and what it does
// while a scan is outstanding.

// TestToggleRequiresTheTokenAndAnswers202WithIt.
//
// 🔴 THE POSITIVE CONTROL IS THE POINT, and without it this test is vacuous.
// requireToken wraps the WHOLE /api/ subtree, so an unauthenticated POST to a
// route that does not exist at all also answers 401 — TestUnknownAPIRouteIs401
// WhenUnauthenticated asserts exactly that. A test that only checked the 401s
// would therefore have passed against the code BEFORE this endpoint existed,
// proving nothing about it. The authenticated leg is what ties the 401s to a
// route that is really there.
func TestToggleRequiresTheTokenAndAnswers202WithIt(t *testing.T) {
	for _, hdr := range []struct{ name, value string }{
		{"no Authorization header", ""},
		{"empty bearer", "Bearer "},
		{"wrong token of the right length", "Bearer " + strings.Repeat("x", len(testToken))},
		{"right token, one byte truncated", "Bearer " + testToken[:len(testToken)-1]},
		{"right token, one byte appended", "Bearer " + testToken + "z"},
		{"right token, no scheme", testToken},
		{"wrong scheme", "Basic " + testToken},
	} {
		t.Run(hdr.name, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			s := newTestServer(t, f)
			w := doWithAuth(t, s, "POST", "/api/toggle", "", hdr.value)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("POST /api/toggle with %s -> %d, want 401. The Pi has passwordless "+
					"sudo and this port has no firewall in front of it.", hdr.name, w.Code)
			}
			if n := f.pendingCount(); n != 0 {
				t.Fatalf("an unauthenticated toggle enqueued %d closures", n)
			}
			if got := f.callLog(); len(got) != 0 {
				t.Fatalf("an unauthenticated toggle reached the viewer: %v", got)
			}
			if f.pausedFlag() {
				t.Fatal("an unauthenticated toggle changed the paused flag")
			}
		})
	}

	t.Run("with the right token", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		w := do(t, s, "POST", "/api/toggle", "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("POST /api/toggle with the right token -> %d, want 202 (body %s). Without "+
				"this leg the 401 assertions above pass on a route that does not exist.",
				w.Code, w.Body.String())
		}
	})
}

// TestToggleFlipsBothWays is the behaviour the PWA cannot get right on its own:
// one endpoint, and it lands on the OTHER state whichever one it started in.
//
// The two directions are separate cases on purpose. A mutant that always paused
// (`SetPaused(true)`) satisfies the playing -> paused half and fails the other,
// and a mutant that always resumed fails the first — neither survives both.
func TestToggleFlipsBothWays(t *testing.T) {
	for _, tc := range []struct {
		name     string
		start    bool
		wantCall string
		wantEnd  bool
	}{
		{"playing becomes paused", false, "TogglePaused:true", true},
		{"paused becomes playing", true, "TogglePaused:false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			f.setPaused(tc.start)
			s := newTestServer(t, f)

			w := do(t, s, "POST", "/api/toggle", "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status %d, want 202 (body %s)", w.Code, w.Body.String())
			}
			f.drain()

			if got := f.callLog(); !reflect.DeepEqual(got, []string{tc.wantCall}) {
				t.Fatalf("calls = %v, want [%s]", got, tc.wantCall)
			}
			if got := f.pausedFlag(); got != tc.wantEnd {
				t.Fatalf("starting paused=%v, a toggle landed on paused=%v, want %v",
					tc.start, got, tc.wantEnd)
			}
		})
	}

	// And it is a TOGGLE, not a latch: two of them return to where they started.
	// A mutant that flipped once and then wrote an absolute value passes both
	// cases above and fails here.
	t.Run("two toggles return to the start", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		for i := 0; i < 2; i++ {
			if w := do(t, s, "POST", "/api/toggle", ""); w.Code != http.StatusAccepted {
				t.Fatalf("toggle %d -> %d, want 202", i+1, w.Code)
			}
			f.drain()
		}
		if f.pausedFlag() {
			t.Fatal("two toggles left the slideshow paused — the second did not flip")
		}
		want := []string{"TogglePaused:true", "TogglePaused:false"}
		if got := f.callLog(); !reflect.DeepEqual(got, want) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	})
}

// TestToggleReadsAndFlipsInsideTheClosure is the atomicity guard, and it is the
// reason this endpoint exists at all.
//
// It is the same shape as TestGotoResolvesInsideTheClosure: the state moves
// between the handler answering 202 and the closure running, and the assertion
// is that the closure acted on the state AS IT IS WHEN IT RUNS.
//
// 🔴 What the mutant looks like. Any handler of the form
//
//	paused := s.viewer.Snapshot().Paused
//	s.enqueue(w, func() { s.viewer.SetPaused(!paused) })
//
// reads false here, so its closure writes SetPaused(true) — and the flag is
// already true by then, so the toggle is a no-op and the user's tap does
// nothing. The correct implementation reads true inside the closure and lands on
// false. The two answers are different values of the same field, checked below.
//
// The readLog assertion is the structural half of the same property: a handler
// that decided the direction here would have had to call Snapshot to do it, and
// there is nothing else on this path that reads.
func TestToggleReadsAndFlipsInsideTheClosure(t *testing.T) {
	f := newFakeViewer("a/1.jpg") // starts playing
	s := newTestServer(t, f)

	w := do(t, s, "POST", "/api/toggle", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 (body %s)", w.Code, w.Body.String())
	}

	// The handler decided nothing and read nothing.
	if got := f.readLog(); len(got) != 0 {
		t.Fatalf("the toggle handler made R2 reads %v. The direction of the flip must be "+
			"decided inside the closure, under one lock acquisition — a read here is the "+
			"read-then-write this endpoint replaces, and it is stale by the time the closure "+
			"runs.", got)
	}
	if got := f.callLog(); len(got) != 0 {
		t.Fatalf("work ran before the handler returned: %v — the handler awaited the mutation", got)
	}
	if n := f.pendingCount(); n != 1 {
		t.Fatalf("pending closures = %d, want exactly 1 enqueued", n)
	}

	// The `p` keypress on the Pi pauses the slideshow while the toggle is still
	// sitting on the GTK loop's queue behind a slow page turn.
	f.setPaused(true)

	f.drain()

	if got := f.pausedFlag(); got != false {
		t.Fatalf("the paused flag moved to true after the 202 and the toggle landed on "+
			"paused=%v, want false. The flip was computed from a state read on the handler "+
			"goroutine, which is exactly the stale read POST /api/toggle exists to eliminate.",
			got)
	}
	if got := f.callLog(); !reflect.DeepEqual(got, []string{"TogglePaused:false"}) {
		t.Fatalf("calls = %v, want [TogglePaused:false] — a handler that captured the direction "+
			"would have enqueued SetPaused with an absolute value instead", got)
	}
}

// TestToggleWhileScanningFlipsAndLeavesScanning pins the tri-state decision
// handleToggle documents: playing / paused / scanning are not three values of
// one field, and a toggle during a scan PROCEEDS.
//
// Both halves are asserted, because either one alone permits the wrong
// behaviour:
//
//   - the flip still happens (a 409/503-while-scanning implementation fails
//     here), and
//   - Scanning is untouched by it (an implementation that "resolved" the
//     tri-state by clearing or setting the scan state fails here).
//
// The scanning-with-a-populated-gallery row is not padding: Snapshot.Scanning
// stays true until the completion closure has RUN, so `{"total":3,
// "scanning":true}` is the ordinary state after every rescan, and an endpoint
// that refused there would be refusing most of the time.
func TestToggleWhileScanningFlipsAndLeavesScanning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		total    int
		scanning bool
		start    bool
		wantCall string
		wantEnd  bool
	}{
		{"indexing, nothing displayed yet", 0, true, false, "TogglePaused:true", true},
		{"rescan not yet displayed", 3, true, false, "TogglePaused:true", true},
		{"rescan not yet displayed, already paused", 3, true, true, "TogglePaused:false", false},
		{"not scanning", 3, false, false, "TogglePaused:true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg", "b/2.jpg", "c/3.jpg")
			f.snap.Total = tc.total
			f.snap.Scanning = tc.scanning
			f.setPaused(tc.start)
			s := newTestServer(t, f)

			w := do(t, s, "POST", "/api/toggle", "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("POST /api/toggle while scanning=%v -> %d, want 202. A toggle during a "+
					"scan proceeds: paused and scanning are orthogonal, a scan is outstanding at "+
					"boot and after every rescan, and refusing would have to be decided from the "+
					"stale read this endpoint exists to eliminate. (body %s)",
					tc.scanning, w.Code, w.Body.String())
			}
			f.drain()

			if got := f.callLog(); !reflect.DeepEqual(got, []string{tc.wantCall}) {
				t.Fatalf("calls = %v, want [%s]", got, tc.wantCall)
			}
			if got := f.pausedFlag(); got != tc.wantEnd {
				t.Fatalf("paused = %v, want %v", got, tc.wantEnd)
			}

			// The third state is still there afterwards, and still readable. A UI
			// that renders playing / paused / scanning has not lost an axis.
			var got Snapshot
			r := do(t, s, "GET", "/api/state", "")
			if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
				t.Fatalf("state body is not JSON: %v", err)
			}
			if got.Scanning != tc.scanning {
				t.Fatalf("after the toggle, scanning = %v, want %v — the toggle collapsed the "+
					"three UI states into two", got.Scanning, tc.scanning)
			}
			if got.Total != tc.total {
				t.Fatalf("after the toggle, total = %d, want %d", got.Total, tc.total)
			}
			if got.Paused != tc.wantEnd {
				t.Fatalf("GET /api/state reports paused = %v after the toggle landed on %v",
					got.Paused, tc.wantEnd)
			}
		})
	}
}

// TestToggleResponseShape pins what the caller is told, byte for byte.
//
// 🔴 It asserts the WHOLE decoded body, not the presence of a field. The
// resulting state is NOT in it and must not be: the flip has not happened when
// this is written, so any `"paused"` here would be a prediction made from a read
// taken before the closure ran — the same read TestToggleReadsAndFlipsInsideThe
// Closure proves can be wrong. A caller that needs the landed state polls
// GET /api/state, which is a synchronous R2 read and is not subject to the queue
// cap. Pinning the whole map is what makes adding such a field a deliberate,
// visible decision rather than a convenience someone slips in.
func TestToggleResponseShape(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	s := newTestServer(t, f)

	w := do(t, s, "POST", "/api/toggle", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
	}
	want := map[string]any{"accepted": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("toggle body = %#v, want %#v. A field describing the resulting state cannot be "+
			"honest here — the flip runs after this response is written.", got, want)
	}
}

// TestToggleDoesNotAwaitABlockedGTKLoop: the never-await rule, driven against a
// loop that is genuinely wedged.
//
// It is the toggle-specific twin of TestMutationHandlerDoesNotAwaitABlockedGTK
// Loop, and it exists separately because this is the one endpoint where a
// maintainer has a REASON to want to wait — the resulting state is only knowable
// after the closure runs. Waiting for it would hang for as long as the loop
// takes to drain, which is up to 30 s per queued image on an S3 GET.
func TestToggleDoesNotAwaitABlockedGTKLoop(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	block := make(chan struct{})
	defer close(block)
	f.enqueueRunner = func(fn func()) {
		go func() {
			<-block // the GTK thread never gets around to it
			fn()
		}()
	}
	s := newTestServer(t, f)

	done := make(chan int, 1)
	go func() {
		done <- do(t, s, "POST", "/api/toggle", "").Code
	}()

	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Fatalf("status %d, want 202", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the toggle handler did not return while the GTK loop was blocked — it waited " +
			"for the flip so it could report the resulting state, which is exactly the trade " +
			"this endpoint must not make")
	}
}
