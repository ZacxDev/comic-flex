package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestEveryS3CallIsBounded pins that no call this program makes on the MinIO
// client waits forever: every one of them is passed a context carrying a
// deadline built in that same function, and no bare context.Background()/TODO()
// survives anywhere in package main outside a WithTimeout/WithDeadline.
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
// 🔴 ROUND-3 REBUILD — the round-2 rewrite was still narrower than its own
// sentence in three ways, all three measured green at 250 PASS / 0 FAIL:
//
//  3. It read ONE FILE, main.go. The same unbounded PurgeImage, in a new
//     store2.go of the same package, was invisible. The unit that matters is the
//     PACKAGE — the sibling route scanner was widened to the package in round 2
//     and this one was not.
//  4. It matched a client call only when the receiver rendered EXACTLY to
//     `<recv>.client`, so one alias defeated it:
//     `c := s.client; return c.RemoveObject(context.Background(), …)`.
//  5. `if len(use.clientCalls) == 0 { continue }` skipped a function ENTIRELY,
//     including its bareBackground flag — so a function whose client calls it
//     failed to RECOGNISE was treated as a function that makes none. An
//     unrecognised shape must fail, not be skipped; that is the same lesson as
//     (2), applied one level up.
//
// So the guard now (a) scans every non-test source in the package, (b)
// enumerates every function rather than a name list, (c) follows client aliases
// and *minio.Client parameters as well as the receiver field, (d) checks the
// bare-Background rule on EVERY function whether or not it recognised a client
// call there, and (e) treats a shape it cannot classify as a FAILURE.
//
// What it still cannot see, stated so nobody reads it as wider than it is: a
// client reached through an interface value, a client stored under a field name
// other than `client`, and whether a caller-supplied context.Context has a
// deadline (that case is reported, not assumed fine).
func TestEveryS3CallIsBounded(t *testing.T) {
	got := scanContextUsePackage(t)

	// Positive control on the RESULT SET. A scanner that found no methods, or no
	// client calls, would report clean on anything — including on a file it
	// failed to parse the way it expected.
	for _, method := range []string{"main.go:S3Store.ListImages", "main.go:S3Store.LoadImage"} {
		use, ok := got[method]
		if !ok {
			t.Fatalf("no %s in the scanned sources (found %v) — the scanner is not seeing this "+
				"type's methods, so its clean verdict on the others means nothing",
				method, sortedKeys(got))
		}
		if len(use.clientCalls) == 0 {
			t.Fatalf("the scanner found no MinIO client call in %s, which plainly makes "+
				"one. It cannot observe the hazard it exists to detect.", method)
		}
	}

	for _, name := range sortedKeys(got) {
		use := got[name]

		// 🔴 Checked for EVERY function, before any client-call filter. Round 2
		// checked it only for functions in which it had already recognised a
		// client call, so a call shape it did not recognise took the bare
		// Background with it.
		if use.bareBackground {
			t.Errorf("%s contains a bare context.Background()/TODO() that is not the parent of a "+
				"WithTimeout/WithDeadline. Whatever deadline the function builds, this one is "+
				"unbounded — and if it is passed to a client call this scanner did not recognise, "+
				"nothing else here will say so.", name)
		}

		if len(use.clientCalls) == 0 {
			// A function that never touches the client cannot wait forever on it.
			continue
		}
		if !use.withTimeout {
			t.Errorf("%s calls the MinIO client (%s) but builds no deadline at all. A "+
				"MinIO that accepts the connection and then goes quiet parks this goroutine "+
				"forever; for a listing that means scanning:true forever plus one leaked "+
				"goroutine and one leaked ListObjects per POST /api/rescan.",
				name, use.clientCallNames())
		}
		for _, call := range use.clientCalls {
			if call.bounded {
				continue
			}
			t.Errorf("%s passes %s to the client call %s: %s. The deadline this function "+
				"builds is not the context it uses, so the budget is inert — which is exactly "+
				"the mutant that survived round 1's version of this guard.",
				name, call.ctxExpr, call.method, call.reason)
		}
	}
}

