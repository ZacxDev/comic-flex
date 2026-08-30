package control

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// These are structural guards. They pin properties that NO behavioural test can
// observe:
//
//   - Constant-time comparison. A `==` mutant answers 401/202 identically to
//     subtle.ConstantTimeCompare on every input, so status codes cannot see it.
//     The property is about TIMING.
//   - The complete set of routes the mux registers. A route added directly in
//     routes() is invisible to any test that only drives the routes it already
//     knows about — including, and this is the point, an UNAUTHENTICATED one.
//   - Absence of GTK anywhere in the dependency graph. A package that imported
//     it would still pass every handler test — on a machine that has GTK3.
//
// Each guard carries its own POSITIVE CONTROL: the detector is run over source
// that DOES contain the hazard, and asserted to find it. Without that, a
// detector wired to nothing would report a reassuring zero.
//
// 🔴 Round-1 audit note. Two of these guards previously read as coverage while
// providing none, and both hazards shipped at a full green suite. The specific
// failures are recorded at each guard, because the SHAPE recurs: a docstring
// that claims a property of the live path while the body inspects a helper
// nothing forces the live path to call, and a "the set must not change" ledger
// compared against a hand-written literal instead of against the set.

// ---------------------------------------------------------------------------
// Shared AST helpers
// ---------------------------------------------------------------------------

func parseSource(t *testing.T, filename, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	return file
}

func funcNamed(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	return nil
}

// operandRoots reports the identifiers an expression ultimately READS.
//
// 🔴 It exists because the first version of this detector matched only bare
// *ast.Ident operands, and `expected[i] != presented[i]` is an *ast.IndexExpr.
// A mutant that reintroduced the byte-by-byte short circuit INSIDE tokenMatches
// therefore sailed past it. Index, slice, paren, deref and CONVERSION forms all
// carry their operand through and must all be unwrapped.
//
// A genuine call does NOT carry its operand through — otherwise the
// `subtle.ConstantTimeCompare(...) == 1` that is supposed to be here would
// itself be reported as a direct comparison of the two tokens.
func operandRoots(e ast.Expr) []string {
	switch x := e.(type) {
	case *ast.Ident:
		return []string{x.Name}
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return []string{id.Name + "." + x.Sel.Name}
		}
		return nil
	case *ast.ParenExpr:
		return operandRoots(x.X)
	case *ast.IndexExpr:
		return operandRoots(x.X)
	case *ast.SliceExpr:
		return operandRoots(x.X)
	case *ast.StarExpr:
		return operandRoots(x.X)
	case *ast.UnaryExpr:
		return operandRoots(x.X)
	case *ast.CallExpr:
		if isConversion(x.Fun) && len(x.Args) == 1 {
			return operandRoots(x.Args[0])
		}
		return nil
	}
	return nil
}

// isConversion reports whether a CallExpr's Fun is a type rather than a func.
func isConversion(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.ArrayType: // []byte(...)
		return true
	case *ast.ParenExpr:
		return isConversion(f.X)
	case *ast.Ident:
		switch f.Name {
		case "string", "byte", "rune", "bool", "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
			return true
		}
	}
	return false
}

