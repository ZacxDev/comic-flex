package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// This file guards ONE invariant, and it is an invariant this PR CREATED.
//
// 🔴 Rescan is the only Viewer write that runs on the HTTP handler goroutine
// rather than inside an Enqueue closure. Round 2 made it so, deliberately — the
// admission answer has to be given while the caller is still there to be told
// 503 — and three separate comments now assert that nothing it reaches touches a
// widget (control_adapter.go's gtkViewer doc and Rescan doc, scanImagesAsync's
// header, and internal/control/viewer.go's Viewer.Rescan contract).
//
// Nothing failed if that stopped being true. Measured at 99a9dca, this mutant
// was 250 PASS / 0 FAIL / 0 races UNDER -race:
//
//	func (g gtkViewer) Rescan() bool {
//	    if !g.iv.scanImagesAsync() { return false }
//	    g.iv.updateImage()   // window.GetSize(), image.SetFromPixbuf()
//	    return true
//	}
//
// It is the natural mistake by symmetry: Next, Prev, GotoKey and GotoIndex all
// call updateImage() after their state change, and a maintainer adding it here
// is doing what the four neighbours do. It survives because every test viewer's
// LoadImage returns an error, so updateSingleImage logs and returns BEFORE the
// widget calls — no fixture in the suite ever reaches them, and -race sees
// nothing because there is no second goroutine touching the same widget in a
// test. On the Pi it is a GTK call from a non-main thread.
//
// A behavioural test cannot close this: it would need a real GTK window, a real
// X display and a real image, i.e. the Pi. So it is closed structurally, by
// asking which functions can REACH a widget through the package's call graph.

// widgetFields are the two gtk widget handles ImageViewer owns. A call on either
// is a GTK call and must happen on the GTK main thread.
//
// 🔴 It is a SPELLED list, and a spelled list silently narrows: add a third
// widget handle to ImageViewer and every call on it becomes invisible to this
// whole file, with no test going red. It is not derived from the struct because
// every fixture in TestThreadAffinityDetectorCanFire declares
// `type ImageViewer struct{}` with no fields at all, so a derived list would be
// EMPTY for them and the positive controls would stop controlling anything.
// TestWidgetFieldsIsTheWholeSetOfWidgetHandles is the ledger instead: it reads
// the REAL struct and fails if the two sets ever disagree, in either direction.
var widgetFields = []string{"window", "image"}

// TestWidgetFieldsIsTheWholeSetOfWidgetHandles asserts the ledger above against
// the struct it claims to enumerate.
//
// It fails when the set GROWS (a third gtk handle nothing in this file can see)
// and when it SHRINKS (a name in the list that no longer exists, which would
// make widgetCall match nothing while still looking like it matches something).
func TestWidgetFieldsIsTheWholeSetOfWidgetHandles(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "ImageViewer" {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			star, ok := field.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "gtk" {
				continue
			}
			for _, name := range field.Names {
				found = append(found, name.Name)
			}
		}
		return false
	})

	if len(found) == 0 {
		t.Fatal("no *gtk.* fields found on ImageViewer in main.go — this ledger is reading the " +
			"wrong thing, so its agreement with widgetFields below means nothing")
	}
	sort.Strings(found)
	want := append([]string(nil), widgetFields...)
	sort.Strings(want)
	if !equalStringSlices(found, want) {
		t.Errorf("ImageViewer's gtk widget handles are %v but widgetFields says %v. Every guard in "+
			"this file matches widget calls by these names, so a handle missing from the list is a "+
			"GTK call nothing here can see, and a name in the list that is not a handle is a "+
			"matcher that quietly matches nothing.", found, want)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// threadAffinity is the package's call graph, annotated with which functions
// touch a widget directly.
type threadAffinity struct {
	funcs  map[string]*ast.FuncDecl // display name -> declaration
	direct map[string]string        // display name -> the widget call it makes
	edges  map[string][]string      // display name -> callees
}

// reaches reports whether from can reach a widget call, and by what path.
//
// seen doubles as the parent map the path is rebuilt from; the queue carries
// names only. It used to carry a `prev` field as well, written on every push and
// read nowhere — two records of the same edge, one of which could go stale
// without anything noticing.
func (g *threadAffinity) reaches(from string) ([]string, bool) {
	seen := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if touch, ok := g.direct[cur]; ok {
			// Rebuild the path back to `from`.
			var path []string
			for at := cur; at != ""; at = seen[at] {
				path = append([]string{at}, path...)
			}
			return append(path, touch), true
		}
		for _, next := range g.edges[cur] {
			if _, been := seen[next]; been {
				continue
			}
			seen[next] = cur
			queue = append(queue, next)
		}
	}
	return nil, false
}

