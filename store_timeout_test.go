package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"testing"
)

// TestEveryS3CallIsBounded pins that no S3Store method waits forever: every call
// this type makes on the MinIO client is passed a context carrying a deadline
// built in that same method.
//
// 🔴 ListImages used a bare context.Background(), unlike LoadImage's 30 s. A
// MinIO that accepted the connection and then went quiet parked the scanning
// goroutine FOREVER: `scanning` never cleared, the display never got its first
// image, and once POST /api/rescan exists every retry leaked another goroutine
// and another concurrent ListObjects. The API made a pre-existing hang
// network-triggerable.
//
// This is a STRUCTURAL guard and says so. Proving the timeout behaviourally
// needs a MinIO that accepts and then stalls, which is not available here; what
// IS mechanically checkable is that a deadline reaches every client call, and
// that is the thing that was missing.
//
// 🔴 ROUND-2 REBUILD — the round-1 version's docstring claimed exactly this
// sentence while the body checked two narrower things, and two mutants SURVIVED
// the full suite at 243 PASS / 0 FAIL under this very test's name:
//
//  1. It looped a HARDCODED []string{"ListImages","LoadImage"}. Adding an
//     unbounded `func (s *S3Store) PurgeImage(key string) error` that called
//     `RemoveObject(context.Background(), …)` was green — a test named
//     "EveryS3CallIsBounded" that only ever looked at two of them.
//  2. It set withTimeout if `context.WithTimeout` appeared ANYWHERE in the
//     method and never checked which context reached the client, and its
//     bareBackground check inspected only AssignStmt. So building the 2 minute
//     budget, dropping it with `_ = ctx`, and calling
//     `ListObjects(context.Background(), …)` was green: the deadline existed and
//     was inert.
//
// So the guard now (a) enumerates the methods instead of naming them, (b)
// follows the context expression to each client call, and (c) treats a shape it
// cannot classify as a FAILURE rather than as an absence of one.
func TestEveryS3CallIsBounded(t *testing.T) {
	got := scanContextUse(t, "main.go")

	// Positive control on the RESULT SET. A scanner that found no methods, or no
	// client calls, would report clean on anything — including on a file it
	// failed to parse the way it expected.
	for _, method := range []string{"ListImages", "LoadImage"} {
		use, ok := got[method]
		if !ok {
			t.Fatalf("no S3Store method named %s in main.go — the scanner is not seeing this "+
				"type's methods, so its clean verdict on the others means nothing", method)
		}
		if len(use.clientCalls) == 0 {
			t.Fatalf("the scanner found no MinIO client call in S3Store.%s, which plainly makes "+
				"one. It cannot observe the hazard it exists to detect.", method)
		}
	}

	for _, method := range sortedKeys(got) {
		use := got[method]
		if len(use.clientCalls) == 0 {
			// A method that never touches the client cannot wait forever on it.
			continue
		}
		if !use.withTimeout {
			t.Errorf("S3Store.%s calls the MinIO client (%s) but builds no deadline at all. A "+
				"MinIO that accepts the connection and then goes quiet parks this goroutine "+
				"forever; for a listing that means scanning:true forever plus one leaked "+
				"goroutine and one leaked ListObjects per POST /api/rescan.",
				method, use.clientCallNames())
		}
		if use.bareBackground {
			t.Errorf("S3Store.%s contains a bare context.Background()/TODO() that is not the "+
				"parent of a WithTimeout/WithDeadline. Whatever deadline the method builds, this "+
				"one is unbounded.", method)
		}
		for _, call := range use.clientCalls {
			if call.bounded {
				continue
			}
			t.Errorf("S3Store.%s passes %s to the client call %s: %s. The deadline this method "+
				"builds is not the context it uses, so the budget is inert — which is exactly "+
				"the mutant that survived round 1's version of this guard.",
				method, call.ctxExpr, call.method, call.reason)
		}
	}
}