// comparesSensitiveDirectly reports whether any ==/!= inside body has an operand
// that reaches one of the named values.
func comparesSensitiveDirectly(body ast.Node, sensitive map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
			return true
		}
		for _, side := range []ast.Expr{bin.X, bin.Y} {
			for _, root := range operandRoots(side) {
				if sensitive[root] {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// Guard 1 — the token comparison on the LIVE path is constant-time
// ---------------------------------------------------------------------------

// comparisonFindings is what inspectTokenComparison reports about one source.
type comparisonFindings struct {
	found                  bool // a func named tokenMatches exists
	callsConstantTimeCmp   bool // ... and calls subtle.ConstantTimeCompare
	comparesOperandsDirect bool // ... and/or a ==/!= reaches one of the tokens
	soleReturnIsCTCompare  bool // ... and its body is EXACTLY that one return
	bodyStatements         int  // statements in tokenMatches
	middlewareFound        bool // a func named requireToken exists
	middlewareCallsMatcher bool // ... and it calls tokenMatches
}

// inspectTokenComparison parses src and reports how the token is compared.
//
// It inspects BOTH tokenMatches and requireToken. Inspecting only the former was
// the round-1 defect: the guard's docstring claimed the property for "the
// comparison the middleware performs", but nothing tied the middleware to the
// function being inspected, so moving the comparison onto the live path
// (`if !ok || s.token != presented`) left tokenMatches correct, unused, and
// unflagged — Go does not report an unused function.
func inspectTokenComparison(t *testing.T, filename, src string) comparisonFindings {
	t.Helper()
	file := parseSource(t, filename, src)

	var out comparisonFindings

	if fn := funcNamed(file, "tokenMatches"); fn != nil {
		out.found = true
		params := map[string]bool{}
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				params[name.Name] = true
			}
		}

		out.bodyStatements = len(fn.Body.List)
		if len(fn.Body.List) == 1 {
			if ret, ok := fn.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
				out.soleReturnIsCTCompare = isConstantTimeCompareOfBoth(ret.Results[0], params)
			}
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok &&
					pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
					out.callsConstantTimeCmp = true
				}
			}
			return true
		})
		if comparesSensitiveDirectly(fn.Body, params) {
			out.comparesOperandsDirect = true
		}
	}

	if fn := funcNamed(file, "requireToken"); fn != nil {
		out.middlewareFound = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "tokenMatches" {
				out.middlewareCallsMatcher = true
			}
			return true
		})
		// The middleware's own locals: the configured token and whatever it
		// pulled out of the header.
		if comparesSensitiveDirectly(fn.Body, map[string]bool{"s.token": true, "presented": true}) {
			out.comparesOperandsDirect = true
		}
	}

	return out
}

// isConstantTimeCompareOfBoth reports whether e is exactly
// `subtle.ConstantTimeCompare(<a>, <b>) == 1` with a and b reaching two
// DIFFERENT parameters — ConstantTimeCompare(x, x) is always 1.
func isConstantTimeCompareOfBoth(e ast.Expr, params map[string]bool) bool {
	bin, ok := e.(*ast.BinaryExpr)
	if !ok || bin.Op != token.EQL {
		return false
	}
	lit, ok := bin.Y.(*ast.BasicLit)
	if !ok || lit.Value != "1" {
		return false
	}
	call, ok := bin.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ConstantTimeCompare" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "subtle" {
		return false
	}
	seen := map[string]bool{}
	for _, arg := range call.Args {
		for _, root := range operandRoots(arg) {
			if params[root] {
				seen[root] = true
			}
		}
	}
	return len(seen) == 2
}

func TestTokenComparisonIsConstantTime(t *testing.T) {
	const path = "auth.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	got := inspectTokenComparison(t, path, string(src))

	if !got.found {
		t.Fatal("no func tokenMatches in auth.go — this guard now pins nothing; " +
			"rename it back or update the guard")
	}
	if !got.middlewareFound {
		t.Fatal("no func requireToken in auth.go — this guard can no longer tell whether the " +
			"constant-time comparison is on the live authentication path")
	}
	if !got.middlewareCallsMatcher {
		t.Error("requireToken does not call tokenMatches. The constant-time comparison is then " +
			"NOT on the live path: tokenMatches can be entirely correct, entirely unused, and the " +
			"middleware can be comparing the token with ==. Go does not report an unused function " +
			"and every status-code assertion still passes, so nothing else catches this.")
	}
	if !got.callsConstantTimeCmp {
		t.Error("tokenMatches does not call subtle.ConstantTimeCompare. A short-circuiting " +
			"comparison leaks a prefix timing oracle for the control token, and no status-code " +
			"test can see that.")
	}
	if got.comparesOperandsDirect {
		t.Error("a ==/!= in tokenMatches or requireToken has the configured token or the " +
			"presented one as an operand — through an index, a slice or a conversion if not " +
			"directly. That is the short-circuiting comparison this guard exists to forbid.")
	}
	if !got.soleReturnIsCTCompare {
		t.Errorf("tokenMatches is not exactly one statement returning "+
			"`subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1` "+
			"(its body has %d statement(s)). Anything that inspects the operands BEFORE the "+
			"constant-time compare — a length check, a byte loop, an early return — puts the "+
			"timing oracle back inside the very function this guard inspects, and both operands "+
			"must reach the compare or it is comparing something with itself.",
			got.bodyStatements)
	}
	if !importsPath(t, path, string(src), "crypto/subtle") {
		t.Error("auth.go does not import crypto/subtle")
	}
}

