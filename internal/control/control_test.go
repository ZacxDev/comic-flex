package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testToken is 40 bytes — deliberately past MinTokenBytes rather than sitting
// on it, so an off-by-one in the length check cannot be masked by the fixture.
const testToken = "0123456789abcdef0123456789abcdef01234567"

func newTestServer(t *testing.T, f *fakeViewer) *Server {
	t.Helper()
	s, err := New(Config{Token: testToken, Viewer: f, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// do issues an authenticated request against the routed handler.
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doWithAuth(t, s, method, path, body, "Bearer "+testToken)
}

func doWithAuth(t *testing.T, s *Server, method, path, body, authz string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// ---------------------------------------------------------------------------
// The endpoint table — proposal §4.2, every row, plus its error rows.
// ---------------------------------------------------------------------------

func TestEndpointTable(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
		// muxGenerated marks the rows answered by net/http.ServeMux itself
		// rather than by one of our handlers; those carry text/plain.
		muxGenerated bool
	}{
		{"healthz", "GET", "/healthz", "", http.StatusOK, false},
		{"state", "GET", "/api/state", "", http.StatusOK, false},
		{"next", "POST", "/api/next", "", http.StatusAccepted, false},
		{"prev", "POST", "/api/prev", "", http.StatusAccepted, false},
		{"pause", "POST", "/api/pause", "", http.StatusAccepted, false},
		{"resume", "POST", "/api/resume", "", http.StatusAccepted, false},
		{"rescan", "POST", "/api/rescan", "", http.StatusAccepted, false},

		{"viewmode landscape_single", "POST", "/api/viewmode", `{"mode":"landscape_single"}`, http.StatusAccepted, false},
		{"viewmode portrait_single", "POST", "/api/viewmode", `{"mode":"portrait_single"}`, http.StatusAccepted, false},
		{"viewmode landscape_two", "POST", "/api/viewmode", `{"mode":"landscape_two"}`, http.StatusAccepted, false},
		{"viewmode unknown", "POST", "/api/viewmode", `{"mode":"panorama"}`, http.StatusBadRequest, false},
		{"viewmode empty", "POST", "/api/viewmode", `{"mode":""}`, http.StatusBadRequest, false},
		{"viewmode absent field", "POST", "/api/viewmode", `{}`, http.StatusBadRequest, false},
		{"viewmode malformed json", "POST", "/api/viewmode", `{"mode":`, http.StatusBadRequest, false},
		// Case matters: a near-miss must not be normalised into a valid mode.
		{"viewmode wrong case", "POST", "/api/viewmode", `{"mode":"Landscape_Single"}`, http.StatusBadRequest, false},

		{"goto known key", "POST", "/api/goto", `{"key":"b/2.jpg"}`, http.StatusAccepted, false},
		{"goto unknown key", "POST", "/api/goto", `{"key":"nope/9.jpg"}`, http.StatusNotFound, false},
		{"goto index in range", "POST", "/api/goto", `{"index":2}`, http.StatusAccepted, false},
		{"goto index at top bound", "POST", "/api/goto", `{"index":3}`, http.StatusNotFound, false},
		{"goto index negative", "POST", "/api/goto", `{"index":-1}`, http.StatusNotFound, false},
		{"goto neither", "POST", "/api/goto", `{}`, http.StatusBadRequest, false},
		{"goto empty key", "POST", "/api/goto", `{"key":""}`, http.StatusBadRequest, false},

		{"interval 1", "POST", "/api/interval", `{"seconds":1}`, http.StatusAccepted, false},
		{"interval 3600", "POST", "/api/interval", `{"seconds":3600}`, http.StatusAccepted, false},
		{"interval 0", "POST", "/api/interval", `{"seconds":0}`, http.StatusBadRequest, false},
		{"interval 3601", "POST", "/api/interval", `{"seconds":3601}`, http.StatusBadRequest, false},
		{"interval negative", "POST", "/api/interval", `{"seconds":-30}`, http.StatusBadRequest, false},
		{"interval absent field", "POST", "/api/interval", `{}`, http.StatusBadRequest, false},

		// Method discipline: mutations are POST-only, reads are GET-only.
		{"next via GET", "GET", "/api/next", "", http.StatusMethodNotAllowed, true},
		{"state via POST", "POST", "/api/state", "", http.StatusMethodNotAllowed, true},
		{"unknown api route", "POST", "/api/bogus", "", http.StatusNotFound, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Three keys, so index 3 is the first out of range. Chosen over a
			// power-of-two or a length equal to any bound in the code.
			f := newFakeViewer("a/1.jpg", "b/2.jpg", "c/3.jpg")
			s := newTestServer(t, f)
			w := do(t, s, tc.method, tc.path, tc.body)
			if w.Code != tc.want {
				t.Fatalf("%s %s -> %d, want %d (body %s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !tc.muxGenerated && ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// R1 — mutations enqueue and return 202 without the work having run.
// ---------------------------------------------------------------------------

func TestMutationsAccept202WithoutRunningTheWork(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		body     string
		wantCall string
	}{
		{"next", "/api/next", "", "Next"},
		{"prev", "/api/prev", "", "Prev"},
		{"pause", "/api/pause", "", "SetPaused:true"},
		{"resume", "/api/resume", "", "SetPaused:false"},
		{"viewmode", "/api/viewmode", `{"mode":"landscape_two"}`, "SetViewMode:landscape_two"},
		{"goto key", "/api/goto", `{"key":"c/3.jpg"}`, "GotoKey:c/3.jpg@2"},
		{"goto index", "/api/goto", `{"index":1}`, "GotoIndex:1"},
		{"interval", "/api/interval", `{"seconds":45}`, "SetInterval:45"},
		{"rescan", "/api/rescan", "", "Rescan"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg", "b/2.jpg", "c/3.jpg")
			s := newTestServer(t, f)

			w := do(t, s, "POST", tc.path, tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status %d, want 202 (body %s)", w.Code, w.Body.String())
			}

			// The proof, not the assertion: the fake has not run the closure,
			// so if the handler had awaited the work this list would be
			// non-empty. It is checked BEFORE drain.
			if got := f.callLog(); len(got) != 0 {
				t.Fatalf("work ran before the handler returned: %v — the handler awaited the mutation", got)
			}
			if n := f.pendingCount(); n != 1 {
				t.Fatalf("pending closures = %d, want exactly 1 enqueued", n)
			}

			f.drain()
			if got := f.callLog(); !reflect.DeepEqual(got, []string{tc.wantCall}) {
				t.Fatalf("after drain calls = %v, want [%s]", got, tc.wantCall)
			}
		})
	}
}

// TestMutationHandlerDoesNotAwaitABlockedGTKLoop is the harsher version: the
// GTK loop is genuinely wedged (the closure blocks forever, as
// updateSingleImage can for 30s on an S3 GET) and the handler must still
// answer. A handler that awaited the closure would hang here.
func TestMutationHandlerDoesNotAwaitABlockedGTKLoop(t *testing.T) {
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
		done <- do(t, s, "POST", "/api/next", "").Code
	}()

	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Fatalf("status %d, want 202", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return while the GTK loop was blocked — it awaited the mutation")
	}
}

// ---------------------------------------------------------------------------
// R2 — GET /api/state is a synchronous read, and its JSON shape is a contract.
// ---------------------------------------------------------------------------

func TestStateIsASynchronousReadNotAnEnqueue(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	s := newTestServer(t, f)

	w := do(t, s, "GET", "/api/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if n := f.pendingCount(); n != 0 {
		t.Fatalf("state enqueued %d closures; R2 requires none", n)
	}
	if got := f.readLog(); !reflect.DeepEqual(got, []string{"Snapshot"}) {
		t.Fatalf("reads = %v, want exactly one Snapshot", got)
	}
}

func TestStateJSONShape(t *testing.T) {
	f := newFakeViewer()
	// Every field distinct from every other, and from any constant the handler
	// names, so a mutant that hardcodes or crosses two fields cannot survive.
	f.snap = Snapshot{
		Total:         7544,
		Index:         412,
		Key:           "batman/issue-04/page-012.jpg",
		ViewMode:      string(ViewLandscapeTwo),
		Paused:        true,
		SlideInterval: 37,
		Scanning:      false,
	}
	s := newTestServer(t, f)

	w := do(t, s, "GET", "/api/state", "")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
	}
	want := map[string]any{
		"total":          float64(7544),
		"index":          float64(412),
		"key":            "batman/issue-04/page-012.jpg",
		"view_mode":      "landscape_two",
		"paused":         true,
		"slide_interval": float64(37),
		"scanning":       false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state body = %#v\nwant %#v", got, want)
	}
}

// TestStateDistinguishesScanningFromEmpty pins the one distinction §4.2 calls
// out by name: total 0 while scanning means "indexing…", total 0 after a scan
// means "no comics". They must not render identically, so they must not
// serialize identically.
func TestStateDistinguishesScanningFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		name         string
		scanning     bool
		wantScanning bool
	}{
		{"not yet scanned", true, true},
		{"scanned and empty", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeViewer()
			f.snap.Total = 0
			f.snap.Scanning = tc.scanning
			s := newTestServer(t, f)

			var got Snapshot
			w := do(t, s, "GET", "/api/state", "")
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if got.Total != 0 {
				t.Fatalf("total = %d, want 0", got.Total)
			}
			if got.Scanning != tc.wantScanning {
				t.Fatalf("scanning = %v, want %v — an un-scanned gallery and an empty one are "+
					"indistinguishable in this response", got.Scanning, tc.wantScanning)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// R3 — goto resolves inside the closure, not on the handler goroutine.
// ---------------------------------------------------------------------------

// TestGotoResolvesInsideTheClosure changes the gallery between the handler
// returning and the closure running. If the handler had captured an index, the
// closure would move to the stale one; because it passes the KEY, the closure
// resolves against the gallery as it is when it runs.
func TestGotoResolvesInsideTheClosure(t *testing.T) {
	f := newFakeViewer("a/1.jpg", "b/2.jpg", "c/3.jpg")
	s := newTestServer(t, f)

	w := do(t, s, "POST", "/api/goto", `{"key":"c/3.jpg"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", w.Code)
	}

	// A rescan lands: the gallery is reordered and c/3.jpg is now index 0.
	f.setKeys("c/3.jpg", "a/1.jpg")
	f.drain()

	if got := f.callLog(); !reflect.DeepEqual(got, []string{"GotoKey:c/3.jpg@0"}) {
		t.Fatalf("calls = %v, want [GotoKey:c/3.jpg@0] — resolution was captured on the "+
			"handler goroutine instead of happening inside the closure", got)
	}
}

// TestGoto404IsAReadNotAnEnqueue pins that a rejected goto does not put
// anything on the GTK queue.
func TestGoto404IsAReadNotAnEnqueue(t *testing.T) {
	f := newFakeViewer("a/1.jpg")
	s := newTestServer(t, f)

	w := do(t, s, "POST", "/api/goto", `{"key":"missing.jpg"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
	if n := f.pendingCount(); n != 0 {
		t.Fatalf("a rejected goto enqueued %d closures, want 0", n)
	}
	if got := f.readLog(); !reflect.DeepEqual(got, []string{"Resolve:missing.jpg"}) {
		t.Fatalf("reads = %v, want exactly one Resolve", got)
	}
}

// TestBadRequestsEnqueueNothing sweeps the 400 paths for the same property:
// cheap validation runs on the handler goroutine and reaches no state.
func TestBadRequestsEnqueueNothing(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"viewmode unknown", "/api/viewmode", `{"mode":"panorama"}`},
		{"interval out of range", "/api/interval", `{"seconds":99999}`},
		{"goto neither", "/api/goto", `{}`},
		{"malformed json", "/api/interval", `{"seconds":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			s := newTestServer(t, f)
			w := do(t, s, "POST", tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
			if n := f.pendingCount(); n != 0 {
				t.Fatalf("a rejected request enqueued %d closures, want 0", n)
			}
			if got := f.callLog(); len(got) != 0 {
				t.Fatalf("a rejected request reached the viewer: %v", got)
			}
		})
	}
}

func TestHealthzBody(t *testing.T) {
	f := newFakeViewer()
	s, err := New(Config{Token: testToken, Viewer: f, Version: "9.9.9"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := do(t, s, "GET", "/healthz", "")
	var got healthBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !got.OK || got.Version != "9.9.9" {
		t.Fatalf("healthz = %+v, want {OK:true Version:9.9.9}", got)
	}
	// It must touch no viewer state at all — that is the whole reason it may be
	// served unauthenticated.
	if len(f.readLog()) != 0 || len(f.callLog()) != 0 || f.pendingCount() != 0 {
		t.Fatalf("healthz touched viewer state: reads=%v calls=%v pending=%d",
			f.readLog(), f.callLog(), f.pendingCount())
	}
}

func TestDefaultAddrIsTheLANBind(t *testing.T) {
	f := newFakeViewer()
	s := newTestServer(t, f)
	if s.Addr() != "0.0.0.0:8790" {
		t.Fatalf("Addr = %q, want 0.0.0.0:8790 — the cluster reaches the Pi by LAN IP", s.Addr())
	}
}

func TestNewRequiresAViewer(t *testing.T) {
	if _, err := New(Config{Token: testToken}); err == nil {
		t.Fatal("New accepted a nil Viewer")
	}
}