// TestContextScannerCanFire is the positive control. It feeds the scanner the
// round-1 mutant AND both round-2 survivors, and then correct source, and
// asserts it separates them.
func TestContextScannerCanFire(t *testing.T) {
	// ROUND-1 MUTANT: a bare Background assigned to the request context.
	const bareAssign = `package main

type S3Store struct{}

func (s *S3Store) ListImages() ([]string, error) {
	ctx := context.Background()
	objectCh := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{})
	_ = objectCh
	return nil, nil
}

func (s *S3Store) LoadImage(key string) (*gdk.Pixbuf, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	return nil, nil
}
`
	got := scanContextUseSource(t, "mutant1.go", bareAssign)
	list, ok := got["ListImages"]
	if !ok {
		t.Fatal("the scanner did not find ListImages in the mutant")
	}
	if list.withTimeout {
		t.Error("the scanner claims the bare-Background mutant uses context.WithTimeout")
	}
	if !list.bareBackground {
		t.Error("the scanner did NOT flag `ctx := context.Background()`")
	}
	if len(list.clientCalls) != 1 || list.clientCalls[0].bounded {
		t.Errorf("the scanner reported client calls %+v; the one call is passed an unbounded "+
			"context and must be reported as such", list.clientCalls)
	}
	load, ok := got["LoadImage"]
	if !ok {
		t.Fatal("the scanner did not find LoadImage in the mutant")
	}
	if !load.withTimeout || load.bareBackground {
		t.Errorf("the scanner misjudged a correctly bounded method: %+v", load)
	}
	if len(load.clientCalls) != 1 || !load.clientCalls[0].bounded {
		t.Errorf("the scanner reported %+v for a correctly bounded call", load.clientCalls)
	}

	// 🔴 ROUND-2 SURVIVOR A, measured green at 243 PASS / 0 FAIL: the deadline
	// is built and then not used. `withTimeout` is true, the AssignStmt-only
	// bareBackground check sees nothing, and the 2 minute budget is inert.
	const inertTimeout = `package main

type S3Store struct{}

func (s *S3Store) ListImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	_ = ctx
	objectCh := s.client.ListObjects(context.Background(), s.bucket, minio.ListObjectsOptions{})
	_ = objectCh
	return nil, nil
}
`
	got = scanContextUseSource(t, "mutant2.go", inertTimeout)
	list, ok = got["ListImages"]
	if !ok {
		t.Fatal("the scanner did not find ListImages in the inert-timeout mutant")
	}
	if !list.bareBackground {
		t.Error("the scanner did NOT flag an INLINE context.Background() argument. Its round-1 " +
			"version only inspected AssignStmt, so this shape was invisible and shipped green.")
	}
	if len(list.clientCalls) != 1 {
		t.Fatalf("the scanner found %d client calls in the inert-timeout mutant, want 1: %+v",
			len(list.clientCalls), list.clientCalls)
	}
	if list.clientCalls[0].bounded {
		t.Error("the scanner accepted `ListObjects(context.Background(), …)` as bounded because " +
			"a WithTimeout appeared elsewhere in the method. That is the whole mutant: the " +
			"deadline exists and does not reach the call.")
	}

	// 🔴 ROUND-2 SURVIVOR B, also green at 243/0: a NEW unbounded method. The
	// round-1 guard iterated a hardcoded two-name list, under a test called
	// TestEveryS3CallIsBounded.
	const newUnboundedMethod = `package main

type S3Store struct{}

func (s *S3Store) ListImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	objectCh := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{})
	_ = objectCh
	return nil, nil
}

func (s *S3Store) PurgeImage(key string) error {
	return s.client.RemoveObject(context.Background(), s.bucket, key, minio.RemoveObjectOptions{})
}
`
	got = scanContextUseSource(t, "mutant3.go", newUnboundedMethod)
	purge, ok := got["PurgeImage"]
	if !ok {
		t.Fatal("the scanner did not report PurgeImage at all — it is still enumerating a " +
			"hardcoded list of method names, and any method added tomorrow is unguarded")
	}
	if purge.withTimeout {
		t.Error("the scanner claims PurgeImage builds a deadline")
	}
	if len(purge.clientCalls) != 1 || purge.clientCalls[0].bounded {
		t.Errorf("the scanner reported %+v for an unbounded RemoveObject", purge.clientCalls)
	}
	if !purge.bareBackground {
		t.Error("the scanner did NOT flag PurgeImage's inline context.Background()")
	}

	// A context that arrives as a parameter cannot be verified here, and must be
	// reported rather than assumed fine — an unrecognised shape fails closed.
	const inheritedContext = `package main

type S3Store struct{}

func (s *S3Store) LoadImage(ctx context.Context, key string) (*gdk.Pixbuf, error) {
	_, _ = s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	return nil, nil
}
`
	got = scanContextUseSource(t, "mutant4.go", inheritedContext)
	load, ok = got["LoadImage"]
	if !ok {
		t.Fatal("the scanner did not find the inherited-context LoadImage")
	}
	if len(load.clientCalls) != 1 || load.clientCalls[0].bounded {
		t.Errorf("the scanner accepted a caller-supplied context as bounded (%+v). It cannot see "+
			"the caller, so it cannot know; treating it as fine is how a guard reports coverage "+
			"it does not have.", load.clientCalls)
	}

	// Negative side: correct source must come back clean, or the scanner is
	// simply always-positive and its verdict on main.go is worthless.
	const good = `package main

type S3Store struct{}

func (s *S3Store) ListImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	objectCh := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{})
	_ = objectCh
	return nil, nil
}

func (s *S3Store) LoadImage(key string) (*gdk.Pixbuf, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	return nil, nil
}

func (s *S3Store) bucketName() string { return s.bucket }
`
	got = scanContextUseSource(t, "good.go", good)
	for _, name := range []string{"ListImages", "LoadImage"} {
		use := got[name]
		if !use.withTimeout || use.bareBackground || len(use.clientCalls) != 1 || !use.clientCalls[0].bounded {
			t.Errorf("the scanner misjudged correctly bounded %s: %+v", name, use)
		}
	}
	if use, ok := got["bucketName"]; !ok || len(use.clientCalls) != 0 {
		t.Errorf("the scanner reported client calls in a method that makes none: %+v", use)
	}
}

