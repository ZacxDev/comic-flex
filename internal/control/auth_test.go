package control

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fail-closed construction
// ---------------------------------------------------------------------------

// TestNewIsFailClosedOnTheToken is acceptance criterion 4. The lengths
// deliberately overshoot the 32-byte boundary in both directions rather than
// clustering on it: 8 catches a check loosened to a smaller minimum, 31/32
// catch an off-by-one, 64 catches an accidental exact-length equality.
func TestNewIsFailClosedOnTheToken(t *testing.T) {
	cases := []struct {
		name    string
		length  int
		wantErr error
	}{
		{"unset", 0, ErrTokenMissing},
		{"one byte", 1, ErrTokenTooShort},
		{"eight bytes", 8, ErrTokenTooShort},
		{"thirty-one bytes", 31, ErrTokenTooShort},
		{"thirty-two bytes", 32, nil},
		{"thirty-three bytes", 33, nil},
		{"sixty-four bytes", 64, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := strings.Repeat("k", tc.length)
			s, err := New(Config{Token: token, Viewer: newFakeViewer()})
			switch {
			case tc.wantErr == nil:
				if err != nil {
					t.Fatalf("a %d-byte token was REFUSED: %v", tc.length, err)
				}
				if s == nil {
					t.Fatal("New returned no server and no error")
				}
			default:
				if err == nil {
					t.Fatalf("a %d-byte token was ACCEPTED — the control server would bind "+
						"on a host with passwordless sudo", tc.length)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				if s != nil {
					t.Fatal("New returned a server alongside the refusal — a listener could still be created")
				}
			}
		})
	}
}

// TestRefusalNamesTheEnvVar keeps the log line actionable: the operator must be
// told which variable to set, not just that something was wrong.
func TestRefusalNamesTheEnvVar(t *testing.T) {
	_, err := New(Config{Token: "", Viewer: newFakeViewer()})
	if err == nil {
		t.Fatal("New accepted an empty token")
	}
	if !strings.Contains(err.Error(), TokenEnvVar) {
		t.Fatalf("refusal %q does not name %s", err, TokenEnvVar)
	}
}

// ---------------------------------------------------------------------------
// Real unauthenticated calls through the routed handler
// ---------------------------------------------------------------------------

// authedRoutes is the asserted ledger of everything behind the token.
//
// 🔴 It is compared against the routes routes() ACTUALLY registers — scanned out
// of control.go by scanRoutes — so it fails when the set GROWS as well as when
// it SHRINKS. Round-1 it was compared against the literal 9 instead, which
// catches shrink and nothing else: adding `POST /api/shutdown` here, or
// `GET /debug/state` returning a full unauthenticated Snapshot() to the OUTER
// mux, was measured to ship at a completely green suite.
//
// Adding an endpoint therefore means adding a row here, and that row makes the
// endpoint get driven unauthenticated by TestEveryAPIEndpointRequiresTheToken.
var authedRoutes = []struct {
	method string
	path   string
	body   string
	wantOK int
}{
	{"GET", "/api/state", "", http.StatusOK},
	{"POST", "/api/next", "", http.StatusAccepted},
	{"POST", "/api/prev", "", http.StatusAccepted},
	{"POST", "/api/pause", "", http.StatusAccepted},
	{"POST", "/api/resume", "", http.StatusAccepted},
	{"POST", "/api/toggle", "", http.StatusAccepted},
	{"POST", "/api/viewmode", `{"mode":"portrait_single"}`, http.StatusAccepted},
	{"POST", "/api/goto", `{"key":"a/1.jpg"}`, http.StatusAccepted},
	{"POST", "/api/interval", `{"seconds":60}`, http.StatusAccepted},
	{"POST", "/api/rescan", "", http.StatusAccepted},
}

