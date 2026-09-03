package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// These cover the two things decodeBody must refuse and the one thing Enqueue
// must refuse. All three were unguarded in round 1, and all three hazards were
// measured to ship at a fully green suite:
//
//   - deleting the http.MaxBytesReader line: 200 PASS / 0 FAIL,
//   - a second JSON object silently discarded: never tested at all,
//   - an unbounded GTK work queue: never tested at all.

// bodyOfSize builds a syntactically valid /api/interval body of at least n
// bytes. The seconds field is a real one, so a body that is accepted is accepted
// on its merits rather than because it failed to parse.
func bodyOfSize(n int) string {
	const head = `{"seconds":60,"pad":"`
	const tail = `"}`
	pad := n - len(head) - len(tail)
	if pad < 1 {
		pad = 1
	}
	return head + strings.Repeat("p", pad) + tail
}

// TestBodiesLargerThanTheCapAreRefused pins maxBodyBytes.
//
// 🔴 Measured: deleting `r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)`
// from decodeBody left the round-1 suite at 200 PASS / 0 FAIL. Nothing posted a
// body of any size at all.
//
// The two sizes deliberately straddle the 4 KiB cap without sitting on it and
// without being a multiple of it: 3000 must still work — that is the positive
// control that proves the test is not passing because everything is refused —
// and 9000 must not.
func TestBodiesLargerThanTheCapAreRefused(t *testing.T) {
	if maxBodyBytes <= 3000 || maxBodyBytes >= 9000 {
		t.Fatalf("maxBodyBytes = %d; the 3000/9000 fixtures no longer straddle it", maxBodyBytes)
	}

	t.Run("under the cap is accepted", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		w := do(t, s, "POST", "/api/interval", bodyOfSize(3000))
		if w.Code != http.StatusAccepted {
			t.Fatalf("a 3000-byte body -> %d, want 202 (body %s). The cap is refusing bodies "+
				"it should accept, so the over-cap case below would pass for the wrong reason.",
				w.Code, w.Body.String())
		}
		if n := f.pendingCount(); n != 1 {
			t.Fatalf("pending closures = %d, want 1", n)
		}
	})

	t.Run("over the cap is refused", func(t *testing.T) {
		f := newFakeViewer("a/1.jpg")
		s := newTestServer(t, f)
		w := do(t, s, "POST", "/api/interval", bodyOfSize(9000))
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a 9000-byte body -> %d, want 413. Without http.MaxBytesReader the decoder "+
				"reads the whole body into memory on a Raspberry Pi before any validation runs, "+
				"and this request is accepted. (body %s)", w.Code, w.Body.String())
		}
		if n := f.pendingCount(); n != 0 {
			t.Fatalf("an over-sized body enqueued %d closures, want 0", n)
		}
		if got := f.callLog(); len(got) != 0 {
			t.Fatalf("an over-sized body reached the viewer: %v", got)
		}
	})
}