// TestTokenComparisonDetectorCanFire is the positive control for the guard
// above. It feeds the detector the two mutants a round-1 audit actually got past
// it, plus the correct source, and asserts it separates them. A green
// TestTokenComparisonIsConstantTime is worth nothing without this, because a
// detector that never fires reports clean on anything.
func TestTokenComparisonDetectorCanFire(t *testing.T) {
	// The shape the ORIGINAL guard did catch.
	const naive = `package control

func tokenMatches(expected, presented string) bool {
	return expected == presented
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !tokenMatches(s.token, presented) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
`
	got := inspectTokenComparison(t, "naive.go", naive)
	if !got.found {
		t.Fatal("the detector did not even find tokenMatches in the mutant")
	}
	if got.callsConstantTimeCmp {
		t.Error("the detector claims the == mutant calls ConstantTimeCompare")
	}
	if !got.comparesOperandsDirect {
		t.Error("the detector did NOT flag `expected == presented`")
	}
	if got.soleReturnIsCTCompare {
		t.Error("the detector accepted `return expected == presented` as the constant-time form")
	}

	// MUTANT A, measured green against the round-1 suite: move the comparison
	// onto the live path and leave tokenMatches behind as dead code.
	const deadMatcher = `package control

import "crypto/subtle"

func tokenMatches(expected, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || s.token != presented {
			return
		}
		next.ServeHTTP(w, r)
	})
}
`
	got = inspectTokenComparison(t, "dead.go", deadMatcher)
	if !got.middlewareFound {
		t.Fatal("the detector did not find requireToken in mutant A")
	}
	if got.middlewareCallsMatcher {
		t.Error("the detector claims mutant A's requireToken calls tokenMatches — it does not, " +
			"and that is the entire hazard: the guarded function is dead code")
	}
	if !got.comparesOperandsDirect {
		t.Error("the detector did NOT flag `s.token != presented` in the middleware — the " +
			"comparison it exists to forbid, on the live path")
	}

	// MUTANT B, measured green against the round-1 suite: defeat the property
	// INSIDE the inspected function. The operands are *ast.IndexExpr, which the
	// round-1 identifier-only detector could not see.
	const shortCircuit = `package control

import "crypto/subtle"

func tokenMatches(expected, presented string) bool {
	if len(expected) != len(presented) {
		return false
	}
	for i := 0; i < len(expected); i++ {
		if expected[i] != presented[i] {
			return false
		}
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !tokenMatches(s.token, presented) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
`
	got = inspectTokenComparison(t, "shortcircuit.go", shortCircuit)
	if !got.comparesOperandsDirect {
		t.Error("the detector did NOT flag `expected[i] != presented[i]` — an IndexExpr, not a " +
			"bare identifier. This is mutant B, and it survived the round-1 guard.")
	}
	if got.soleReturnIsCTCompare {
		t.Error("the detector accepted a tokenMatches with a byte loop in front of the " +
			"constant-time compare as the sole-return form")
	}
	if got.bodyStatements < 2 {
		t.Errorf("the detector counted %d statements in a three-statement mutant", got.bodyStatements)
	}

	// And a self-comparison, which is constant-time and always true.
	const selfCompare = `package control

import "crypto/subtle"

func tokenMatches(expected, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(expected)) == 1
}
`
	if got = inspectTokenComparison(t, "self.go", selfCompare); got.soleReturnIsCTCompare {
		t.Error("the detector accepted ConstantTimeCompare(expected, expected), which is always 1")
	}

	// Second control: source that is CORRECT must come back clean, so the
	// detector is not simply always-positive.
	const good = `package control

import "crypto/subtle"

func tokenMatches(expected, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !tokenMatches(s.token, presented) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
`
	ok := inspectTokenComparison(t, "good.go", good)
	if !ok.callsConstantTimeCmp || ok.comparesOperandsDirect ||
		!ok.soleReturnIsCTCompare || !ok.middlewareCallsMatcher {
		t.Errorf("the detector misjudged correct source: %+v", ok)
	}
}