func TestEveryAPIEndpointRequiresTheToken(t *testing.T) {
	// The ledger must be exactly the set routes() registers behind the token —
	// derived from control.go, not from a number written next to it.
	registered := scanControlRoutes(t).inner
	listed := make([]string, 0, len(authedRoutes))
	for _, rt := range authedRoutes {
		listed = append(listed, rt.method+" "+rt.path)
	}
	sort.Strings(listed)
	if !equalStrings(registered, listed) {
		t.Fatalf("routes() registers %v behind the token but this ledger lists %v.\n"+
			"Every authenticated endpoint must appear here, because appearing here is what "+
			"gets it driven WITHOUT credentials below. A route registered and not listed has "+
			"never been checked to require the token.", registered, listed)
	}

	for _, rt := range authedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			for _, hdr := range []struct{ name, value string }{
				{"no Authorization header", ""},
				{"empty bearer", "Bearer "},
				{"wrong token", "Bearer " + strings.Repeat("x", len(testToken))},
				{"right token, one byte truncated", "Bearer " + testToken[:len(testToken)-1]},
				{"right token, one byte appended", "Bearer " + testToken + "z"},
				{"right token, no scheme", testToken},
				{"wrong scheme", "Basic " + testToken},
			} {
				t.Run(hdr.name, func(t *testing.T) {
					f := newFakeViewer("a/1.jpg")
					s := newTestServer(t, f)
					w := doWithAuth(t, s, rt.method, rt.path, rt.body, hdr.value)
					if w.Code != http.StatusUnauthorized {
						t.Fatalf("%s %s with %s -> %d, want 401", rt.method, rt.path, hdr.name, w.Code)
					}
					if n := f.pendingCount(); n != 0 {
						t.Fatalf("an unauthenticated request enqueued %d closures", n)
					}
					if got := f.readLog(); len(got) != 0 {
						t.Fatalf("an unauthenticated request read viewer state: %v", got)
					}
				})
			}

			t.Run("right token", func(t *testing.T) {
				f := newFakeViewer("a/1.jpg")
				s := newTestServer(t, f)
				w := do(t, s, rt.method, rt.path, rt.body)
				if w.Code != rt.wantOK {
					t.Fatalf("%s %s with the right token -> %d, want %d (body %s)",
						rt.method, rt.path, w.Code, rt.wantOK, w.Body.String())
				}
			})
		})
	}
}

// TestHealthzIsUnauthenticated is the deliberate exception, and is asserted so
// that a change making it authenticated is a visible decision rather than a
// silent break of the liveness probe.
func TestHealthzIsUnauthenticated(t *testing.T) {
	f := newFakeViewer()
	s := newTestServer(t, f)
	w := doWithAuth(t, s, "GET", "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated /healthz -> %d, want 200", w.Code)
	}
}

// TestUnauthorizedAdvertisesBearer pins the response shape.
func TestUnauthorizedAdvertisesBearer(t *testing.T) {
	f := newFakeViewer()
	s := newTestServer(t, f)
	w := doWithAuth(t, s, "GET", "/api/state", "", "")
	if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// TestUnknownAPIRouteIs401WhenUnauthenticated: the middleware wraps the whole
// /api/ subtree, so an unauthenticated probe cannot enumerate routes.
func TestUnknownAPIRouteIs401WhenUnauthenticated(t *testing.T) {
	f := newFakeViewer()
	s := newTestServer(t, f)
	w := doWithAuth(t, s, "POST", "/api/definitely-not-a-route", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown route -> %d, want 401", w.Code)
	}
}

// ---------------------------------------------------------------------------
// bearerToken unit coverage
// ---------------------------------------------------------------------------

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
		wantOK bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true}, // scheme is case-insensitive per RFC 7235
		{"BEARER abc", "abc", true},
		{"Bearer ", "", true},
		{"abc", "", false},
		{"", "", false},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearerabc", "", false},
	}
	for _, tc := range cases {
		got, ok := bearerToken(tc.header)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestTokenMatches(t *testing.T) {
	if !tokenMatches(testToken, testToken) {
		t.Error("the correct token did not match")
	}
	for _, wrong := range []string{
		"",
		testToken[:len(testToken)-1],
		testToken + "z",
		strings.Repeat("x", len(testToken)),
		// Shares a long prefix — the case a short-circuiting compare leaks.
		testToken[:len(testToken)-1] + "0",
	} {
		if tokenMatches(testToken, wrong) {
			t.Errorf("a wrong token matched: %q", wrong)
		}
	}
}
