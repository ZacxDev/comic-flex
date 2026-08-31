package control

import (
	"fmt"
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

// srcFile is one parsed-from-memory source file. scanRoutesFiles takes a set of
// them so the real scan can cover the WHOLE package while the positive controls
// can still feed it a single synthetic file.
type srcFile struct {
	name string
	src  string
}

// scanRoutesFiles enumerates every route registration reachable from routes().
//
// It identifies which mux is which STRUCTURALLY — by finding the
// `<outer>.Handle(<pattern>, s.requireToken(<inner>))` call — rather than by the
// variable names, so renaming a variable does not silently blind it.
//
// 🔴 ROUND-2 REBUILD. The round-1 version of this scanner inspected only the
// body of routes(), and only registrations whose receiver was a bare *ast.Ident.
// Two ordinary refactors walked straight past it, BOTH measured green at
// 243 PASS / 0 FAIL, and both served `GET /debug/state -> 200` with a full
// Snapshot to anyone who could reach :8790:
//
//  1. `s.registerDebug(mux)` — the registration moved into a helper, so all
//     routes() contained was a CallExpr the scanner ignored.
//  2. `wrapper.outer.HandleFunc("GET /debug/state", …)` — a SelectorExpr
//     receiver, where the scanner did `if !ok { return true }` and SKIPPED it.
//
// The lesson is the shape, not the two instances: a scanner that RETURNS on a
// form it does not recognise reports clean on every form nobody thought of. So
// this one records an UNKNOWN — which fails the test — for anything it cannot
// attribute:
//
//   - a Handle/HandleFunc whose receiver is not one of the muxes routes() built,
//     in any expression form,
//   - any other call inside routes() that is HANDED a mux, or an ALIAS of one
//     (the helper case), which is receiver-form-independent and holds wherever
//     the helper lives, including another package,
//   - any *Server method anywhere in this package that takes a *http.ServeMux
//     AND registers on it or hands it on,
//   - any Handle/HandleFunc anywhere in this package outside routes(), whose
//     receiver is a mux this scanner can identify or whose first argument is a
//     literal route pattern.
//
// 🔴 ROUND-3 REBUILD, three findings:
//
//  1. muxVars was populated only from `x := http.NewServeMux()`, so ONE ALIAS
//     defeated pass 3 entirely. `dbg := mux; debugreg.Register(dbg)`, with the
//     sibling package doing `mux.HandleFunc("GET /debug/ping", …)`, was measured
//     green at 250 PASS / 0 FAIL and served `GET /debug/ping -> 200` with no
//     credentials. Aliases are now followed to a fixpoint.
//  2. Pass 4 flagged ANY method named Handle or HandleFunc anywhere in the
//     package. `Handle` is an extremely common method name — an unrelated
//     `func (q jobQueue) Handle(msg string)` called from a *Server method failed
//     two tests with `registers "tick" outside routes()`. A guard that cries
//     wolf gets deleted, and a deleted guard covers nothing, so it is narrowed
//     to a receiver this scanner can see IS a mux, or a first argument that is
//     literally a route pattern.
//  3. Pass 4 flagged any *Server method TAKING a *http.ServeMux, so a read-only
//     `func (s *Server) describeMux(mux *http.ServeMux) string` failed two
//     tests. It now flags only a method that actually registers on that mux or
//     hands it on — the hand-off from routes() is caught independently by pass 3
//     however far away the helper lives, so nothing is lost.
func scanRoutesFiles(t *testing.T, files []srcFile) routeScan {
	t.Helper()

	parsed := make([]*ast.File, 0, len(files))
	var fn *ast.FuncDecl
	var fnFile string
	for _, f := range files {
		file := parseSource(t, f.name, f.src)
		parsed = append(parsed, file)
		if got := funcNamed(file, "routes"); got != nil {
			if fn != nil {
				t.Fatalf("two funcs named routes (%s and %s) — this guard cannot tell which one "+
					"builds the served handler", fnFile, f.name)
			}
			fn, fnFile = got, f.name
		}
	}
	if fn == nil {
		t.Fatal("no func routes() in the scanned sources — this guard can no longer see the route set")
	}

	var out routeScan

	// The muxes routes() itself constructs, AND every alias of one. Anything
	// registered on something else is by definition not attributable to one of
	// them.
	//
	// 🔴 The alias half is round 3's finding. Without it, `dbg := mux` made the
	// mux invisible to pass 3 and `debugreg.Register(dbg)` — a sibling package
	// registering an unauthenticated route — shipped at a green suite.
	muxVars := muxIdents(fn, nil)

	// Pass 1: find the mount, which tells us which mux is authenticated. The
	// wrap call is remembered by identity so pass 3 does not report it as a
	// stray hand-off of the inner mux.
	var mountWrap *ast.CallExpr
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
		mountWrap = wrap
		return true
	})

	// Pass 2: attribute every registration inside routes().
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return true
		}
		pattern := literalString(call.Args[0])
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			// 🔴 NOT `return true`. `wrapper.outer.HandleFunc(...)` is a
			// registration this scanner cannot attribute; skipping it is how the
			// round-1 version reported clean on an unauthenticated /debug/state.
			out.unknown = append(out.unknown,
				exprString(sel.X)+"."+sel.Sel.Name+"("+strconv.Quote(pattern)+
					") — receiver is not one of the muxes routes() built")
			return true
		}
		switch {
		case out.mounted && recv.Name == out.innerMux:
			out.inner = append(out.inner, pattern)
		case out.mounted && recv.Name == out.outerMux:
			if pattern == out.mountPattern && sel.Sel.Name == "Handle" {
				return true // the mount itself, already accounted for
			}
			out.outer = append(out.outer, pattern)
		default:
			out.unknown = append(out.unknown,
				recv.Name+"."+sel.Sel.Name+"("+strconv.Quote(pattern)+")")
		}
		return true
	})

	// Pass 3: any OTHER call inside routes() that is handed one of the muxes, or
	// an alias of one. This is what catches indirect registration —
	// `s.registerDebug(mux)`, `debugreg.Register(dbg)` — and it does so at the
	// CALL SITE, without needing to find the callee, so it holds wherever the
	// callee lives: another file, another package, an interface method.
	//
	// That is the only cross-package claim in this file, and it is a claim about
	// hand-offs FROM routes(). Pass 4 below is package-scoped and says so.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || call == mountWrap {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
			(sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
			return true // already attributed by pass 2
		}
		for _, arg := range call.Args {
			id, ok := arg.(*ast.Ident)
			if !ok || !muxVars[id.Name] {
				continue
			}
			out.unknown = append(out.unknown,
				exprString(call.Fun)+"("+id.Name+") — a call inside routes() is handed the mux, "+
					"so it may register routes this scanner cannot see")
		}
		return true
	})

	// Pass 4, PACKAGE-WIDE (and package-wide only — a registrar in another
	// package is caught by pass 3's hand-off, not here): registration that never
	// appears in routes() at all.
	for i, file := range parsed {
		for _, decl := range file.Decls {
			decl, ok := decl.(*ast.FuncDecl)
			if !ok || decl.Body == nil || decl == fn {
				continue
			}
			// Every identifier in THIS function that this scanner can see holds a
			// mux: a *http.ServeMux parameter, a local from http.NewServeMux, and
			// any alias of either.
			local := muxIdents(decl, nil)

			ast.Inspect(decl.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				// 🔴 NARROWED in round 3. Flagging every method named Handle or
				// HandleFunc made an unrelated `q.Handle("tick")` on a job queue
				// fail two tests, and a guard that cries wolf is a guard someone
				// deletes. Two things still make it fire, and either one is
				// enough: the receiver is something this scanner can SEE is a mux,
				// or the first argument is literally a route pattern (in which
				// case whatever it is registering on, it is registering a route).
				recvIsMux := false
				if id, ok := sel.X.(*ast.Ident); ok && local[id.Name] {
					recvIsMux = true
				}
				if !recvIsMux && !isRoutePatternLiteral(call.Args[0]) {
					return true
				}
				out.unknown = append(out.unknown,
					files[i].name+":"+decl.Name.Name+" registers "+
						strconv.Quote(literalString(call.Args[0]))+" outside routes()")
				return true
			})
		}
	}

	sort.Strings(out.outer)
	sort.Strings(out.inner)
	sort.Strings(out.unknown)
	return out
}

