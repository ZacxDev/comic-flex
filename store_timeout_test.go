package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestEveryS3CallIsBounded pins that no S3Store method waits forever.
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
// IS mechanically checkable is that the deadline exists at every call site, and
// that is the thing that was missing. It is paired with a positive control below
// so a detector wired to nothing cannot report clean.
func TestEveryS3CallIsBounded(t *testing.T) {
	got := scanContextUse(t, "main.go")

	for _, method := range []string{"ListImages", "LoadImage"} {
		use, ok := got[method]
		if !ok {
			t.Fatalf("no S3Store method named %s in main.go — this guard now pins nothing", method)
		}
		if !use.withTimeout {
			t.Errorf("S3Store.%s builds its context without context.WithTimeout. A MinIO that "+
				"accepts the connection and then goes quiet parks this goroutine forever; for "+
				"ListImages that means scanning:true forever and one leaked goroutine plus one "+
				"leaked ListObjects per POST /api/rescan.", method)
		}
		if use.bareBackground {
			t.Errorf("S3Store.%s passes a bare context.Background() to a client call — the "+
				"deadline it built is not the context it uses.", method)
		}
	}
}

// TestContextScannerCanFire is the positive control: fed a method that DOES use
// a bare Background(), the scanner must say so, and fed one that does not, it
// must come back clean.
func TestContextScannerCanFire(t *testing.T) {
	const mutant = `package main

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
	got := scanContextUseSource(t, "mutant.go", mutant)

	list, ok := got["ListImages"]
	if !ok {
		t.Fatal("the scanner did not find ListImages in the mutant")
	}
	if list.withTimeout {
		t.Error("the scanner claims the bare-Background mutant uses context.WithTimeout")
	}
	if !list.bareBackground {
		t.Error("the scanner did NOT flag `ctx := context.Background()` — it cannot observe the " +
			"hazard, so its clean verdict on main.go means nothing")
	}

	load, ok := got["LoadImage"]
	if !ok {
		t.Fatal("the scanner did not find LoadImage in the mutant")
	}
	if !load.withTimeout || load.bareBackground {
		t.Errorf("the scanner misjudged a correctly bounded method: %+v", load)
	}
}

// contextUse is what the scanner reports about one method.
type contextUse struct {
	withTimeout    bool // calls context.WithTimeout or context.WithDeadline
	bareBackground bool // assigns context.Background() straight to a variable
}

func scanContextUse(t *testing.T, path string) map[string]contextUse {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return scanContextUseSource(t, path, string(src))
}

// scanContextUseSource reports, per method on *S3Store, how its context is made.
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
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); !ok || id.Name != "S3Store" {
			continue
		}

		var use contextUse
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
			if sel.Sel.Name == "WithTimeout" || sel.Sel.Name == "WithDeadline" {
				use.withTimeout = true
			}
			return true
		})
		// A bare Background() assigned to a variable is one that gets USED as
		// the request context; the Background() nested inside WithTimeout's
		// argument list is not.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Rhs) != 1 {
				return true
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Background" && sel.Sel.Name != "TODO" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" {
				use.bareBackground = true
			}
			return true
		})
		out[fn.Name.Name] = use
	}
	return out
}
