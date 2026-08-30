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
func TestAFullQueueIs503NotAFalse202(t *testing.T) {
	// Every mutation endpoint, so a handler that replies without going through
	// Server.enqueue is visible.
	for _, rt := range authedRoutes {
		if rt.wantOK != http.StatusAccepted {
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
