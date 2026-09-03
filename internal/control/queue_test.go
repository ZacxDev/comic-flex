package control

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// These cover POST /api/queue and the `queue` object GET /api/state carries.
//
// The BEHAVIOUR behind the queue — the skip rule (D3), the drain-to-the-
// interrupted-position rule (D4), the replace rule (D7) — cannot be pinned from
// this package: it has no gallery and no cursor. It is pinned in package main by
// state_queue_test.go. What this package can and must pin is the wire: the
// bounds, the refusals, the fact that nothing is resolved on the handler
// goroutine, and that the state object always carries a queue.

// queueBody renders a body holding n distinct keys, each shaped like the real
// bucket's (a directory, a title with spaces, a page file).
func queueBody(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = `"CGC Comics 2022/Absolute Batman/img` + strconv.Itoa(i) + `.jpg"`
	}
	return `{"keys":[` + strings.Join(keys, ",") + `]}`
}

// TestQueueIsAlwaysPresentInTheStateObject is the absent-vs-empty guard, and it
// is the one thing a consumer cannot code around after the fact.
//
// 🔴 The version signal for this whole feature is the PRESENCE of the `queue`
// key. A Pi that predates the play queue omits it; a Pi that has one emits it
// even when nothing is queued. If an empty queue could also make the key vanish,
// the two states collapse and a client can never tell "this Pi cannot play
// collections" from "this Pi is idle" — the same collapse that would have made an
// absent seconds_until_next read as a stopped slideshow forever.
//
// It reads the RAW BYTES rather than an unmarshalled map, because an absent key
// and a present zero-valued object are indistinguishable once a map has swallowed
// them, and absent-vs-present is the entire property.
func TestQueueIsAlwaysPresentInTheStateObject(t *testing.T) {
	f := newFakeViewer()
	// Deliberately the ZERO value: no queue running, nothing ever queued. This is
	// the state the Pi is in at every boot, and it is the state that must still
	// carry the object.
	f.snap = Snapshot{ViewMode: string(ViewLandscapeSingle), SlideInterval: 30}
	s := newTestServer(t, f)

	body := do(t, s, "GET", "/api/state", "").Body.String()
	for _, want := range []string{`"queue":{`, `"id":0`, `"length":0`, `"position":0`, `"skipped":0`,
		// boot_id rides the same rule: present on a Pi that has it, absent on one
		// that predates it, so a client can branch on presence. It is empty here
		// because the fake viewer is not the process — package main's adapter is
		// what fills it, and TestBootIDIsStableAndNonEmpty pins that end.
		`"boot_id":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("state body does not contain %s — a client cannot tell a Pi with no queue "+
				"from a Pi that predates queues at all, and those are opposite facts.\nbody: %s",
				want, body)
		}
	}
}

// TestQueueStateIsPassedThroughUnchanged drives every field of QueueState
// through the real handler — count the rows of `want` below rather than trusting
// a number in this sentence, which said "the three numbers" for two rounds after
// `id` made it four. They are pairwise distinct and distinct from every other
// numeric field in the snapshot, so a mutant crossing two of them cannot pass.
func TestQueueStateIsPassedThroughUnchanged(t *testing.T) {
	f := newFakeViewer()
	f.snap = Snapshot{
		Total: 88, Index: 41, ViewMode: string(ViewLandscapeSingle), SlideInterval: 45,
		Queue: QueueState{ID: 7, Length: 12, Position: 5, Skipped: 3},
	}
	s := newTestServer(t, f)

	var got map[string]any
	if err := json.Unmarshal(do(t, s, "GET", "/api/state", "").Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	want := map[string]any{
		"id": float64(7), "length": float64(12), "position": float64(5), "skipped": float64(3),
	}
	if !reflect.DeepEqual(got["queue"], want) {
		t.Fatalf("queue = %#v, want %#v", got["queue"], want)
	}
}

// TestAnOverLongQueueIsRefused pins maxQueueKeys.
//
// The two lengths straddle the cap by ONE on each side, because the failure this
// guards is a bound that admits one too many, and a fixture 100 either side of it
// cannot see that. The at-the-cap case is the positive control: without it, a
// handler that refused every queue would pass the over-cap half.
func TestAnOverLongQueueIsRefused(t *testing.T) {
	t.Run("exactly at the cap is accepted", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		w := do(t, s, "POST", "/api/queue", queueBody(maxQueueKeys))
		if w.Code != http.StatusAccepted {
			t.Fatalf("a queue of exactly maxQueueKeys (%d) -> %d, want 202 (body %s). The cap is "+
				"refusing lists it should accept, so the over-cap case below would pass for the "+
				"wrong reason.", maxQueueKeys, w.Code, w.Body.String())
		}
		if n := f.pendingCount(); n != 1 {
			t.Fatalf("pending closures = %d, want 1", n)
		}
	})

	t.Run("one over the cap is refused", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		w := do(t, s, "POST", "/api/queue", queueBody(maxQueueKeys+1))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("a queue of maxQueueKeys+1 (%d) -> %d, want 400. An unbounded list from a "+
				"network client is the hazard the admission caps exist for, arriving as one "+
				"request. (body %s)", maxQueueKeys+1, w.Code, w.Body.String())
		}
		var body errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("400 body is not JSON: %v (%s)", err, w.Body.String())
		}
		if !strings.Contains(body.Error, strconv.Itoa(maxQueueKeys)) {
			t.Errorf("400 message %q does not name the cap (%d) — the caller is refused with no "+
				"idea what would be accepted", body.Error, maxQueueKeys)
		}
		if n := f.pendingCount(); n != 0 {
			t.Fatalf("an over-long queue enqueued %d closures, want 0", n)
		}
		if got := f.callLog(); len(got) != 0 {
			t.Fatalf("an over-long queue reached the viewer: %v", got)
		}
	})
}

// TestTheQueueHasItsOwnBodyCap pins that POST /api/queue is NOT held to
// maxBodyBytes.
//
// 🔴 It is the guard that keeps the feature from being silently useless. Every
// other body on this API is a scalar and fits in a handful of bytes; a queue is a
// list of object keys that run 40-plus bytes each, so the 4 KiB scalar cap admits
// about eighty of them. A maxQueueKeys of 500 behind a 4 KiB body reader is a cap
// that can never be reached — the endpoint would 413 on any real collection while
// every unit test written with three-key fixtures passed.
//
// Both directions, because the pair is what makes either meaningful: a body over
// the SCALAR cap is accepted here, and a body over the QUEUE cap is still 413.
func TestTheQueueHasItsOwnBodyCap(t *testing.T) {
	if maxQueueBodyBytes <= maxBodyBytes {
		t.Fatalf("maxQueueBodyBytes (%d) is not above maxBodyBytes (%d); this test's premise is gone",
			maxQueueBodyBytes, maxBodyBytes)
	}

	t.Run("a body over the scalar cap is accepted", func(t *testing.T) {
		// 200 realistic keys is comfortably past 4 KiB and comfortably inside
		// 128 KiB — and 200 is not maxQueueKeys, so a handler that confused the
		// two bounds fails here rather than landing on the boundary.
		body := queueBody(200)
		if len(body) <= maxBodyBytes {
			t.Fatalf("the 200-key fixture is %d bytes, which no longer exceeds maxBodyBytes (%d) — "+
				"this test would pass without the queue having its own cap at all",
				len(body), maxBodyBytes)
		}
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		if w := do(t, s, "POST", "/api/queue", body); w.Code != http.StatusAccepted {
			t.Fatalf("a %d-byte queue body -> %d, want 202. POST /api/queue is being held to the "+
				"SCALAR body cap, which no real collection fits inside. (body %s)",
				len(body), w.Code, w.Body.String())
		}
	})

	t.Run("a body over the queue cap is still refused", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		// One long key, so the refusal is the BODY reader's and not the length
		// cap's: a single element can never trip maxQueueKeys.
		body := `{"keys":["` + strings.Repeat("k", maxQueueBodyBytes+1024) + `"]}`
		w := do(t, s, "POST", "/api/queue", body)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a %d-byte queue body -> %d, want 413. Without the MaxBytesReader the decoder "+
				"buffers the whole body on a Raspberry Pi before any validation runs. (body %s)",
				len(body), w.Code, w.Body.String())
		}
		if n := f.pendingCount(); n != 0 {
			t.Fatalf("an over-sized body enqueued %d closures, want 0", n)
		}
	})
}

// TestQueueResolvesNoKeyOnTheHandlerGoroutine pins the deliberate ASYMMETRY with
// POST /api/goto, which is the decision most likely to be "fixed" by someone
// adding a 404 for symmetry.
//
// goto names ONE page and can tell the caller it is missing. A queue names many,
// and decision D3 says a key that has left the gallery is skipped and COUNTED at
// play time rather than refused up front — so a per-key resolution here would be
// a lookup whose answer is thrown away, taken against a gallery a rescan can
// replace before the closure runs. A queue of keys that are all missing is a
// perfectly good 202.
//
// 🔴 The assertion is the EXACT read log, not "no reads". The handler does make
// one R2 read — the indexing admission check below — and asserting emptiness
// would have made that check impossible to add without deleting a guard. What
// must never appear is a Resolve: one read that does not depend on the keys is a
// different thing from len(keys) lookups whose results are discarded.
func TestQueueResolvesNoKeyOnTheHandlerGoroutine(t *testing.T) {
	f := newFakeViewer("a/1.jpg", "b/2.jpg")
	s := newTestServer(t, f)

	w := do(t, s, "POST", "/api/queue", `{"keys":["gone/1.jpg","gone/2.jpg"]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("a queue of keys not in the gallery -> %d, want 202. Decision D3 makes a missing "+
			"key a SKIP at play time, not a refusal here. (body %s)", w.Code, w.Body.String())
	}
	if got, want := f.readLog(), []string{"Snapshot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reads = %v, want exactly %v. A Resolve here is a per-key lookup whose answer is "+
			"thrown away and is stale by the time the closure runs; the authoritative resolution "+
			"happens inside the closure, under the lock.", got, want)
	}
	f.drain()
	if got, want := f.callLog(), []string{"SetQueue:gone/1.jpg,gone/2.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// TestAQueuePostedWhileIndexingIsRefusedAsBackpressure is finding F3.
//
// `scanning && total == 0` is the Pi's state at every boot until the first
// listing returns. A queue installed then is consumed in full against an empty
// gallery, every key is skipped, the display never moves — and the caller was
// told 202. The refusal makes it observable AND retryable, and it reuses the
// backpressure convention rather than inventing a second one, because retrying
// really is the correct response: the scan is seconds away.
//
// Both directions, and the second is what stops the first passing for the wrong
// reason: an empty gallery that has FINISHED scanning still answers 202, because
// retrying that would never help and the honest report is the skip count.
func TestAQueuePostedWhileIndexingIsRefusedAsBackpressure(t *testing.T) {
	t.Run("indexing is refused", func(t *testing.T) {
		f := newFakeViewer()
		f.snap = Snapshot{Total: 0, Scanning: true,
			ViewMode: string(ViewLandscapeSingle), SlideInterval: 30}
		s := newTestServer(t, f)

		w := do(t, s, "POST", "/api/queue", `{"keys":["a/1.jpg","b/2.jpg"]}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("a queue posted while the gallery is still indexing -> %d, want 503. Every "+
				"key would be skipped against an empty gallery and the caller would be told 202 "+
				"for a collection that never played. (body %s)", w.Code, w.Body.String())
		}
		// 🔴 The VALUE, not merely its presence. The queue-full default of 1 s is
		// calibrated for a bound that clears as the GTK main loop turns; this one
		// clears when the first ListImages over S3 returns, which on a Pi against
		// a large bucket is seconds to minutes. A client honouring a 1 here
		// hot-loops once per second for the whole first scan, against the very
		// listing it is waiting for. Asserting presence alone let one constant
		// serve both, which is the mutant this line kills.
		got := w.Header().Get("Retry-After")
		if got == "" {
			t.Fatal("a 503 with no Retry-After — the caller is told to back off with no idea how long")
		}
		secs, err := strconv.Atoi(got)
		if err != nil {
			t.Fatalf("Retry-After = %q, want a number of seconds", got)
		}
		if secs != retryAfterIndexing {
			t.Errorf("Retry-After = %d, want %d. The indexing gate clears when a bucket listing "+
				"over S3 returns, not when the main loop turns, so it must not share the "+
				"queue-full backoff (%d).", secs, retryAfterIndexing, retryAfterQueueFull)
		}
		if retryAfterIndexing <= retryAfterQueueFull {
			t.Errorf("retryAfterIndexing (%d) is not above retryAfterQueueFull (%d); the two "+
				"backoffs have collapsed and the assertion above no longer distinguishes them",
				retryAfterIndexing, retryAfterQueueFull)
		}
		var body errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error == "" {
			t.Errorf("503 body = %q, want a JSON error message", w.Body.String())
		}
		if n := f.pendingCount(); n != 0 {
			t.Fatalf("a refused queue enqueued %d closures, want 0", n)
		}
		if got := f.callLog(); len(got) != 0 {
			t.Fatalf("a refused queue reached the viewer: %v", got)
		}
	})

	t.Run("the refusal is reported, not silent", func(t *testing.T) {
		// 🔴 The other two admission points are counted and logged by the viewer.
		// This one is decided in THIS package, which has no logger by design, so
		// without the callback an operator turned down through the whole first
		// bucket listing has zero journald evidence that the Pi said anything.
		f := newFakeViewer()
		f.snap = Snapshot{Total: 0, Scanning: true,
			ViewMode: string(ViewLandscapeSingle), SlideInterval: 30}
		var reasons []string
		s, err := New(Config{Token: testToken, Viewer: f, Version: "test",
			RefuseLog: func(reason string) { reasons = append(reasons, reason) }})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if w := do(t, s, "POST", "/api/queue", `{"keys":["a/1.jpg"]}`); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503 — this subtest is not exercising the refusal", w.Code)
		}
		if len(reasons) != 1 {
			t.Fatalf("RefuseLog called %d times, want 1. A refusal decided in this package and "+
				"reported nowhere is invisible on the Pi.", len(reasons))
		}
		if !strings.Contains(reasons[0], "queue") || !strings.Contains(reasons[0], "scan") {
			t.Errorf("RefuseLog reason = %q; it must name the endpoint and the cause, or an "+
				"operator reading journald cannot tell which admission point spoke", reasons[0])
		}

		// And an ACCEPTED request says nothing: a log line per POST would be
		// unbounded volume for the ordinary case.
		f.snap.Total = 3
		f.snap.Scanning = false
		if w := do(t, s, "POST", "/api/queue", `{"keys":["a/1.jpg"]}`); w.Code != http.StatusAccepted {
			t.Fatalf("status %d, want 202", w.Code)
		}
		if len(reasons) != 1 {
			t.Fatalf("RefuseLog was called for an ACCEPTED queue: %v", reasons)
		}
	})

	t.Run("a nil RefuseLog refuses silently rather than panicking", func(t *testing.T) {
		// Every existing test builds a Server without one, so this is the shape
		// the whole suite runs in; a Server that panicked here would take the
		// slideshow down on a request an operator makes at every boot.
		f := newFakeViewer()
		f.snap = Snapshot{Total: 0, Scanning: true,
			ViewMode: string(ViewLandscapeSingle), SlideInterval: 30}
		s := newTestServer(t, f)
		if w := do(t, s, "POST", "/api/queue", `{"keys":["a/1.jpg"]}`); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", w.Code)
		}
	})

	t.Run("cancel is NOT subject to this gate", func(t *testing.T) {
		// 🔴 handleQueueCancel carries a 🔴 comment saying it is deliberately
		// ungated, and until this test nothing enforced it: adding the same
		// `Scanning && Total == 0` check there "for consistency" SURVIVED the full
		// suite. The consequence is specific — an operator gets 503 on STOP, and
		// is told to wait to be allowed to stop, during the one state (the first
		// bucket listing at boot) where the display is least likely to be doing
		// what they want.
		//
		// There is also nothing for a cancel to be consumed against: the reason
		// the gate exists for POST /api/queue is that a queue installed against an
		// empty gallery is silently eaten, and a cancel installs nothing.
		f := newFakeViewer()
		f.snap = Snapshot{Total: 0, Scanning: true,
			ViewMode: string(ViewLandscapeSingle), SlideInterval: 30}
		s := newTestServer(t, f)

		w := do(t, s, "POST", "/api/queue/cancel", "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("POST /api/queue/cancel while the gallery is still indexing -> %d, want 202. "+
				"Cancel is deliberately ungated: there is nothing for it to be consumed against, "+
				"and refusing it asks the operator to wait to be allowed to STOP. (body %s)",
				w.Code, w.Body.String())
		}
		// And it really did reach the viewer, rather than being accepted by some
		// path that does nothing.
		f.drain()
		if got, want := f.callLog(), []string{"CancelQueue"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	})

	t.Run("a scanned but empty gallery is accepted", func(t *testing.T) {
		f := newFakeViewer()
		f.snap = Snapshot{Total: 0, Scanning: false,
			ViewMode: string(ViewLandscapeSingle), SlideInterval: 30}
		s := newTestServer(t, f)

		w := do(t, s, "POST", "/api/queue", `{"keys":["a/1.jpg"]}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("a queue posted against a scanned-but-empty gallery -> %d, want 202. "+
				"Retrying an empty bucket would never help, so the honest answer is the 202 and "+
				"an attributable skip count — not backpressure. (body %s)", w.Code, w.Body.String())
		}
	})

	t.Run("a scanning gallery that already has images is accepted", func(t *testing.T) {
		// scanning is true past the point where total is populated (a rescan's
		// completion closure is still queued). Refusing on `scanning` ALONE would
		// make the endpoint dead for the whole drain latency of the display queue.
		f := newFakeViewer("a/1.jpg", "b/2.jpg")
		f.snap.Scanning = true
		s := newTestServer(t, f)

		w := do(t, s, "POST", "/api/queue", `{"keys":["a/1.jpg"]}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("a queue posted during a rescan of a POPULATED gallery -> %d, want 202. The "+
				"refusal condition is `scanning AND total == 0`, not `scanning`. (body %s)",
				w.Code, w.Body.String())
		}
	})
}