// muxIdents reports every identifier inside fn that holds an *http.ServeMux, as
// far as this scanner can tell: a parameter declared *http.ServeMux, a local
// assigned from http.NewServeMux(), and any alias of either, followed to a
// fixpoint. seed lets a caller add names it already knows.
//
// 🔴 The alias fixpoint is round 3's finding 1. Two lines — `dbg := mux` then
// handing dbg to a sibling package's registrar — served an unauthenticated
// endpoint past all three route guards.
func muxIdents(fn *ast.FuncDecl, seed map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range seed {
		out[k] = true
	}
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if !isServeMuxPointer(field.Type) {
				continue
			}
			for _, name := range field.Names {
				out[name.Name] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		note := func(lhs ast.Expr, rhs ast.Expr) {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name == "_" || out[id.Name] {
				return
			}
			switch r := rhs.(type) {
			case *ast.CallExpr:
				sel, ok := r.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewServeMux" {
					return
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
					return
				}
			case *ast.Ident:
				if !out[r.Name] {
					return
				}
			default:
				return
			}
			out[id.Name] = true
			changed = true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					note(x.Lhs[0], x.Rhs[0])
				}
			case *ast.ValueSpec:
				if len(x.Names) == 1 && len(x.Values) == 1 {
					note(x.Names[0], x.Values[0])
				}
			}
			return true
		})
	}
	return out
}