func (g *threadAffinity) names() []string {
	out := make([]string, 0, len(g.funcs))
	for k := range g.funcs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestNothingOffTheGTKThreadCanReachAWidget is the guard.
//
// It states its own limits rather than claiming more than it checks. Callee
// resolution is by NAME, not by type — a method call X.M(...) is treated as an
// edge to EVERY method named M in this package. That over-approximates the graph,
// which is the fail-closed direction: it can report a path that types would rule
// out, never miss one that types would allow.
//
// 🔴 WHAT IT CANNOT SEE. The round-3 version of this paragraph named two things:
// a func value stored in a STRUCT FIELD, and a package this scan does not read.
// Round 4 measured four escapes, and NONE of them was on that list — a list of
// blind spots that omits the blind spots is worse than no list, because it reads
// as coverage. The measured set, at d025210:
//
//	g.iv.onScanComplete(g.iv.updateImage)  func value as a PARAMETER  reaches=false
//	w := iv.window; w.GetSize()            receiver ALIAS             reaches=false
//	scanImagesAsyncVia(runInline)          inline scheduler           reaches=false
//	gdk.ScreenGetDefault()                 package gdk                reaches=false
//
// The first is CLOSED in round 4: a function handed to a call as a VALUE is now
// an edge to it, unless the call hands its arguments to the main loop (see
// handsToMainLoop). It was the serious one — `onScanComplete(update func())` is
// the idiom scanImagesAsyncVia uses two lines from the guarded call.
//
// The other three are OPEN and are stated as open:
//
//   - a receiver ALIAS. widgetCall matches on the rendered receiver, so
//     `w := iv.window` renames the widget out of its view. Closing it needs the
//     alias fixpoint that internal/control's mux scanner has.
//   - a scheduler parameter whose ARGUMENT is not a scheduler. schedulerParams
//     exempts by the parameter's declared type func(func()) and never resolves
//     what the caller passed, so `scanImagesAsyncVia(runInline)` is exempted on a
//     type. This is the claim the 🔴 comment in TestThreadAffinityDetectorCanFire
//     used to make and did not hold; it is corrected there.
//   - package gdk. widgetCall treats a call into package gtk as a widget call and
//     gdk as ordinary. That is deliberate, not an oversight to fix by adding gdk:
//     gdk-pixbuf loading OFF the main thread is correct and is what the image
//     path does, so flagging every gdk call would cry wolf on the code as
//     written.
//
// The real seams the last two would sit on are covered from the other side by
// TestScanImagesAsyncSchedulesThroughTheGTKMainLoop and
// TestTheSignalHandlerSchedulesThroughScheduleQuit, which assert through
// scanSchedulerCalls that the production entry points hand over idleOnce /
// scheduleQuit specifically rather than some other func(func()). That is what
// makes "nothing is open today" a measurement rather than a hope — and it is a
// SEPARATE guard, so deleting it reopens these.
func TestNothingOffTheGTKThreadCanReachAWidget(t *testing.T) {
	g := scanThreadAffinity(t, packageSources(t))

	// Positive control on the RESULT SET, from REAL data rather than a fixture:
	// the R1 writes and the render chain genuinely DO reach a widget. A graph
	// that reported otherwise is wired to nothing, and its clean verdict on
	// Rescan would mean nothing.
	for _, must := range []string{
		"gtkViewer.Next", "gtkViewer.Prev", "gtkViewer.GotoKey", "gtkViewer.GotoIndex",
		"gtkViewer.SetViewMode", "ImageViewer.updateImage", "ImageViewer.updateSingleImage",
	} {
		if _, ok := g.funcs[must]; !ok {
			t.Fatalf("no %s in the scanned package (found %v) — this guard is not looking at the "+
				"code it claims to check", must, g.names())
		}
		if _, ok := g.reaches(must); !ok {
			t.Fatalf("%s does NOT reach a widget call according to this graph, and it plainly "+
				"does (updateSingleImage calls image.SetFromPixbuf, and reaches window.GetSize "+
				"via iv.layoutBox). The detector cannot observe the hazard, so its clean verdict "+
				"below is worthless.", must)
		}
	}

	// The actual invariant: everything that runs OFF the GTK main thread.
	offThread := []string{
		"gtkViewer.Rescan",            // called synchronously by POST /api/rescan
		"ImageViewer.scanImagesAsync", // and everything it does outside its scheduled closure
		"ImageViewer.scanImagesAsyncVia",
		"gtkViewer.Snapshot", // R2 read, on the handler goroutine
		"gtkViewer.Resolve",  // R2 read, on the handler goroutine
		"gtkViewer.Enqueue",  // the bridge itself; the closure it schedules may render
	}
	for _, name := range offThread {
		if _, ok := g.funcs[name]; !ok {
			t.Fatalf("no %s in the scanned package — this guard now pins nothing for it", name)
		}
		if path, ok := g.reaches(name); ok {
			t.Errorf("%s can reach a GTK widget call, and it runs on the HTTP handler goroutine, "+
				"not the GTK main thread: %s. GTK is not thread safe; on the Pi this is a widget "+
				"call from a non-main thread. Nothing in the suite can see it, because every test "+
				"viewer's LoadImage errors out before the widget calls are reached. Put the render "+
				"inside the idleOnce closure instead.",
				name, strings.Join(path, " -> "))
		}
	}
}

// TestThreadAffinityDetectorCanFire is the synthetic positive control: fed the
// exact mutant that was measured green at 250 PASS / 0 FAIL / 0 races, it must
// see it — and it must stay quiet on the shape that is actually in the tree.
func TestThreadAffinityDetectorCanFire(t *testing.T) {
	const mutant = `package main

type ImageViewer struct{}
type gtkViewer struct{ iv *ImageViewer }

func idleOnce(fn func()) { glib.IdleAdd(func() bool { fn(); return false }) }

func (iv *ImageViewer) updateImage()       { iv.updateSingleImage() }
func (iv *ImageViewer) updateSingleImage() { iv.window.GetSize(); iv.image.SetFromPixbuf(nil) }
func (iv *ImageViewer) onScanComplete(update func()) { update() }

func (iv *ImageViewer) scanImagesAsyncVia(schedule func(func())) bool {
	if !iv.tryBeginScan() {
		return false
	}
	go func() {
		schedule(func() { iv.onScanComplete(iv.updateImage) })
	}()
	return true
}

func (iv *ImageViewer) scanImagesAsync() bool { return iv.scanImagesAsyncVia(idleOnce) }

func (g gtkViewer) Rescan() bool {
	if !g.iv.scanImagesAsync() {
		return false
	}
	g.iv.updateImage()
	return true
}

func (g gtkViewer) Next() { g.iv.updateImage() }
`
	g := scanThreadAffinity(t, []goSource{{name: "mutant.go", src: mutant}})
	path, ok := g.reaches("gtkViewer.Rescan")
	if !ok {
		t.Fatalf("the detector did not see gtkViewer.Rescan -> updateImage -> window.GetSize in "+
			"source that plainly contains it (graph: %v). It cannot observe the hazard, so its "+
			"clean verdict on the real package means nothing.", g.names())
	}
	t.Logf("mutant path: %s", strings.Join(path, " -> "))

	// The correct shape: the SAME render, reached only through the scheduler, and
	// only as a function VALUE handed to onScanComplete. Neither route may count.
	const good = `package main

type ImageViewer struct{}
type gtkViewer struct{ iv *ImageViewer }

func idleOnce(fn func()) { glib.IdleAdd(func() bool { fn(); return false }) }

func (iv *ImageViewer) updateImage()       { iv.updateSingleImage() }
func (iv *ImageViewer) updateSingleImage() { iv.window.GetSize(); iv.image.SetFromPixbuf(nil) }
func (iv *ImageViewer) onScanComplete(update func()) { update() }

func (iv *ImageViewer) scanImagesAsyncVia(schedule func(func())) bool {
	if !iv.tryBeginScan() {
		return false
	}
	go func() {
		schedule(func() { iv.onScanComplete(iv.updateImage) })
	}()
	return true
}

func (iv *ImageViewer) scanImagesAsync() bool { return iv.scanImagesAsyncVia(idleOnce) }

func (g gtkViewer) Rescan() bool { return g.iv.scanImagesAsync() }

func (g gtkViewer) Next() { g.iv.updateImage() }
`
	g = scanThreadAffinity(t, []goSource{{name: "good.go", src: good}})
	if path, ok := g.reaches("gtkViewer.Rescan"); ok {
		t.Errorf("the detector reports correct source as reaching a widget (%s) — it is "+
			"always-positive, which is a guard nobody can keep green and therefore a guard nobody "+
			"keeps", strings.Join(path, " -> "))
	}
	if _, ok := g.reaches("gtkViewer.Next"); !ok {
		t.Error("the detector says gtkViewer.Next reaches no widget in source where it calls " +
			"updateImage directly")
	}

	// 🔴 A scheduler that is not a scheduler must NOT be treated as the bridge,
	// AT THE CALL SITE. The exemption is earned structurally — by the callee being
	// a function that itself calls glib.IdleAdd, or by the parameter's declared
	// type being func(func()).
	//
	// 🔴 ROUND-4 CORRECTION. This comment used to end "so an inline 'scheduler'
	// cannot buy its way out of it", and that was false in one direction the
	// fixture below does not exercise. `runNow(func(){ … })` is caught, as here,
	// because runNow is neither a bridge nor a declared func(func()) parameter.
	// But `scanImagesAsyncVia(runInline)` IS exempted: inside
	// scanImagesAsyncVia the parameter `schedule` is exempt on its declared TYPE,
	// and schedulerParams never resolves the VALUE the caller passed — measured
	// `reaches=false` at d025210. So the exemption is earned by the callee's own
	// signature, not by what any caller hands it; what pins the caller is
	// TestScanImagesAsyncSchedulesThroughTheGTKMainLoop, which asserts through
	// scanSchedulerCalls that scanImagesAsync hands over idleOnce specifically.
	const fakeBridge = `package main

type ImageViewer struct{}
type gtkViewer struct{ iv *ImageViewer }

func runNow(fn func()) { fn() }

func (iv *ImageViewer) updateImage()       { iv.updateSingleImage() }
func (iv *ImageViewer) updateSingleImage() { iv.window.GetSize() }

func (g gtkViewer) Rescan() bool {
	runNow(func() { g.iv.updateImage() })
	return true
}
`
	g = scanThreadAffinity(t, []goSource{{name: "fake.go", src: fakeBridge}})
	if path, ok := g.reaches("gtkViewer.Rescan"); !ok {
		t.Errorf("the detector exempted `runNow(func(){ updateImage() })`, where runNow just calls "+
			"its argument on the caller's goroutine. Anything named like a scheduler would then be "+
			"a way out of this guard (graph: %v)", g.names())
	} else {
		t.Logf("fake-bridge path: %s", strings.Join(path, " -> "))
	}

	// 🔴 ROUND-4 MUTANT, measured `reaches=false` at d025210: the render is
	// reached through a func value handed to a PARAMETER. This is the shape
	// scanImagesAsyncVia itself uses two lines from the guarded call, so a
	// maintainer writing it in Rescan is copying the neighbouring line.
	const funcValueParam = `package main

type ImageViewer struct{}
type gtkViewer struct{ iv *ImageViewer }

func idleOnce(fn func()) { glib.IdleAdd(func() bool { fn(); return false }) }

func (iv *ImageViewer) updateImage()       { iv.updateSingleImage() }
func (iv *ImageViewer) updateSingleImage() { iv.window.GetSize() }
func (iv *ImageViewer) onScanComplete(update func()) { update() }

func (g gtkViewer) Rescan() bool {
	g.iv.onScanComplete(g.iv.updateImage)
	return true
}
`
	g = scanThreadAffinity(t, []goSource{{name: "funcvalue.go", src: funcValueParam}})
	path, ok = g.reaches("gtkViewer.Rescan")
	if !ok {
		t.Errorf("the detector did not see `g.iv.onScanComplete(g.iv.updateImage)` reaching a "+
			"widget (graph: %v). The rendering call is in the callee and updateImage appears only "+
			"as an ARGUMENT, so a graph built from callees alone renders off the GTK thread with "+
			"this guard silent.", g.names())
	} else {
		t.Logf("func-value path: %s", strings.Join(path, " -> "))
	}

	// ... and the widening must not swallow the bridge. Handing the SAME method
	// value to the main loop is the correct way to render from off-thread, and it
	// must stay exempt — otherwise every scheduling function looks unsafe and the
	// guard is one nobody can keep green.
	const funcValueThroughTheBridge = `package main

type ImageViewer struct{}
type gtkViewer struct{ iv *ImageViewer }

func idleOnce(fn func()) { glib.IdleAdd(func() bool { fn(); return false }) }

func (iv *ImageViewer) updateImage()       { iv.updateSingleImage() }
func (iv *ImageViewer) updateSingleImage() { iv.window.GetSize() }

func (g gtkViewer) Rescan() bool {
	idleOnce(g.iv.updateImage)
	return true
}
`
	g = scanThreadAffinity(t, []goSource{{name: "bridged.go", src: funcValueThroughTheBridge}})
	if path, ok := g.reaches("gtkViewer.Rescan"); ok {
		t.Errorf("the detector reports `idleOnce(g.iv.updateImage)` as reaching a widget off the "+
			"GTK thread (%s). That IS the bridge — the render runs on the main loop — and flagging "+
			"it makes the one correct way to schedule a render fail this guard.",
			strings.Join(path, " -> "))
	}
}

// packageSources reads every non-test source in package main, with the positive
// control on the input set that keeps the scan non-vacuous.
func packageSources(t *testing.T) []goSource {
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
	if len(files) < 3 {
		t.Fatalf("only %d non-test sources found in package main (%v) — this guard is not looking "+
			"at the package it claims to check", len(files), files)
	}
	return files
}

// scanThreadAffinity builds the annotated call graph.
func scanThreadAffinity(t *testing.T, files []goSource) *threadAffinity {
	t.Helper()

	g := &threadAffinity{
		funcs:  map[string]*ast.FuncDecl{},
		direct: map[string]string{},
		edges:  map[string][]string{},
	}

	parsed := make([]*ast.File, 0, len(files))
	imports := map[string]bool{}
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.name, f.src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f.name, err)
		}
		parsed = append(parsed, file)
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			name := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = true
		}
	}
	// The gtk/gdk/glib names are load-bearing even in a synthetic fixture that
	// declares no imports, so seed them.
	for _, name := range []string{"gtk", "gdk", "glib", "log", "fmt", "os", "time", "sync", "context"} {
		imports[name] = true
	}

	// Index every declaration, and index method names so a call can be resolved
	// without a type checker.
	byMethod := map[string][]string{}
	byFunc := map[string]string{}
	for _, file := range parsed {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := funcDisplayName(fn)
			g.funcs[name] = fn
			if fn.Recv != nil {
				byMethod[fn.Name.Name] = append(byMethod[fn.Name.Name], name)
			} else {
				byFunc[fn.Name.Name] = name
			}
		}
	}

	// The GTK-main-loop bridges: package functions whose own body calls
	// glib.IdleAdd or glib.IdleAddPriority. Naming them would be a spelled guard;
	// deriving them from what they DO means renaming idleOnce cannot silently
	// widen the exemption, and inventing a same-shaped function that does not
	// reach the main loop cannot buy one.
	bridges := map[string]bool{}
	for name, fn := range g.funcs {
		plain := name
		if i := strings.LastIndex(name, "."); i >= 0 {
			plain = name[i+1:]
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "glib" &&
				strings.HasPrefix(sel.Sel.Name, "IdleAdd") {
				bridges[plain] = true
			}
			return true
		})
	}

	for name, fn := range g.funcs {
		schedulers := schedulerParams(fn)
		// edgeTo resolves a callee-or-func-value expression to this package's
		// declarations, by NAME — the same over-approximation the whole graph uses.
		edgeTo := func(e ast.Expr) {
			switch x := e.(type) {
			case *ast.Ident:
				if to, ok := byFunc[x.Name]; ok {
					g.edges[name] = append(g.edges[name], to)
				}
			case *ast.SelectorExpr:
				// Skip a call into an imported package: it cannot be one of this
				// package's methods however its name reads.
				if pkg, ok := x.X.(*ast.Ident); ok && imports[pkg.Name] {
					return
				}
				g.edges[name] = append(g.edges[name], byMethod[x.Sel.Name]...)
			}
		}
		walkNonBridgedCalls(fn, bridges, schedulers, func(call *ast.CallExpr) {
			if touch, ok := widgetCall(call); ok {
				if _, already := g.direct[name]; !already {
					g.direct[name] = touch
				}
				return
			}
			edgeTo(call.Fun)

			// 🔴 ROUND 4: a function passed as a VALUE is a call this graph did not
			// see. `g.iv.onScanComplete(g.iv.updateImage)` renders — the rendering
			// call is in the callee, and `func onScanComplete(update func())` is the
			// exact idiom scanImagesAsyncVia uses two lines from the guarded call —
			// but updateImage appeared only as an argument, so Rescan measured
			// `reaches=false`. Handing a func value to a call is treated as reaching
			// it.
			//
			// The bridge exemption is preserved because handsToMainLoop is asked
			// first: `idleOnce(g.iv.updateImage)` is a render scheduled ONTO the main
			// loop, which is correct and must not count, exactly as the closure form
			// `idleOnce(func(){ … })` does not.
			if handsToMainLoop(call, bridges, schedulers) {
				return
			}
			for _, arg := range call.Args {
				edgeTo(arg)
			}
		})
	}
	return g
}