// TestContextScannerCanFire is the positive control. It feeds the scanner the
// round-1 mutant, both round-2 survivors and all three round-3 survivors, and
// then correct source, and asserts it separates them.
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

	// 🔴 ROUND-3 SURVIVOR A, measured green at 250 PASS / 0 FAIL: the same
	// unbounded RemoveObject behind ONE alias of the receiver field. The round-2
	// scanner compared the receiver of the call against the literal string
	// "<recv>.client", so `c.RemoveObject(...)` matched nothing and the method
	// was then skipped whole by the len(clientCalls)==0 shortcut — bare
	// Background and all.
	const aliasedClient = `package main

type S3Store struct{}

func (s *S3Store) ListImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	objectCh := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{})
	_ = objectCh
	return nil, nil
}

func (s *S3Store) PurgeImage(key string) error {
	c := s.client
	return c.RemoveObject(context.Background(), s.bucket, key, minio.RemoveObjectOptions{})
}
`
	got = scanContextUseSource(t, "mutant7.go", aliasedClient)
	purge, ok = got["PurgeImage"]
	if !ok {
		t.Fatal("the scanner did not report the aliased-client PurgeImage at all")
	}
	if len(purge.clientCalls) != 1 || purge.clientCalls[0].bounded {
		t.Errorf("the scanner reported %+v for `c := s.client; c.RemoveObject(context.Background(), "+
			"…)`. One alias of the receiver field is an ordinary refactor and it shipped an "+
			"unbounded MinIO call at a fully green suite.", purge.clientCalls)
	}
	if !purge.bareBackground {
		t.Error("the scanner did NOT flag the aliased method's bare context.Background() — the " +
			"round-2 version skipped the whole method because it recognised no client call in it")
	}

	// 🔴 ROUND-3 SURVIVOR B: the same call in a NON-METHOD helper. The round-2
	// scanner required fn.Recv != nil and skipped every plain function.
	const nonMethodHelper = `package main

type S3Store struct{}

func (s *S3Store) ListImages() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	objectCh := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{})
	_ = objectCh
	return nil, nil
}

func purgeAll(c *minio.Client, bucket, key string) error {
	return c.RemoveObject(context.Background(), bucket, key, minio.RemoveObjectOptions{})
}
`
	got = scanContextUseSource(t, "mutant8.go", nonMethodHelper)
	purgeFn, ok := got["purgeAll"]
	if !ok {
		t.Fatal("the scanner did not report the non-method helper purgeAll — it still requires a " +
			"receiver, so any free function taking a *minio.Client is unguarded")
	}
	if len(purgeFn.clientCalls) != 1 || purgeFn.clientCalls[0].bounded {
		t.Errorf("the scanner reported %+v for an unbounded RemoveObject on a *minio.Client "+
			"parameter", purgeFn.clientCalls)
	}

	// 🔴 ROUND-3 SURVIVOR C: a SECOND FILE of the same package. The scanner read
	// main.go only, so moving the method out of it hid it completely.
	got = scanContextUseFiles(t, []goSource{
		{name: "main.go", src: newUnboundedMethod[:strings.Index(newUnboundedMethod, "func (s *S3Store) PurgeImage")]},
		{name: "store2.go", src: "package main\n\n" +
			"func (s *S3Store) PurgeImage(key string) error {\n" +
			"\treturn s.client.RemoveObject(context.Background(), s.bucket, key, minio.RemoveObjectOptions{})\n}\n"},
	}, func(rec s3Record) string { return rec.file + ":" + rec.name })
	second, ok := got["store2.go:S3Store.PurgeImage"]
	if !ok {
		t.Fatalf("the scanner did not report a method declared in a second file of the same "+
			"package (found %v). The package is what gets built and what serves; reading one "+
			"file is a guard that any `git mv` walks past.", sortedKeys(got))
	}
	if len(second.clientCalls) != 1 || second.clientCalls[0].bounded {
		t.Errorf("the scanner reported %+v for the second file's unbounded call", second.clientCalls)
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
	// simply always-positive and its verdict on the package is worthless.
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

func hexish(v int) string { return "x" }
`
	got = scanContextUseSource(t, "good.go", good)
	for _, name := range []string{"ListImages", "LoadImage"} {
		use := got[name]
		if !use.withTimeout || use.bareBackground || len(use.clientCalls) != 1 || !use.clientCalls[0].bounded {
			t.Errorf("the scanner misjudged correctly bounded %s: %+v", name, use)
		}
	}
	if use, ok := got["bucketName"]; !ok || len(use.clientCalls) != 0 || use.bareBackground {
		t.Errorf("the scanner reported client calls or a bare Background in a method that has "+
			"neither: %+v", use)
	}
	if use, ok := got["hexish"]; !ok || len(use.clientCalls) != 0 || use.bareBackground {
		t.Errorf("the scanner misjudged an ordinary free function: %+v", use)
	}
}

// clientCall is one call this function makes on the MinIO client.
type clientCall struct {
	method  string // the client method, e.g. "ListObjects"
	ctxExpr string // the first argument, rendered
	bounded bool   // that argument is a context this function derived with a deadline
	reason  string // why not, when !bounded
}

// contextUse is what the scanner reports about one function.
type contextUse struct {
	withTimeout bool // calls context.WithTimeout or context.WithDeadline
	// bareBackground marks a context.Background()/TODO() that is NOT the
	// argument of a WithTimeout/WithDeadline — i.e. one that gets USED.
	bareBackground bool
	clientCalls    []clientCall
}

// s3Record is one scanned function, with the file it came from.
type s3Record struct {
	file string
	name string // "S3Store.PurgeImage" for a method, "purgeAll" for a free function
	use  contextUse
}

// goSource is one parsed-from-memory source file, so the real scan can cover the
// whole package while the positive controls feed it synthetic files.
type goSource struct {
	name string
	src  string
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

// scanContextUsePackage scans EVERY non-test source in package main.
//
// 🔴 Reading main.go alone was round 3's finding 2: the identical unbounded
// method, moved to a store2.go of the same package, was invisible to a test
// named TestEveryS3CallIsBounded. The unit that gets built and that talks to
// MinIO is the package.
func scanContextUsePackage(t *testing.T) map[string]contextUse {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var files []goSource
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		files = append(files, goSource{name: name, src: string(src)})
	}
	// Positive control on the INPUT SET: an empty or truncated file list would
	// make every check above vacuously clean.
	if len(files) < 3 {
		t.Fatalf("only %d non-test sources found in package main (%v) — the scanner is not "+
			"looking at the package it claims to check", len(files), files)
	}
	var haveMain bool
	for _, f := range files {
		if f.name == "main.go" {
			haveMain = true
		}
	}
	if !haveMain {
		t.Fatal("main.go was not among the scanned sources")
	}
	return scanContextUseFiles(t, files, func(rec s3Record) string { return rec.file + ":" + rec.name })
}

// scanContextUseSource scans one source and keys the result by the plain
// function name, which is what the single-file positive controls above assert
// on.
func scanContextUseSource(t *testing.T, filename, src string) map[string]contextUse {
	t.Helper()
	return scanContextUseFiles(t, []goSource{{name: filename, src: src}}, func(rec s3Record) string {
		if i := strings.LastIndex(rec.name, "."); i >= 0 {
			return rec.name[i+1:]
		}
		return rec.name
	})
}

// scanContextUseFiles reports, for EVERY function and method in the given
// sources, how its context is made and which context actually reaches each
// MinIO client call.
func scanContextUseFiles(t *testing.T, files []goSource, key func(s3Record) string) map[string]contextUse {
	t.Helper()
	out := map[string]contextUse{}
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.name, f.src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f.name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			rec := s3Record{file: f.name, name: funcDisplayName(fn), use: inspectContextUse(fn)}
			k := key(rec)
			if prev, clash := out[k]; clash {
				t.Fatalf("two scanned functions share the key %q (%+v and %+v) — the scan would "+
					"silently drop one of them", k, prev, rec.use)
			}
			out[k] = rec.use
		}
	}
	return out
}

// funcDisplayName renders "S3Store.PurgeImage" for a method and "purgeAll" for a
// free function.
func funcDisplayName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return fn.Name.Name
	}
	recvType := fn.Recv.List[0].Type
	if star, ok := recvType.(*ast.StarExpr); ok {
		recvType = star.X
	}
	if id, ok := recvType.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return renderExpr(recvType) + "." + fn.Name.Name
}

// inspectContextUse is the whole detector for one function.
func inspectContextUse(fn *ast.FuncDecl) contextUse {
	var use contextUse

	// (1) Which identifiers hold a context this function gave a deadline to, and
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
	clients := clientExpressions(fn)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !clients[renderExpr(sel.X)] {
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
				cc.reason = "it is not a context this function derived with WithTimeout/WithDeadline"
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

// clientExpressions reports every expression inside fn that holds the MinIO
// client, rendered.
//
// 🔴 Round 2 had this as a single string, `<receiver>.client`, so one alias hid
// an unbounded call. Three sources now count, and aliases of any of them are
// followed to a fixpoint:
//
//   - any selector ending in `.client` — the receiver field, and the same field
//     on any other type that holds one,
//   - any parameter declared *minio.Client,
//   - any local assigned from one of those, however many hops away.
func clientExpressions(fn *ast.FuncDecl) map[string]bool {
	clients := map[string]bool{}

	// Selectors ending in .client, wherever they appear.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "client" {
			clients[renderExpr(sel)] = true
		}
		return true
	})

	// Parameters typed *minio.Client.
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if renderExpr(field.Type) != "*minio.Client" {
				continue
			}
			for _, name := range field.Names {
				clients[name.Name] = true
			}
		}
	}

	// Aliases, to a fixpoint: `c := s.client`, then `d := c`, …
	for changed := true; changed; {
		changed = false
		record := func(lhs, rhs ast.Expr) {
			if !clients[renderExpr(rhs)] {
				return
			}
			name := renderExpr(lhs)
			if name == "_" || clients[name] {
				return
			}
			clients[name] = true
			changed = true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					record(x.Lhs[0], x.Rhs[0])
				}
			case *ast.ValueSpec:
				if len(x.Names) == 1 && len(x.Values) == 1 {
					record(x.Names[0], x.Values[0])
				}
			}
			return true
		})
	}
	return clients
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