func isServeMuxPointer(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ServeMux" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// isRoutePatternLiteral reports whether e is a string literal shaped like a
// Go 1.22 ServeMux pattern: "/path" or "METHOD /path".
func isRoutePatternLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return true
	}
	method, path, found := strings.Cut(s, " ")
	if !found || !strings.HasPrefix(path, "/") || method == "" {
		return false
	}
	return method == strings.ToUpper(method)
}

// 🔴 REMOVED in round 3: a rule that flagged any *Server method merely TAKING a
// *http.ServeMux. It fired on `func (s *Server) describeMux(mux *http.ServeMux)
// string`, which registers nothing, and any widening of it to "…and hands it on"
// fired on `fmt.Sprintf("%T", mux)`. A guard that fails on ordinary code is a
// guard someone deletes, and a deleted guard covers nothing.
//
// Nothing is lost, and this is the argument rather than an assertion. The only
// way a registrar can touch the mux that gets SERVED is to be handed it, and the
// only place it can be handed it from is routes() — which pass 3 flags at the
// FIRST HOP, whatever the callee is and wherever it lives. A helper that is
// never reached from routes() cannot affect the served mux; a helper reached
// through a chain is flagged at the chain's first hop; a route registered on a
// Server FIELD (`s.mux.HandleFunc("GET /debug", …)`) from anywhere in the
// package is flagged by the route-pattern half of the sweep above; and New
// serving something other than routes() is TestTheServedHandlerIsTheOneRoutesBuilt.
// TestRouteScannerCanFire holds a control for each of those.

// exprString renders an expression for a failure message. It handles the forms
// a receiver or a callee can take; anything else is reported by its Go type so
// the message still names something a maintainer can grep for.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.ParenExpr:
		return "(" + exprString(x.X) + ")"
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.IndexExpr:
		return exprString(x.X) + "[...]"
	case *ast.CallExpr:
		return exprString(x.Fun) + "(...)"
	}
	return fmt.Sprintf("<%T>", e)
}

// scanRoutes scans a single source. The positive controls use it; the real scan
// goes through scanControlRoutes, which covers the whole package.
func scanRoutes(t *testing.T, filename, src string) routeScan {
	t.Helper()
	return scanRoutesFiles(t, []srcFile{{name: filename, src: src}})
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

// scanControlRoutes scans EVERY non-test source in this package, not just
// control.go.
//
// 🔴 Reading one file was a second way to be narrower than the sentence: moving
// `registerDebug` into a new file of the same package would have taken the
// registration out of the scanner's view entirely, and the package still serves
// whatever routes() returns. The unit that matters is the package.
func scanControlRoutes(t *testing.T) routeScan {
	t.Helper()
	return scanRoutesFiles(t, controlSources(t))
}

// controlSources reads every non-test source in this package, with the positive
// control on the input set that makes the scans built on it non-vacuous.
func controlSources(t *testing.T) []srcFile {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var files []srcFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		files = append(files, srcFile{name: name, src: string(src)})
	}
	// Positive control on the INPUT SET: an empty or truncated file list would
	// make every scan built on it vacuously clean.
	if len(files) < 3 {
		t.Fatalf("only %d non-test sources found in this package (%v) — the scanner is not "+
			"looking at the package it claims to check", len(files), files)
	}
	var haveControl bool
	for _, f := range files {
		if f.name == "control.go" {
			haveControl = true
		}
	}
	if !haveControl {
		t.Fatal("control.go was not among the scanned sources")
	}
	return files
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

	// 🔴 ROUND-2 MUTANT 1, measured green at 243 PASS / 0 FAIL against the
	// round-1 rebuild of this scanner, and probed live: unauthenticated
	// `GET /debug/state -> 200 body={"total":1,…}`. routes() contains nothing
	// but a CallExpr, so a scanner that only enumerates Handle/HandleFunc calls
	// in routes() sees an entirely clean function.
	const helperRegistration = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.registerDebug(mux)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}