// ---------------------------------------------------------------------------
// Guard 2 — the COMPLETE set of routes routes() registers
// ---------------------------------------------------------------------------

// routeScan is every route registration found inside routes(), split by which
// mux it landed on.
//
// 🔴 This exists because the round-1 auth ledger compared its own length against
// the literal 9. That catches a route being REMOVED from the ledger and catches
// nothing at all when one is ADDED to the mux — including
// `mux.HandleFunc("GET /debug/state", …)` returning a full unauthenticated
// Snapshot(), which was measured to ship at a completely green suite.
type routeScan struct {
	outerMux     string   // the variable the unauthenticated mux is built on
	innerMux     string   // the variable mounted behind requireToken
	mountPattern string   // where the inner mux is mounted on the outer one
	mounted      bool     // the inner mux IS mounted behind requireToken
	outer        []string // patterns registered on the outer mux (mount excluded)
	inner        []string // patterns registered on the inner mux
	unknown      []string // registrations this scanner cannot attribute
}

// scanRoutes parses routes() and enumerates every Handle/HandleFunc call in it.
//
// It identifies which mux is which STRUCTURALLY — by finding the
// `<outer>.Handle(<pattern>, s.requireToken(<inner>))` call — rather than by the
// variable names, so renaming a variable does not silently blind it.
func scanRoutes(t *testing.T, filename, src string) routeScan {
	t.Helper()
	file := parseSource(t, filename, src)
	fn := funcNamed(file, "routes")
	if fn == nil {
		t.Fatal("no func routes() in " + filename + " — this guard can no longer see the route set")
	}

	var out routeScan

	// Pass 1: find the mount, which tells us which mux is authenticated.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Handle" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		wrap, ok := call.Args[1].(*ast.CallExpr)
		if !ok {
			return true
		}
		wrapSel, ok := wrap.Fun.(*ast.SelectorExpr)
		if !ok || wrapSel.Sel.Name != "requireToken" || len(wrap.Args) != 1 {
			return true
		}
		inner, ok := wrap.Args[0].(*ast.Ident)
		if !ok {
			return true
		}
		out.mounted = true
		out.outerMux = recv.Name
		out.innerMux = inner.Name
		out.mountPattern = literalString(call.Args[0])
		return true
	})

	// Pass 2: attribute every registration.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pattern := literalString(call.Args[0])
		switch {
		case out.mounted && recv.Name == out.innerMux:
			out.inner = append(out.inner, pattern)
		case out.mounted && recv.Name == out.outerMux:
			if pattern == out.mountPattern && sel.Sel.Name == "Handle" {
				return true // the mount itself, already accounted for
			}
			out.outer = append(out.outer, pattern)
		default:
			out.unknown = append(out.unknown, recv.Name+"."+sel.Sel.Name+"("+pattern+")")
		}
		return true
	})

	sort.Strings(out.outer)
	sort.Strings(out.inner)
	sort.Strings(out.unknown)
	return out
}

// literalString unquotes a string literal, or returns a marker that will not
// match any expected pattern — a route registered from a computed value is
// invisible to this scanner and must fail loudly rather than be skipped.
func literalString(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "<non-literal pattern>"
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "<unparseable pattern>"
	}
	return s
}

func scanControlRoutes(t *testing.T) routeScan {
	t.Helper()
	src, err := os.ReadFile("control.go")
	if err != nil {
		t.Fatalf("reading control.go: %v", err)
	}
	return scanRoutes(t, "control.go", string(src))
}