// TestTrailingJSONIsRefused pins that a body carries EXACTLY ONE value.
//
// json.Decoder.Decode stops at the end of the first value and reports success,
// so `{"seconds":5}{"seconds":9999}` was accepted with the SECOND object
// silently discarded — and the caller was told 202 for a value it never sent.
// The two numbers are far apart and neither is a bound in the code, so a mutant
// that honoured the wrong one is visible in the call log.
func TestTrailingJSONIsRefused(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"two interval objects", "/api/interval", `{"seconds":5}{"seconds":9999}`},
		{"two viewmode objects", "/api/viewmode", `{"mode":"portrait_single"}{"mode":"landscape_two"}`},
		{"object then array", "/api/interval", `{"seconds":5}[1,2,3]`},
		{"object then garbage", "/api/interval", `{"seconds":5} not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			s := newTestServer(t, f)
			w := do(t, s, "POST", tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s -> %d, want 400. A second JSON value in the body is silently "+
					"discarded, so the caller is told 202 for a request it did not make. "+
					"(body %s)", tc.body, w.Code, w.Body.String())
			}
			if n := f.pendingCount(); n != 0 {
				t.Fatalf("a multi-value body enqueued %d closures, want 0", n)
			}
		})
	}

	// Positive control: ONE object, with whitespace and a trailing newline
	// around it, must still be accepted. Otherwise the assertions above would
	// pass with a decodeBody that rejected every body.
	f := newFakeViewer("a/1.jpg")
	s := newTestServer(t, f)
	w := do(t, s, "POST", "/api/interval", "  {\"seconds\":5}  \n")
	if w.Code != http.StatusAccepted {
		t.Fatalf("a single well-formed object with surrounding whitespace -> %d, want 202 "+
			"(body %s)", w.Code, w.Body.String())
	}
	f.drain()
	if got := f.callLog(); len(got) != 1 || got[0] != "SetInterval:5" {
		t.Fatalf("calls = %v, want [SetInterval:5]", got)
	}
}

// ---------------------------------------------------------------------------
// The GTK work queue is bounded
// ---------------------------------------------------------------------------

// TestAFullQueueIs503NotAFalse202 pins the refusal path through the handlers.
//
// The GTK loop drains at up to 30 s per image while an authenticated caller can
// POST as fast as the Pi accepts connections, so the queue must have a floor to
// stand on. The property that matters is not that a request is dropped — it is
// that the client is TOLD: a 202 for work that was never scheduled is the same
// lie as the discarded second JSON object above.
// enqueuedMutationPaths is every 202 endpoint whose admission goes through
// Server.enqueue, i.e. the GTK work queue. scanAdmittedPaths is every 202
// endpoint admitted by the concurrent-scan bound instead.
//
// 🔴 They are asserted to PARTITION the 202 endpoints by
// TestEveryAcceptedMutationIsCoveredByOneAdmissionTest. Without that, another
// mutation endpoint added tomorrow lands in neither list and is never driven
// against a full queue at all — the round-1 shape exactly: a ledger that catches
// removal and not growth. (POST /api/toggle is exactly such an endpoint, added
// after these lists were written; the partition test is what made adding it here
// non-optional rather than something to remember.)
var (
	enqueuedMutationPaths = []string{
		"/api/next", "/api/prev", "/api/pause", "/api/resume", "/api/toggle",
		"/api/viewmode", "/api/goto", "/api/interval", "/api/queue", "/api/queue/cancel",
	}
	scanAdmittedPaths = []string{"/api/rescan"}
)

func TestEveryAcceptedMutationIsCoveredByOneAdmissionTest(t *testing.T) {
	covered := map[string]int{}
	for _, p := range enqueuedMutationPaths {
		covered[p]++
	}
	for _, p := range scanAdmittedPaths {
		covered[p]++
	}

	seen := map[string]bool{}
	for _, rt := range authedRoutes {
		if rt.wantOK != http.StatusAccepted {
			continue
		}
		seen[rt.path] = true
		switch covered[rt.path] {
		case 1: // exactly one admission test drives it
		case 0:
			t.Errorf("%s answers 202 but appears in NEITHER enqueuedMutationPaths nor "+
				"scanAdmittedPaths, so nothing drives it against a full queue. It can answer "+
				"202 for work that was refused and no test would see it.", rt.path)
		default:
			t.Errorf("%s appears in BOTH admission lists; they are meant to partition the 202 "+
				"endpoints, and one of the two tests is asserting the wrong contract for it", rt.path)
		}
	}
	for p := range covered {
		if !seen[p] {
			t.Errorf("%s is listed as an admission-tested mutation but no longer answers 202 in "+
				"authedRoutes — this list is stale and the coverage it claims is imaginary", p)
		}
	}
}

func TestAFullQueueIs503NotAFalse202(t *testing.T) {
	// Every enqueued mutation endpoint, so a handler that replies without going
	// through Server.enqueue is visible.
	for _, rt := range authedRoutes {
		if rt.wantOK != http.StatusAccepted || !containsString(enqueuedMutationPaths, rt.path) {
			continue
		}
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			f.capacity = 2 // small, and not the real cap: this is the handler's contract
			s := newTestServer(t, f)

			for i := 0; i < f.capacity; i++ {
				if w := do(t, s, rt.method, rt.path, rt.body); w.Code != http.StatusAccepted {
					t.Fatalf("request %d of %d -> %d, want 202", i+1, f.capacity, w.Code)
				}
			}

			w := do(t, s, rt.method, rt.path, rt.body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s with a full queue -> %d, want 503. This handler answers 202 for "+
					"work the GTK loop refused to take, so the caller believes a page turn is "+
					"queued when nothing is. (body %s)", rt.method, rt.path, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got == "" {
				t.Error("a 503 with no Retry-After — the caller is told to back off with no idea how long")
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Errorf("503 body = %q, want a JSON error message", w.Body.String())
			}
			if n := f.pendingCount(); n != f.capacity {
				t.Fatalf("pending closures = %d, want %d — the refused request was queued anyway",
					n, f.capacity)
			}
			if n := f.refusedCount(); n != 1 {
				t.Fatalf("the viewer refused %d enqueues, want 1", n)
			}

			// And the queue recovers: draining it makes room again.
			f.drain()
			if w := do(t, s, rt.method, rt.path, rt.body); w.Code != http.StatusAccepted {
				t.Fatalf("after the loop drained, %s %s -> %d, want 202 — the cap is a "+
					"permanent wall rather than backpressure", rt.method, rt.path, w.Code)
			}
		})
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// TestRescanStartsTheListingWithoutEnqueueingIt pins what POST /api/rescan
// actually is: a synchronous admission decision, not a queued mutation.
//
// 🔴 Round-1 it WAS a queued mutation, and that is precisely why it was the only
// unbounded endpoint of the nine: the closure spawns the listing onto its own
// goroutine and returns in microseconds, so the queue slot it took was free
// again before the next request arrived. Driving the real enqueueBounded plus
// the real adapter 500 times measured
// `attempts=500 refused(503)=0 queueDepth=0 scansInFlight=500`.
func TestRescanStartsTheListingWithoutEnqueueingIt(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	s := newTestServer(t, f)

	w := do(t, s, "POST", "/api/rescan", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /api/rescan -> %d, want 202 (body %s)", w.Code, w.Body.String())
	}
	// The listing was STARTED, synchronously, before the handler answered...
	if got := f.callLog(); len(got) != 1 || got[0] != "Rescan" {
		t.Fatalf("calls = %v, want exactly [Rescan] — the listing must be admitted while the "+
			"caller is still there to be told 503, not deferred to the GTK loop", got)
	}
	// ...and NOTHING was put on the GTK main loop's queue. A closure here means
	// the admission decision has been moved back behind the queue, where the
	// bound cannot see the work it is supposed to bound.
	if n := f.pendingCount(); n != 0 {
		t.Fatalf("POST /api/rescan enqueued %d closures on the GTK loop; it must enqueue none — "+
			"a rescan routed through Enqueue frees its slot in microseconds and the cap becomes "+
			"decorative", n)
	}
}

// TestAFullScanBudgetIs503NotAFalse202 is the rescan half of the 503 contract:
// the same "the client is TOLD" property, against the bound that actually
// governs rescans.
func TestAFullScanBudgetIs503NotAFalse202(t *testing.T) {
	for _, path := range scanAdmittedPaths {
		t.Run("POST "+path, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			f.scanCapacity = 2 // small, and not the real cap: this is the handler's contract
			s := newTestServer(t, f)

			for i := 0; i < f.scanCapacity; i++ {
				if w := do(t, s, "POST", path, ""); w.Code != http.StatusAccepted {
					t.Fatalf("request %d of %d -> %d, want 202", i+1, f.scanCapacity, w.Code)
				}
			}

			w := do(t, s, "POST", path, "")
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("POST %s with the scan budget full -> %d, want 503. The handler answers "+
					"202 for a listing that was never started, so the caller believes the gallery "+
					"is being refreshed when nothing is. (body %s)", path, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got == "" {
				t.Error("a 503 with no Retry-After — the caller is told to back off with no idea how long")
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Errorf("503 body = %q, want a JSON error message", w.Body.String())
			}
			if n := f.scansRefusedCount(); n != 1 {
				t.Fatalf("the viewer refused %d listings, want 1", n)
			}
			if got := f.callLog(); len(got) != f.scanCapacity {
				t.Fatalf("calls = %v, want %d — the refused request started a listing anyway",
					got, f.scanCapacity)
			}

			// And it recovers: a listing finishing makes room again. A permanent
			// wall would leave the display stuck on a stale gallery forever.
			f.endScan()
			if w := do(t, s, "POST", path, ""); w.Code != http.StatusAccepted {
				t.Fatalf("after a listing finished, POST %s -> %d, want 202 — the bound is a "+
					"permanent wall rather than backpressure", path, w.Code)
			}
		})
	}
}

// TestRescanIsNotSubjectToTheGTKQueueCap is the converse of
// TestReadsAreNotSubjectToTheQueueCap, and it pins the asymmetry the round-2
// audit found backwards. A backlog of page turns on the GTK loop must not stop
// an operator refreshing the gallery, and — the part that matters — a rescan
// must not be admitted merely because the page-turn queue happens to be empty.
func TestRescanIsNotSubjectToTheGTKQueueCap(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	f.capacity = 1 // the GTK queue is full after one page turn
	s := newTestServer(t, f)

	if w := do(t, s, "POST", "/api/next", ""); w.Code != http.StatusAccepted {
		t.Fatalf("first mutation -> %d, want 202", w.Code)
	}
	if w := do(t, s, "POST", "/api/next", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("second mutation -> %d, want 503", w.Code)
	}
	if w := do(t, s, "POST", "/api/rescan", ""); w.Code != http.StatusAccepted {
		t.Fatalf("POST /api/rescan with a full GTK queue -> %d, want 202 — the rescan bound is "+
			"the scan budget, not the page-turn queue", w.Code)
	}
	if n := f.refusedCount(); n != 1 {
		t.Fatalf("Enqueue refused %d times, want 1 — the rescan went through Enqueue", n)
	}
}

// TestReadsAreNotSubjectToTheQueueCap: a full mutation queue must not stop a
// client finding out WHY. GET /api/state is an R2 read and enqueues nothing.
func TestReadsAreNotSubjectToTheQueueCap(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	f.capacity = 1
	s := newTestServer(t, f)

	if w := do(t, s, "POST", "/api/next", ""); w.Code != http.StatusAccepted {
		t.Fatalf("first mutation -> %d, want 202", w.Code)
	}
	if w := do(t, s, "POST", "/api/next", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("second mutation -> %d, want 503", w.Code)
	}
	if w := do(t, s, "GET", "/api/state", ""); w.Code != http.StatusOK {
		t.Fatalf("GET /api/state with a full queue -> %d, want 200 — a client that is being "+
			"refused cannot see the state that explains why", w.Code)
	}
	if w := doWithAuth(t, s, "GET", "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("GET /healthz with a full queue -> %d, want 200 — the liveness probe would "+
			"restart the unit for a backlog", w.Code)
	}
}