func (s *Server) registerDebug(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.viewer.Snapshot())
	})
}
`
	got = scanRoutes(t, "mutant4.go", helperRegistration)
	if len(got.unknown) == 0 {
		t.Fatalf("the scanner reported NO unknowns for a routes() that registers its debug "+
			"endpoint through a helper (outer=%v inner=%v). An unauthenticated GET /debug/state "+
			"returning a full Snapshot ships green, which is exactly what round 1 measured.",
			got.outer, got.inner)
	}
	if equalStrings(got.outer, []string{"GET /healthz"}) && len(got.unknown) == 0 {
		t.Fatal("the scanner attributed the helper's route to nothing at all")
	}

	// 🔴 ROUND-2 MUTANT 2, also measured green at 243 PASS / 0 FAIL: the same
	// route, registered directly, on a receiver that is not a bare *ast.Ident.
	// The round-1 scanner did `if !ok { return true }` here — it SKIPPED the
	// registration rather than recording that it could not attribute it.
	const nonIdentReceiver = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	wrapper := struct{ outer *http.ServeMux }{outer: mux}
	wrapper.outer.HandleFunc("GET /debug/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.viewer.Snapshot())
	})
	mux.Handle("/api/", s.requireToken(api))
	return mux
}
`
	got = scanRoutes(t, "mutant5.go", nonIdentReceiver)
	if len(got.unknown) == 0 {
		t.Fatalf("the scanner reported NO unknowns for `wrapper.outer.HandleFunc(\"GET "+
			"/debug/state\", …)` (outer=%v). A SelectorExpr receiver is an ordinary refactor and "+
			"it served the snapshot unauthenticated at a fully green suite.", got.outer)
	}
	if containsSubstring(got.unknown, "GET /debug/state") == false {
		t.Errorf("the scanner's unknowns %v do not name the route it could not attribute — a "+
			"maintainer cannot act on it", got.unknown)
	}

	// A route registered from a computed pattern must also be loud rather than
	// silently attributed to the wrong bucket.
	const computedPattern = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc(debugPath, s.handleState)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}
`
	got = scanRoutes(t, "mutant6.go", computedPattern)
	if !containsSubstring(got.outer, "<non-literal pattern>") &&
		!containsSubstring(got.unknown, "<non-literal pattern>") {
		t.Errorf("a route registered from a computed pattern was reported as outer=%v unknown=%v "+
			"— neither names it, so TestOnlyHealthzIsRegisteredUnauthenticated cannot fail on it",
			got.outer, got.unknown)
	}

	// 🔴 ROUND-3 MUTANT, measured green at 250 PASS / 0 FAIL and probed live at
	// `unauthenticated GET /debug/ping -> 200 body={"debug":"pong"}`: ONE ALIAS
	// of the mux, handed to a registrar in a SIBLING PACKAGE. Pass 3 fired only
	// on a bare identifier that was in muxVars, and muxVars was populated only
	// from `x := http.NewServeMux()`.
	const aliasedMux = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	dbg := mux
	debugreg.Register(dbg)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}
`
	got = scanRoutes(t, "mutant7.go", aliasedMux)
	if len(got.unknown) == 0 {
		t.Fatalf("the scanner reported NO unknowns for `dbg := mux; debugreg.Register(dbg)` "+
			"(outer=%v inner=%v). Two lines, one of them an alias, put an unauthenticated route on "+
			"the served mux from another package with every route guard green.",
			got.outer, got.inner)
	}
	if !containsSubstring(got.unknown, "dbg") {
		t.Errorf("the scanner's unknowns %v do not name the alias it could not follow", got.unknown)
	}

	// 🔴 ROUND-3 CRY-WOLF CONTROLS. A guard that fails on ordinary, harmless code
	// gets deleted, and a deleted guard covers nothing. Neither of these
	// registers anything, and neither may produce an unknown.
	const commonMethodNames = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}

func (q jobQueue) Handle(msg string) {}

func (s *Server) describeMux(mux *http.ServeMux) string {
	return fmt.Sprintf("%T", mux)
}