// TestTheAPISubtreeIsMountedBehindTheMiddleware pins the relationship the whole
// authentication story rests on: there is exactly one authenticated mux, and it
// is reached only through requireToken.
func TestTheAPISubtreeIsMountedBehindTheMiddleware(t *testing.T) {
	got := scanControlRoutes(t)
	if !got.mounted {
		t.Fatal("routes() no longer mounts a mux behind s.requireToken(...). Every /api/ route " +
			"is then served with no authentication at all, on a port that is open to the LAN of " +
			"a host with passwordless sudo.")
	}
	if got.mountPattern != "/api/" {
		t.Errorf("the authenticated subtree is mounted at %q, want \"/api/\" — a route under "+
			"/api/ that falls outside the mount is served unauthenticated", got.mountPattern)
	}
	if len(got.unknown) != 0 {
		t.Errorf("routes() registers %v on a mux this guard cannot attribute, or from a "+
			"non-literal pattern. Either way the route set is no longer enumerable and the "+
			"ledger in auth_test.go is no longer a ledger.", got.unknown)
	}
}

// TestOnlyHealthzIsRegisteredUnauthenticated is the growth half the round-1
// ledger did not have. It asserts the OUTER mux — the one in front of the
// middleware — carries exactly one route, AND drives every route it finds there
// without credentials to prove nothing else answers.
func TestOnlyHealthzIsRegisteredUnauthenticated(t *testing.T) {
	got := scanControlRoutes(t)
	want := []string{"GET /healthz"}
	if !equalStrings(got.outer, want) {
		t.Errorf("routes() registers %v outside the token, want exactly %v. Anything else here "+
			"is served to anyone who can reach :8790.", got.outer, want)
	}

	// Behavioural half: whatever the scan found, drive it with no credentials.
	for _, pattern := range got.outer {
		method, path, ok := splitPattern(pattern)
		if !ok {
			t.Errorf("cannot drive route %q — it has no method", pattern)
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			f := newFakeViewer("a/1.jpg")
			s := newTestServer(t, f)
			w := doWithAuth(t, s, method, path, "", "")
			if path == "/healthz" {
				return // the deliberate exception, asserted in auth_test.go
			}
			if w.Code < 400 {
				t.Errorf("unauthenticated %s -> %d, and it is not /healthz", pattern, w.Code)
			}
			if reads := f.readLog(); len(reads) != 0 {
				t.Errorf("unauthenticated %s read viewer state: %v", pattern, reads)
			}
		})
	}
}