// clientCall is one call this S3Store method makes on the MinIO client.
type clientCall struct {
	method  string // the client method, e.g. "ListObjects"
	ctxExpr string // the first argument, rendered
	bounded bool   // that argument is a context this method derived with a deadline
	reason  string // why not, when !bounded
}

// contextUse is what the scanner reports about one method.
type contextUse struct {
	withTimeout bool // calls context.WithTimeout or context.WithDeadline
	// bareBackground marks a context.Background()/TODO() that is NOT the
	// argument of a WithTimeout/WithDeadline — i.e. one that gets USED.
	bareBackground bool
	clientCalls    []clientCall
}

func (u contextUse) clientCallNames() string {
	names := make([]string, 0, len(u.clientCalls))
	for _, c := range u.clientCalls {
		names = append(names, c.method)
	}
	return fmt.Sprint(names)
}

func sortedKeys(m map[string]contextUse) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scanContextUse(t *testing.T, path string) map[string]contextUse {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return scanContextUseSource(t, path, string(src))
}

// scanContextUseSource reports, for EVERY method on *S3Store, how its context is
// made and which context actually reaches each MinIO client call.
func scanContextUseSource(t *testing.T, filename, src string) map[string]contextUse {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}

	out := map[string]contextUse{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recvType := fn.Recv.List[0].Type
		if star, ok := recvType.(*ast.StarExpr); ok {
			recvType = star.X
		}
		if id, ok := recvType.(*ast.Ident); !ok || id.Name != "S3Store" {
			continue
		}
		out[fn.Name.Name] = inspectS3Method(fn)
	}
	return out
}

// inspectS3Method is the whole detector for one method.
func inspectS3Method(fn *ast.FuncDecl) contextUse {
	var use contextUse

	// (1) Which identifiers hold a context this method gave a deadline to, and
	// which Background()/TODO() calls are merely the PARENT of such a deadline.
	timeoutVars := map[string]bool{}
	nested := map[*ast.CallExpr]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "context" {
			return true
		}
		if sel.Sel.Name != "WithTimeout" && sel.Sel.Name != "WithDeadline" {
			return true
		}
		use.withTimeout = true
		for _, arg := range call.Args {
			if inner, ok := arg.(*ast.CallExpr); ok {
				nested[inner] = true
			}
		}
		return true
	})
	// The variable a WithTimeout/WithDeadline was assigned to.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "WithTimeout" && sel.Sel.Name != "WithDeadline") {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "context" {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			timeoutVars[id.Name] = true
		}
		return true
	})

	// (2) A Background()/TODO() that is NOT wrapped in a deadline is one that
	// gets used, wherever it appears — as an assignment OR inline as an
	// argument. Round 1 inspected only the assignment form.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || nested[call] {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Background" && sel.Sel.Name != "TODO") {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" {
			use.bareBackground = true
		}
		return true
	})

	// (3) Which context reaches each MinIO client call.
	params := map[string]bool{}
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				params[name.Name] = true
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || renderExpr(sel.X) != receiverField(fn, "client") {
			return true
		}
		cc := clientCall{method: sel.Sel.Name, ctxExpr: renderExpr(call.Args[0])}
		switch arg := call.Args[0].(type) {
		case *ast.Ident:
			switch {
			case timeoutVars[arg.Name]:
				cc.bounded = true
			case params[arg.Name]:
				cc.reason = "it is a parameter, so the deadline (if any) is the CALLER's and this " +
					"guard cannot see it — bound it here, or widen the guard to check every caller"
			default:
				cc.reason = "it is not a context this method derived with WithTimeout/WithDeadline"
			}
		case *ast.CallExpr:
			cc.reason = "it is a call expression, not a context carrying a deadline built here"
		default:
			cc.reason = "this guard cannot classify that expression, and an unrecognised shape " +
				"must fail rather than be skipped"
		}
		use.clientCalls = append(use.clientCalls, cc)
		return true
	})

	return use
}

// receiverField renders `<receiver>.<field>` for the method's own receiver, so
// the detector follows a renamed receiver rather than assuming "s".
func receiverField(fn *ast.FuncDecl, field string) string {
	name := "s"
	if fn.Recv != nil && len(fn.Recv.List) == 1 && len(fn.Recv.List[0].Names) == 1 {
		name = fn.Recv.List[0].Names[0].Name
	}
	return name + "." + field
}

// renderExpr renders an expression for matching and for failure messages.
func renderExpr(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return renderExpr(x.X) + "." + x.Sel.Name
	case *ast.ParenExpr:
		return "(" + renderExpr(x.X) + ")"
	case *ast.StarExpr:
		return "*" + renderExpr(x.X)
	case *ast.CallExpr:
		return renderExpr(x.Fun) + "(...)"
	}
	return fmt.Sprintf("<%T>", e)
}