func (s *Server) tick() {
	s.queue.Handle("tick")
}
`
	got = scanRoutes(t, "harmless.go", commonMethodNames)
	if len(got.unknown) != 0 {
		t.Errorf("the scanner reported %v for a job queue with a method called Handle and a "+
			"read-only describeMux. `Handle` is an extremely common method name and a mux "+
			"parameter that is only read registers nothing; failing on either trains a maintainer "+
			"to delete this guard, and then it covers nothing at all.", got.unknown)
	}

	// ... and the narrowing cost no coverage: the SAME helper, once routes()
	// actually hands it the mux, is flagged at that first hop — which is the only
	// hop by which anything can reach the served mux.
	const handsOnTheMux = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.setupDebug(mux)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}

func (s *Server) setupDebug(mux *http.ServeMux) {
	debugreg.Register(mux)
}
`
	got = scanRoutes(t, "handson.go", handsOnTheMux)
	if len(got.unknown) == 0 {
		t.Fatal("the scanner reported no unknowns for a routes() that hands its mux to a helper " +
			"which forwards it to another package's registrar. Round 3 removed the blanket " +
			"'*Server method taking a mux' rule on the argument that pass 3 catches this at the " +
			"first hop; if it does not, that removal lost coverage.")
	}

	// And a route-pattern literal registered outside routes() still fires even
	// when the receiver is something this scanner cannot identify as a mux.
	const patternOutsideRoutes = `package control

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", s.requireToken(api))
	return mux
}

func (s *Server) late() {
	s.registry.HandleFunc("GET /debug/state", s.handleState)
}
`
	got = scanRoutes(t, "late.go", patternOutsideRoutes)
	if !containsSubstring(got.unknown, "GET /debug/state") {
		t.Errorf("the scanner's unknowns %v do not name a route-pattern registration made outside "+
			"routes() on a receiver it cannot identify. The narrowing must key on the PATTERN when "+
			"it cannot key on the receiver, or it fails open.", got.unknown)
	}

	// Negative side: the real package must come back with a mount and no
	// unknowns. Without this the scanner could simply be always-positive.
	if real := scanControlRoutes(t); !real.mounted || len(real.unknown) != 0 {
		t.Fatalf("the scanner misjudged the real package: %+v", real)
	}
}

// ---------------------------------------------------------------------------
// Guard 2b — the handler the server actually SERVES is the one routes() built
// ---------------------------------------------------------------------------

// handlerBinding is one place in this package where an http.Handler is bound to
// a server.
type handlerBinding struct {
	where    string // "control.go:New"
	expr     string // the bound expression, rendered
	isRoutes bool   // it is exactly `<recv>.routes()`
}

// scanHandlerBindings finds every place a handler is bound: the Handler field of
// an http.Server composite literal, and any assignment to a `.Handler` field.
//
// 🔴 This guard did not exist through rounds 1-3 and every route guard above was
// blind without it. Measured green at 251 PASS / 0 FAIL: leave routes() entirely
// intact, and make New serve a WRAPPER around it —
//
//	Handler: s.debugRoutes(),
//	func (s *Server) debugRoutes() http.Handler {
//	    inner := s.routes()
//	    return http.HandlerFunc(func(w, r) {
//	        if r.URL.Path == "/debug/ping" { writeJSON(w, 200, s.viewer.Snapshot()); return }
//	        inner.ServeHTTP(w, r)
//	    })
//	}
//
// — which contains no Handle and no HandleFunc anywhere, so all three route
// guards passed while `GET /debug/ping` returned a full unauthenticated
// Snapshot. Enumerating what routes() registers says nothing at all if routes()
// is not what gets served.
//
// Scope, stated so this is not read as wider than it is: it finds bindings of a
// field named Handler — the http.Server composite literal and any assignment to
// one. A server started some other way, e.g. `http.ListenAndServe(addr, h)`,
// binds no such field and is not seen. Today this package has exactly one
// binding and the guard asserts that count, so a second route in would have to
// come with a second binding it would report.
func scanHandlerBindings(t *testing.T, files []srcFile) []handlerBinding {
	t.Helper()
	var out []handlerBinding

	isRoutesCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "routes"
	}

	for _, f := range files {
		file := parseSource(t, f.name, f.src)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			where := f.name + ":" + fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if exprString(x.Type) != "http.Server" {
						return true
					}
					for _, elt := range x.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Handler" {
							continue
						}
						out = append(out, handlerBinding{
							where: where, expr: exprString(kv.Value), isRoutes: isRoutesCall(kv.Value),
						})
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Handler" || i >= len(x.Rhs) {
							continue
						}
						out = append(out, handlerBinding{
							where: where, expr: exprString(x.Rhs[i]), isRoutes: isRoutesCall(x.Rhs[i]),
						})
					}
				}
				return true
			})
		}
	}
	return out
}