// TestRouteScannerCanFire is the positive control for the scanner: fed the two
// route-growth mutants that shipped green in round 1, it must see them.
func TestRouteScannerCanFire(t *testing.T) {
	const unauthedDebugRoute = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /debug/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.viewer.Snapshot())
	})
	mux.Handle("/api/", s.requireToken(api))
	return mux
}
`
	got := scanRoutes(t, "mutant.go", unauthedDebugRoute)
	if !got.mounted || got.outerMux != "mux" || got.innerMux != "api" {
		t.Fatalf("the scanner did not identify the two muxes: %+v", got)
	}
	if !equalStrings(got.outer, []string{"GET /debug/state", "GET /healthz"}) {
		t.Fatalf("the scanner reported outer routes %v; it cannot see an unauthenticated route "+
			"added straight to the outer mux, so its clean verdict on control.go means nothing",
			got.outer)
	}

	const extraAPIRoute = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)
	api.HandleFunc("POST /api/shutdown", s.handleShutdown)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}
`
	got = scanRoutes(t, "mutant2.go", extraAPIRoute)
	if !equalStrings(got.inner, []string{"GET /api/state", "POST /api/shutdown"}) {
		t.Fatalf("the scanner reported api routes %v; it cannot see a route added to the "+
			"authenticated mux, so the auth ledger cannot be derived from it", got.inner)
	}

	const unmounted = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", api)
	return mux
}
`
	if got = scanRoutes(t, "mutant3.go", unmounted); got.mounted {
		t.Fatal("the scanner claims the api mux is mounted behind requireToken when it is " +
			"mounted bare — it cannot see the whole API being served unauthenticated")
	}

	// Negative side: the real file must come back with a mount and no unknowns.
	if real := scanControlRoutes(t); !real.mounted || len(real.unknown) != 0 {
		t.Fatalf("the scanner misjudged the real control.go: %+v", real)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// splitPattern splits a Go 1.22 ServeMux pattern into its method and path.
func splitPattern(pattern string) (method, path string, ok bool) {
	method, path, found := strings.Cut(pattern, " ")
	if !found || method == "" || path == "" {
		return "", "", false
	}
	return method, path, true
}

// ---------------------------------------------------------------------------
// Guard 3 — nothing in this package's dependency GRAPH is GTK
// ---------------------------------------------------------------------------

// forbiddenImportSubstrings are the toolchain-bound packages internal/control
// must never reach for, directly OR through anything it imports. Importing any
// of them would make this package require a GTK3 + X11 build environment, which
// is exactly what the port interface exists to avoid.
var forbiddenImportSubstrings = []string{"gotk3", "/glib", "/gtk", "/gdk"}

func hasForbidden(path string) bool {
	for _, bad := range forbiddenImportSubstrings {
		if strings.Contains(path, bad) {
			return true
		}
	}
	return false
}

// packageDeps asks the toolchain for the complete transitive dependency set of
// a package pattern.
func packageDeps(t *testing.T, pattern string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pattern).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pattern, err, out)
	}
	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}
	return deps
}

// TestControlPackageDependenciesIncludeNoGTK is the mechanically complete
// version of this check.
//
// 🔴 It replaces a scan that globbed "*.go" in this directory. That scan was
// blind to a future subpackage under internal/control and blind to a GTK
// dependency arriving TRANSITIVELY through some other import — and "does this
// package still build with a bare toolchain" is the property that actually
// matters, which no per-file import list can answer. Asking `go list -deps` is
// the whole graph, resolved by the same toolchain that would do the building.
func TestControlPackageDependenciesIncludeNoGTK(t *testing.T) {
	deps := packageDeps(t, ".")

	// Positive control on the RESULT SET: an empty or truncated dependency list
	// would make the scan below vacuously clean.
	if len(deps) < 20 {
		t.Fatalf("go list reported only %d dependencies (%v) — it is not seeing the package", len(deps), deps)
	}
	found := map[string]bool{}
	for _, d := range deps {
		found[d] = true
	}
	for _, must := range []string{"crypto/subtle", "encoding/json", "net/http"} {
		if !found[must] {
			t.Fatalf("go list -deps . did not report %q, which this package plainly imports — "+
				"the dependency set being scanned is not this package's", must)
		}
	}

	var hits []string
	for _, d := range deps {
		if hasForbidden(d) {
			hits = append(hits, d)
		}
	}
	if len(hits) != 0 {
		t.Errorf("internal/control depends on %v. It must build with no GTK3/X11 toolchain, "+
			"and gotk3 cannot be cross-compiled. Put the adapter in package main instead.", hits)
	}
	t.Logf("scanned %d transitive dependencies", len(deps))
}

// TestForbiddenDependencyDetectorCanFire is the positive control, and it uses
// REAL data rather than a synthetic fixture: package main genuinely does depend
// on gotk3, so the same scanner run against it MUST come back dirty. A scanner
// that reported clean on that is wired to nothing and its verdict above is
// worthless.
func TestForbiddenDependencyDetectorCanFire(t *testing.T) {
	deps := packageDeps(t, "../..") // the module root, i.e. package main
	var hits []string
	for _, d := range deps {
		if hasForbidden(d) {
			hits = append(hits, d)
		}
	}
	if len(hits) == 0 {
		t.Fatalf("the scanner found no GTK dependency in package main, which imports "+
			"gotk3/gtk, gotk3/gdk and gotk3/glib directly (%d deps scanned). It cannot observe "+
			"the hazard, so its clean verdict on internal/control means nothing.", len(deps))
	}
	t.Logf("positive control: package main pulls in %d forbidden dependencies, e.g. %v",
		len(hits), hits[:min(3, len(hits))])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// importsPath reports whether src imports want.
func importsPath(t *testing.T, filename, src, want string) bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	for _, imp := range file.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == want {
			return true
		}
	}
	return false
}