// handsToMainLoop reports whether call gives its arguments to the GTK main loop,
// so that whatever they contain runs ON that loop and may render.
//
// One predicate, one place: walkNonBridgedCalls uses it to decide whether to
// descend into the arguments, and the func-value edge above uses it to decide
// whether an argument is a hand-off to the loop or an ordinary call. Two copies
// of this test would be two chances to disagree about the one exemption in this
// file.
func handsToMainLoop(call *ast.CallExpr, bridges, schedulers map[string]bool) bool {
	switch callee := call.Fun.(type) {
	case *ast.Ident:
		return bridges[callee.Name] || schedulers[callee.Name]
	case *ast.SelectorExpr:
		pkg, ok := callee.X.(*ast.Ident)
		return ok && pkg.Name == "glib" && strings.HasPrefix(callee.Sel.Name, "IdleAdd")
	}
	return false
}

// widgetCall reports whether call is a GTK call: a method on one of
// ImageViewer's widget handles, or a call into package gtk.
func widgetCall(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	recv := renderExpr(sel.X)
	for _, field := range widgetFields {
		if recv == field || strings.HasSuffix(recv, "."+field) {
			return recv + "." + sel.Sel.Name + "() [GTK widget]", true
		}
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "gtk" {
		return "gtk." + sel.Sel.Name + "() [GTK]", true
	}
	return "", false
}

// schedulerParams reports the parameters of fn declared `func(func())` — the
// GTK-main-loop scheduler seam that scanImagesAsyncVia and enqueueBounded take.
// The type is the credential, not the name.
func schedulerParams(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Type.Params == nil {
		return out
	}
	for _, field := range fn.Type.Params.List {
		outer, ok := field.Type.(*ast.FuncType)
		if !ok || outer.Results != nil || outer.Params == nil || len(outer.Params.List) != 1 {
			continue
		}
		inner, ok := outer.Params.List[0].Type.(*ast.FuncType)
		if !ok || inner.Results != nil || (inner.Params != nil && len(inner.Params.List) != 0) {
			continue
		}
		for _, name := range field.Names {
			out[name.Name] = true
		}
	}
	return out
}

// walkNonBridgedCalls visits every call in fn EXCEPT the arguments of a call
// that hands work to the GTK main loop. A closure scheduled onto the main loop
// runs ON the main loop and may render; that is the whole point of the bridge,
// and counting it would make every scheduling function look unsafe.
func walkNonBridgedCalls(fn *ast.FuncDecl, bridges, schedulers map[string]bool, visit func(*ast.CallExpr)) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		visit(call)
		return !handsToMainLoop(call, bridges, schedulers) // its arguments run on the GTK main loop
	})
}