func scanControlHandlerBindings(t *testing.T) []handlerBinding {
	t.Helper()
	return scanHandlerBindings(t, controlSources(t))
}

// TestTheServedHandlerIsTheOneRoutesBuilt closes the seam every other route
// guard sits on: they enumerate what routes() registers, and none of them
// checked that routes() is what the server hands to net/http.
func TestTheServedHandlerIsTheOneRoutesBuilt(t *testing.T) {
	got := scanControlHandlerBindings(t)

	// Positive control on the RESULT SET: the package HAS to bind a handler
	// somewhere or there is no server, and a scan that found none would make the
	// assertions below vacuous.
	if len(got) == 0 {
		t.Fatal("no http.Handler binding found anywhere in this package. Either the server is not " +
			"wired up at all or this scan is broken; both make its verdict meaningless.")
	}
	if len(got) != 1 {
		t.Errorf("%d handler bindings found (%+v), want exactly 1. More than one means the "+
			"handler that is actually served depends on order, and enumerating routes() no "+
			"longer tells you what answers on :8790.", len(got), got)
	}
	for _, b := range got {
		if !b.isRoutes {
			t.Errorf("%s binds the served handler to %s, not to routes(). Every other guard in "+
				"this file enumerates what routes() registers; if that is not what gets served, "+
				"they all pass while an unauthenticated endpoint answers. A wrapper needs no "+
				"Handle and no HandleFunc to add one.", b.where, b.expr)
		}
		if !strings.HasSuffix(b.where, ":New") {
			t.Errorf("the served handler is bound in %s rather than in New. New is the one "+
				"constructor callers go through, and it is where this guard looks.", b.where)
		}
	}
}

// TestServedHandlerScannerCanFire is the positive control: fed the wrapper
// mutant that shipped green, it must see it, and it must stay quiet on the
// correct shape.
func TestServedHandlerScannerCanFire(t *testing.T) {
	const wrapped = `package control

func New(cfg Config) (*Server, error) {
	s := &Server{}
	s.http = &http.Server{
		Addr:    cfg.Addr,
		Handler: s.debugRoutes(),
	}
	return s, nil
}

func (s *Server) debugRoutes() http.Handler {
	inner := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/debug/ping" {
			writeJSON(w, http.StatusOK, s.viewer.Snapshot())
			return
		}
		inner.ServeHTTP(w, r)
	})
}
`
	got := scanHandlerBindings(t, []srcFile{{name: "mutant.go", src: wrapped}})
	if len(got) != 1 {
		t.Fatalf("the scanner found %d handler bindings in the wrapper mutant, want 1: %+v", len(got), got)
	}
	if got[0].isRoutes {
		t.Errorf("the scanner accepted %s as routes(). That mutant left routes() completely "+
			"intact and served a wrapper around it, and it was measured at 251 PASS / 0 FAIL "+
			"answering `unauthenticated GET /debug/ping -> 200` with a full Snapshot.", got[0].expr)
	}

	// A late assignment is the same hazard in the other syntactic form.
	const reassigned = `package control

func New(cfg Config) (*Server, error) {
	s := &Server{}
	s.http = &http.Server{Handler: s.routes()}
	s.http.Handler = s.debugRoutes()
	return s, nil
}
`
	got = scanHandlerBindings(t, []srcFile{{name: "mutant2.go", src: reassigned}})
	if len(got) != 2 {
		t.Fatalf("the scanner found %d bindings where a composite literal is followed by an "+
			"assignment, want 2: %+v", len(got), got)
	}
	clean := 0
	for _, b := range got {
		if b.isRoutes {
			clean++
		}
	}
	if clean != 1 {
		t.Errorf("the scanner judged %d of 2 bindings to be routes(); the reassignment is the one "+
			"that actually serves and it is not routes(): %+v", clean, got)
	}

	// Negative side: the correct shape must come back clean and singular.
	const good = `package control

func New(cfg Config) (*Server, error) {
	s := &Server{}
	s.http = &http.Server{
		Addr:    cfg.Addr,
		Handler: s.routes(),
	}
	return s, nil
}
`
	got = scanHandlerBindings(t, []srcFile{{name: "good.go", src: good}})
	if len(got) != 1 || !got[0].isRoutes {
		t.Errorf("the scanner misjudged correct source: %+v", got)
	}
}

func containsSubstring(haystack []string, want string) bool {
	for _, s := range haystack {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
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
