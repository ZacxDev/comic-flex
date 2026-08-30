package control

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These are structural guards. Both pin properties that NO behavioural test can
// observe:
//
//   - Constant-time comparison. A `==` mutant answers 401/202 identically to
//     subtle.ConstantTimeCompare on every input, so status codes cannot see it.
//     The property is about TIMING, and the only mechanical check available is
//     that the call is there and the operands are not compared directly.
//   - Absence of a gotk3/glib import. A package that imported them would still
//     pass every handler test — on a machine that has GTK3. The cost only shows
//     up as a build failure somewhere else.
//
// Each guard therefore carries its own POSITIVE CONTROL: the detector is run
// over synthetic source that DOES contain the hazard, and asserted to find it.
// Without that, a detector wired to nothing would report a reassuring zero.

// ---------------------------------------------------------------------------
// Guard 1 — the token comparison is constant-time
// ---------------------------------------------------------------------------

// comparisonFindings is what inspectTokenComparison reports about one source.
type comparisonFindings struct {
	found                  bool // a func named tokenMatches exists
	callsConstantTimeCmp   bool // ... and calls subtle.ConstantTimeCompare
	comparesOperandsDirect bool // ... and/or compares its two params with ==/!=
}

// inspectTokenComparison parses src and reports what tokenMatches does.
func inspectTokenComparison(t *testing.T, filename, src string) comparisonFindings {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}

	var out comparisonFindings
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "tokenMatches" || fn.Body == nil {
			continue
		}
		out.found = true

		// The parameter names, so we can spot a direct comparison of them.
		params := map[string]bool{}
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				params[name.Name] = true
			}
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
					out.callsConstantTimeCmp = true
				}
			case *ast.BinaryExpr:
				if node.Op != token.EQL && node.Op != token.NEQ {
					return true
				}
				lhs, lok := node.X.(*ast.Ident)
				rhs, rok := node.Y.(*ast.Ident)
				if lok && rok && params[lhs.Name] && params[rhs.Name] {
					out.comparesOperandsDirect = true
				}
			}
			return true
		})
	}
	return out
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
	if !got.callsConstantTimeCmp {
		t.Error("tokenMatches does not call subtle.ConstantTimeCompare. A short-circuiting " +
			"comparison leaks a prefix timing oracle for the control token, and no status-code " +
			"test can see that.")
	}
	if got.comparesOperandsDirect {
		t.Error("tokenMatches compares its two operands directly with ==/!=. That is the " +
			"short-circuiting comparison this guard exists to forbid.")
	}
	if !importsPath(t, path, string(src), "crypto/subtle") {
		t.Error("auth.go does not import crypto/subtle")
	}
}

// TestTokenComparisonDetectorCanFire is the positive control for the guard
// above: fed source that DOES compare directly, the detector must say so. A
// green TestTokenComparisonIsConstantTime is worth nothing without this,
// because a detector that never fires reports clean on anything.
func TestTokenComparisonDetectorCanFire(t *testing.T) {
	const mutant = `package control

func tokenMatches(expected, presented string) bool {
	return expected == presented
}
`
	got := inspectTokenComparison(t, "mutant.go", mutant)
	if !got.found {
		t.Fatal("the detector did not even find tokenMatches in the mutant")
	}
	if got.callsConstantTimeCmp {
		t.Error("the detector claims the == mutant calls ConstantTimeCompare")
	}
	if !got.comparesOperandsDirect {
		t.Error("the detector did NOT flag `expected == presented` — it cannot observe the " +
			"hazard it is supposed to guard, so its clean verdict on auth.go means nothing")
	}

	// Second control: source that is CORRECT must come back clean, so the
	// detector is not simply always-positive.
	const good = `package control

import "crypto/subtle"

func tokenMatches(expected, presented string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}
`
	ok := inspectTokenComparison(t, "good.go", good)
	if !ok.callsConstantTimeCmp || ok.comparesOperandsDirect {
		t.Errorf("the detector misjudged correct source: %+v", ok)
	}
}

// ---------------------------------------------------------------------------
// Guard 2 — this package imports no GTK
// ---------------------------------------------------------------------------

// forbiddenImportSubstrings are the toolchain-bound packages internal/control
// must never reach for. Importing any of them would make this package require a
// GTK3 + X11 build environment, which is exactly what the port interface exists
// to avoid.
var forbiddenImportSubstrings = []string{"gotk3", "/glib", "/gtk", "/gdk"}

// forbiddenImports returns the import paths in src that match the ban list.
func forbiddenImports(t *testing.T, filename, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	var hits []string
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		for _, bad := range forbiddenImportSubstrings {
			if strings.Contains(path, bad) {
				hits = append(hits, path)
				break
			}
		}
	}
	return hits
}

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

func TestControlPackageImportsNoGTK(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing: %v", err)
	}
	// Positive control on the FILE SET too: a glob that matched nothing would
	// make the scan below vacuously clean.
	if len(entries) < 4 {
		t.Fatalf("only %d .go files found (%v) — the scan is not seeing the package", len(entries), entries)
	}

	scanned := 0
	for _, path := range entries {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		scanned++
		if hits := forbiddenImports(t, path, string(src)); len(hits) != 0 {
			t.Errorf("%s imports %v — internal/control must build with no GTK3/X11 toolchain, "+
				"and gotk3 cannot be cross-compiled. Put the adapter in package main instead.",
				path, hits)
		}
	}
	t.Logf("scanned %d files (including _test.go)", scanned)
}

// TestForbiddenImportDetectorCanFire is the positive control: the scanner must
// report a non-zero count on source that DOES import gotk3. Without it, the
// zero above is indistinguishable from a scanner wired to nothing.
func TestForbiddenImportDetectorCanFire(t *testing.T) {
	const mutant = `package control

import (
	"net/http"

	"github.com/gotk3/gotk3/glib"
)

var _ = http.StatusOK
var _ = glib.IdleAdd
`
	hits := forbiddenImports(t, "mutant.go", mutant)
	if len(hits) != 1 || hits[0] != "github.com/gotk3/gotk3/glib" {
		t.Fatalf("the scanner found %v on source that imports gotk3/glib — it cannot observe "+
			"the hazard, so its clean verdict on the real package means nothing", hits)
	}

	// And it must not fire on the ordinary imports this package really uses.
	const good = `package control

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
)

var _ = subtle.ConstantTimeCompare
var _ = json.Marshal
var _ = http.StatusOK
`
	if hits := forbiddenImports(t, "good.go", good); len(hits) != 0 {
		t.Fatalf("the scanner false-positived on %v", hits)
	}
}
