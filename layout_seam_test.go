package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/comic-flex/internal/layout"
	"github.com/gotk3/gotk3/gdk"
)

// These cover the SEAM between the pure geometry in internal/layout and the GTK
// render path, which is the part neither side's own tests can see.
//
// 🔴 internal/layout is hermetically tested and package main is hermetically
// tested, and that combination is exactly how a defect survives: every
// assertion over there is about arithmetic, and nothing over there knows
// whether updateSingleImage actually CALLS it. The bug being fixed lived in
// precisely that gap — scaleToFit's arithmetic was always correct, it was just
// handed the window's size.

// ---------------------------------------------------------------------------
// Finding a call site: the AST, not a regex.
// ---------------------------------------------------------------------------

// callSite is one method call, with the function it appears in and the
// expression it was called on.
type callSite struct {
	File string
	Func string
	Recv string
	Line int
}

func (c callSite) String() string {
	return fmt.Sprintf("%s:%d %s() in %s, on %q", c.File, c.Line, "", c.Func, c.Recv)
}

// callSitesIn walks one parsed file and reports every call to the named method.
//
// 🔴 This is an AST walk and NOT a regex, and that is the round-1 audit fix.
// The previous version matched the text `\.window\.GetSize\(\)` in main.go. An
// auditor beat it in one edit by writing `win.GetSize()` inside the
// configure-event handler this very change adds — where `win` is the handler's
// own parameter and is in scope at exactly the place a maintainer would reach
// for it. The regex was also blind to the other files of package main, and its
// comment-stripper truncated at the first `//`, so a string literal containing
// `//` would have hidden a call site behind it.
//
// The AST has none of those failure modes: it sees calls, whatever the receiver
// is named, and it never confuses code with a comment or a string.
func callSitesIn(fset *token.FileSet, file *ast.File, name, method string) []callSite {
	var sites []callSite
	var currentFunc string
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			currentFunc = node.Name.Name
		case *ast.CallExpr:
			// Two call shapes, and missing the second is a real defect this
			// guard's own first run demonstrated: a METHOD call is a
			// SelectorExpr (`iv.window.GetSize()`), but a PACKAGE-LEVEL function
			// call is a bare Ident (`firstUsableSize(...)`). Matching only the
			// former reported "displaySize does not call firstUsableSize" for
			// code that plainly does.
			switch fun := node.Fun.(type) {
			case *ast.SelectorExpr:
				if fun.Sel.Name == method {
					sites = append(sites, callSite{
						File: name,
						Func: currentFunc,
						Recv: types.ExprString(fun.X),
						Line: fset.Position(fun.Sel.Pos()).Line,
					})
				}
			case *ast.Ident:
				if fun.Name == method {
					sites = append(sites, callSite{
						File: name,
						Func: currentFunc,
						Recv: "", // a package-level call has no receiver
						Line: fset.Position(fun.Pos()).Line,
					})
				}
			}
		}
		return true
	})
	return sites
}

// packageCallSites walks EVERY non-test .go file of package main, not just
// main.go — control_adapter.go and state.go hold `iv` too.
func packageCallSites(t *testing.T, method string) []callSite {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var sites []callSite
	scanned := 0
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", n, err)
		}
		scanned++
		sites = append(sites, callSitesIn(fset, f, n, method)...)
	}
	if scanned == 0 {
		t.Fatalf("scanned no source files at all — this guard is inspecting nothing")
	}
	t.Logf("scanned %d non-test source file(s) for %s()", scanned, method)
	return sites
}

// TestTheCallSiteFinderSeesACallARegexWouldMiss is the instrument control, and
// it is built from the EXACT shape that walked the previous guard rather than a
// synthetic one: a `GetSize` called on a receiver not named `iv.window`, plus
// one hidden behind a string literal containing `//`.
//
// Without this, a finder that silently matched nothing would make every ledger
// below vacuously clean.
func TestTheCallSiteFinderSeesACallARegexWouldMiss(t *testing.T) {
	const src = `package main
func handler() {
	s := "https://example.invalid//path"
	_ = s
	w, h := win.GetSize()
	_, _ = w, h
}
func other() { _, _ = iv.window.GetSize() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the control source: %v", err)
	}
	sites := callSitesIn(fset, f, "synthetic.go", "GetSize")
	if len(sites) != 2 {
		t.Fatalf("the finder reported %d call sites in the control source, want 2: %+v", len(sites), sites)
	}
	if sites[0].Recv != "win" || sites[0].Func != "handler" {
		t.Errorf("site 0 = %+v, want the `win` receiver in handler — this is the exact call "+
			"that walked the previous regex guard", sites[0])
	}
	if sites[1].Recv != "iv.window" || sites[1].Func != "other" {
		t.Errorf("site 1 = %+v, want iv.window in other", sites[1])
	}

	// The OTHER call shape: a package-level function, which is an Ident and not
	// a SelectorExpr. Matching only selectors made this guard report a call it
	// was staring at as absent.
	bare := callSitesIn(fset, f, "synthetic.go", "handler")
	if len(bare) != 0 {
		t.Fatalf("handler is declared but never called; got %+v", bare)
	}
	const callsSrc = `package main
func a() { firstUsableSize(nil) }
`
	f2, err := parser.ParseFile(fset, "calls.go", callsSrc, 0)
	if err != nil {
		t.Fatalf("parsing the second control source: %v", err)
	}
	got := callSitesIn(fset, f2, "calls.go", "firstUsableSize")
	if len(got) != 1 || got[0].Func != "a" || got[0].Recv != "" {
		t.Errorf("package-level call not found correctly: %+v", got)
	}
}

// TestOnlyLayoutBoxReadsTheWindowSizeForLayout is an asserted ledger of every
// gtk.Window.GetSize call site in package main. It fails when the set GROWS as
// well as when it shrinks.
//
// 🔴 STRUCTURAL guard, labelled as one: it proves the render paths no longer
// read the window's own size, not that they lay out correctly. The behavioural
// half is in internal/layout. It earns its place because the regression it
// guards is a one-line edit that types perfectly — reinstating
// `w, h := iv.window.GetSize()` in updateSingleImage reintroduces the exact
// latch measured on the Pi, and every other test in this repo stays green.
//
// setupUI's site is the cursor-hiding handler, which is a legitimate reader: it
// compares a pointer coordinate against the window the pointer arrived in,
// which is a question about the WINDOW and not about layout.
func TestOnlyLayoutBoxReadsTheWindowSizeForLayout(t *testing.T) {
	sites := packageCallSites(t, "GetSize")
	for _, s := range sites {
		t.Logf("  %s:%d  %s() on %s", s.File, s.Line, s.Func, s.Recv)
	}

	want := []callSite{
		{File: "main.go", Func: "layoutBox", Recv: "iv.window"},
		{File: "main.go", Func: "setupUI", Recv: "iv.window"},
	}
	if len(sites) != len(want) {
		t.Fatalf("GetSize() is called from %d site(s), want %d.\n"+
			"If you added a LAYOUT read, route it through iv.layoutBox() instead — feeding the "+
			"window's own size back into layout is the feedback loop that latched the rotation "+
			"bug. If the new read is legitimately about the window rather than the layout, add "+
			"it to this ledger.\ngot: %+v\nwant: %+v", len(sites), len(want), sites, want)
	}
	for i, w := range want {
		if sites[i].File != w.File || sites[i].Func != w.Func || sites[i].Recv != w.Recv {
			t.Errorf("call site %d = %s:%d %s() on %s, want %s %s() on %s",
				i, sites[i].File, sites[i].Line, sites[i].Func, sites[i].Recv, w.File, w.Func, w.Recv)
		}
	}
}

// TestBothRenderPathsUseTheSameLayoutBox pins that the single-image and two-up
// paths agree on where geometry comes from. They disagreed by construction
// before: each had its own inline `iv.window.GetSize()`.
func TestBothRenderPathsUseTheSameLayoutBox(t *testing.T) {
	renderFuncs := map[string]bool{"updateSingleImage": true, "updateTwoImages": true}

	for _, s := range packageCallSites(t, "GetSize") {
		if renderFuncs[s.Func] {
			t.Errorf("%s:%d reads the window size directly inside %s — that is the latch",
				s.File, s.Line, s.Func)
		}
	}
	for _, method := range []string{"layoutBox", "noteLayoutBox"} {
		seen := map[string]bool{}
		for _, s := range packageCallSites(t, method) {
			if renderFuncs[s.Func] {
				seen[s.Func] = true
			}
		}
		for fn := range renderFuncs {
			if !seen[fn] {
				t.Errorf("%s does not call %s(); both render paths must take their geometry "+
					"from the same place and record what they rendered at", fn, method)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// manualScheduler stands in for the GTK main loop: it collects closures instead
// of running them, so a test decides when — and whether — each one runs.
type manualScheduler struct {
	mu      sync.Mutex
	pending []func()
}

func (m *manualScheduler) schedule(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, fn)
}

func (m *manualScheduler) runAll() {
	m.mu.Lock()
	queued := m.pending
	m.pending = nil
	m.mu.Unlock()
	for _, fn := range queued {
		fn()
	}
}

func (m *manualScheduler) depth() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// countingStore makes "did a render actually happen?" observable without a
// display. updateSingleImage calls LoadImage BEFORE it touches iv.window or
// iv.image, and returns as soon as it errors — so a viewer with a gallery and
// this store runs the whole decision path and stops short of every widget.
type countingStore struct {
	mu    sync.Mutex
	loads int
}

func (c *countingStore) ListImages() ([]string, error) {
	return nil, fmt.Errorf("countingStore does not list")
}

func (c *countingStore) LoadImage(key string) (*gdk.Pixbuf, error) {
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return nil, fmt.Errorf("countingStore never loads (%s)", key)
}

func (c *countingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

// ---------------------------------------------------------------------------
// The relayout bound and its guard
// ---------------------------------------------------------------------------

// TestRelayoutConsultsTheBoxGuardBeforeRendering is the BEHAVIOURAL half that
// round 1 was missing.
//
// 🔴 The previous version of this guard ran the closure against an EMPTY
// gallery, where updateImage no-ops regardless — so it could not tell "the
// guard skipped the render" from "the render ran and did nothing", and an audit
// mutation of `if !iv.layoutBoxChanged(box)` to `if false && …` SURVIVED the
// whole suite. Both directions are asserted here, and the second is the
// positive control: without it, a harness wired to nothing would report the
// same clean zero as a working guard.
func TestRelayoutConsultsTheBoxGuardBeforeRendering(t *testing.T) {
	store := &countingStore{}
	iv := newTestViewer("page.jpg")
	iv.store = store
	sched := &manualScheduler{}
	current := layout.Box{W: 3840, H: 2160}
	readBox := func() layout.Box { return current }

	// Unchanged box: the render must be SKIPPED.
	iv.noteLayoutBox(current)
	if !iv.scheduleRelayoutVia(sched.schedule, readBox) {
		t.Fatal("the relayout was refused")
	}
	sched.runAll()
	if got := store.count(); got != 0 {
		t.Errorf("relayout rendered %d time(s) for an UNCHANGED box, want 0 — "+
			"the box guard is not being consulted", got)
	}

	// Changed box: the render must HAPPEN. Positive control on the whole
	// harness — if this stays 0, the assertion above proves nothing.
	iv.noteLayoutBox(layout.Box{W: 2160, H: 3840})
	if !iv.scheduleRelayoutVia(sched.schedule, readBox) {
		t.Fatal("the second relayout was refused")
	}
	sched.runAll()
	if got := store.count(); got != 1 {
		t.Errorf("relayout rendered %d time(s) for a CHANGED box, want 1 — "+
			"this harness cannot observe a render at all, so the assertion above is vacuous", got)
	}
}

// TestLayoutBoxChangedIsSensitiveToBothAxes is the sharpest guard here.
//
// 🔴 An audit mutation that compared WIDTH ONLY survived the whole suite,
// because the only fixture in play changed both axes (3840x2160 vs 2160x3840).
// The latch this change exists to prevent was measured as
// 3840x2160 -> 3840x3513: the width is IDENTICAL and only the height moves. A
// width-only comparator declines to re-render in exactly that case.
func TestLayoutBoxChangedIsSensitiveToBothAxes(t *testing.T) {
	iv := newTestViewer()
	rendered := layout.Box{W: 3840, H: 2160}
	iv.noteLayoutBox(rendered)

	for _, tc := range []struct {
		name string
		box  layout.Box
		want bool
	}{
		{"the measured latch: height only, width identical", layout.Box{W: 3840, H: 3513}, true},
		{"width only, height identical", layout.Box{W: 2160, H: 2160}, true},
		{"both axes", layout.Box{W: 2160, H: 3840}, true},
		{"identical", rendered, false},
		{"transposed (non-square, so genuinely different)", layout.Box{W: 2160, H: 3840}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := iv.layoutBoxChanged(tc.box); got != tc.want {
				t.Errorf("layoutBoxChanged(%+v) = %v, want %v (last rendered %+v)",
					tc.box, got, tc.want, rendered)
			}
		})
	}
}

// TestRelayoutCoalescesABurstOfConfigureEvents is the bound that keeps the new
// handler from being a denial of service on the Pi.
//
// A rotation emits several configure events in a row, and now a screen
// size-changed as well. Each one that got through would queue a closure able to
// block for 30 s in an S3 GET, and they all ask the same question.
func TestRelayoutCoalescesABurstOfConfigureEvents(t *testing.T) {
	iv := newTestViewer()
	sched := &manualScheduler{}
	box := func() layout.Box { return layout.Box{W: 3840, H: 2160} }

	admitted := 0
	for i := 0; i < 25; i++ {
		if iv.scheduleRelayoutVia(sched.schedule, box) {
			admitted++
		}
	}

	if admitted != 1 {
		t.Errorf("25 geometry events admitted %d relayouts, want exactly 1", admitted)
	}
	if got := sched.depth(); got != 1 {
		t.Errorf("%d closures queued on the main loop, want 1", got)
	}
	if !iv.relayoutIsPending() {
		t.Error("the relayout slot is not held while a closure is outstanding")
	}
}

// TestRelayoutSlotIsReleasedByTheClosureNotByScheduling is the direction that
// round 3 of the control-API work got wrong for scans: a bound released when the
// work is merely SCHEDULED bounds nothing.
func TestRelayoutSlotIsReleasedByTheClosureNotByScheduling(t *testing.T) {
	iv := newTestViewer()
	sched := &manualScheduler{}
	box := func() layout.Box { return layout.Box{W: 3840, H: 2160} }

	if !iv.scheduleRelayoutVia(sched.schedule, box) {
		t.Fatal("the first relayout was refused")
	}
	// Scheduled but NOT run: the slot must still be held.
	if iv.scheduleRelayoutVia(sched.schedule, box) {
		t.Error("a second relayout was admitted while the first was still queued — " +
			"the slot is being released at scheduling time, not when the closure runs")
	}

	sched.runAll()

	if iv.relayoutIsPending() {
		t.Error("the slot is still held after the closure ran")
	}
	if !iv.scheduleRelayoutVia(sched.schedule, box) {
		t.Error("a relayout was refused after the previous one completed — the slot leaked")
	}
}

// ---------------------------------------------------------------------------
// The display-geometry fallback chain
// ---------------------------------------------------------------------------

// TestFirstUsableSizeSkipsAnUnusableReading is the guard for round 1's
// deploy-relevant finding: the fallback chain in displaySize was UNREACHABLE,
// because a zeroed monitor geometry passed a nil check and was returned as an
// answer. The failure mode is the worst kind — displaySize yields 0,0,
// SelectBox then treats the display as unknown and falls back entirely to the
// window size, which is the exact policy that latched. The fix would have gone
// inert with nothing in the journal.
func TestFirstUsableSizeSkipsAnUnusableReading(t *testing.T) {
	reading := func(w, h int, calls *int) func() (int, int) {
		return func() (int, int) { *calls++; return w, h }
	}

	t.Run("a zeroed reading falls through to the next candidate", func(t *testing.T) {
		var a, b int
		w, h, ok := firstUsableSize(reading(0, 0, &a), reading(3840, 2160, &b))
		if !ok || w != 3840 || h != 2160 {
			t.Errorf("got %dx%d ok=%v, want 3840x2160 ok=true", w, h, ok)
		}
		if a != 1 || b != 1 {
			t.Errorf("candidates evaluated %d and %d times, want 1 and 1", a, b)
		}
	})

	t.Run("a HALF-zero reading is unusable too", func(t *testing.T) {
		// This is the shape an invalidated monitor actually produces mid-xrandr.
		for _, bad := range [][2]int{{3840, 0}, {0, 2160}, {-1, 2160}, {3840, -1}} {
			var a, b int
			w, h, ok := firstUsableSize(reading(bad[0], bad[1], &a), reading(2160, 3840, &b))
			if !ok || w != 2160 || h != 3840 {
				t.Errorf("reading %v: got %dx%d ok=%v, want the fallback 2160x3840", bad, w, h, ok)
			}
		}
	})

	t.Run("a usable first reading short-circuits the rest", func(t *testing.T) {
		var a, b int
		w, h, ok := firstUsableSize(reading(3840, 2160, &a), reading(1, 1, &b))
		if !ok || w != 3840 || h != 2160 {
			t.Errorf("got %dx%d ok=%v, want 3840x2160 ok=true", w, h, ok)
		}
		if b != 0 {
			t.Errorf("the second candidate was evaluated %d time(s); it must not be reached", b)
		}
	})

	t.Run("no usable reading reports not-ok rather than a plausible zero", func(t *testing.T) {
		var a, b int
		w, h, ok := firstUsableSize(reading(0, 0, &a), reading(0, 0, &b))
		if ok || w != 0 || h != 0 {
			t.Errorf("got %dx%d ok=%v, want 0x0 ok=false", w, h, ok)
		}
	})

	t.Run("no candidates at all", func(t *testing.T) {
		if _, _, ok := firstUsableSize(); ok {
			t.Error("an empty candidate list reported ok")
		}
	})
}

// TestDisplaySizeConsultsTheFallbackChain pins that displaySize routes its
// readings through firstUsableSize rather than open-coding the decision again.
//
// 🔴 STRUCTURAL, and labelled as such. It exists because the behavioural half
// cannot be reached: every candidate calls into GDK, so exercising displaySize
// itself needs a real display. The pure decision is tested above; this asserts
// it is the one actually used. Together they cover what round 1's mutation
// (`return w, h, true`) walked straight through.
func TestDisplaySizeConsultsTheFallbackChain(t *testing.T) {
	var found bool
	for _, s := range packageCallSites(t, "firstUsableSize") {
		if s.Func == "displaySize" {
			found = true
		}
	}
	if !found {
		t.Error("displaySize does not call firstUsableSize; a hand-rolled fallback chain is " +
			"how round 1's unreachable-fallback defect happened")
	}
}

// relayoutTriggers returns the GTK/GDK signal names that setupUI wires to
// scheduleRelayout, by walking the AST for `.Connect("<signal>", func(){…})`
// calls whose closure body reaches scheduleRelayout.
func relayoutTriggers(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	var signals []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "setupUI" {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Connect" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			reaches := false
			ast.Inspect(call.Args[1], func(k ast.Node) bool {
				if c, ok := k.(*ast.CallExpr); ok {
					if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "scheduleRelayout" {
						reaches = true
					}
				}
				return true
			})
			if reaches {
				signals = append(signals, strings.Trim(lit.Value, `"`))
			}
			return true
		})
		return false
	})
	return signals
}

// TestGeometryChangesAreObservedFromBothTheWindowAndTheScreen is the guard for
// round 1's finding 2, which had a fix and no test.
//
// 🔴 configure-event alone is NOT sufficient, and the reason is an ORDERING.
// The window manager and GDK learn about a rotation independently. If the WM
// gets there first, layoutBox reads a window already rotated against a display
// GDK still reports as the old orientation, and their minimum is a SQUARE box.
// That renders SMALLER than the window — so the window does not move, so no
// further configure-event is emitted, so nothing re-reads the geometry. While
// playing, the 30 s slide timer hides it; while PAUSED, startSlideshow gates
// the re-render behind `if !iv.isPaused()` and the undersized frame persists
// INDEFINITELY. Pausing to look at one page is exactly when an operator would
// see it.
//
// 🔴 STRUCTURAL, and labelled as one: setupUI needs a display, so the wiring
// cannot be exercised here. It pins WHICH signals are wired, which is the thing
// that silently regresses — deleting the screen handlers restores the defect
// and every behavioural test still passes.
func TestGeometryChangesAreObservedFromBothTheWindowAndTheScreen(t *testing.T) {
	got := relayoutTriggers(t)
	t.Logf("signals wired to scheduleRelayout: %v", got)

	want := map[string]string{
		"configure-event":  "the window's own resize",
		"size-changed":     "the screen resizing under a rotation, when GDK moves after the WM",
		"monitors-changed": "the monitor set/geometry changing without a window resize",
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s] = true
	}
	for signal, why := range want {
		if !seen[signal] {
			t.Errorf("no relayout is wired to %q (%s). Without it a geometry change that does "+
				"not resize the window is never noticed, and while PAUSED it is never corrected.",
				signal, why)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d signals are wired to scheduleRelayout, want %d: got %v",
			len(got), len(want), got)
	}
}

// TestTheRelayoutTriggerScannerCanFire is the instrument control for the guard
// above: a zero from a scanner wired to nothing is indistinguishable from a
// zero from correct code.
func TestTheRelayoutTriggerScannerCanFire(t *testing.T) {
	if got := relayoutTriggers(t); len(got) == 0 {
		t.Fatal("the scanner found no relayout triggers at all in setupUI — it is not working, " +
			"so the ledger above means nothing")
	}
}

// TestAFreshViewerAlwaysNeedsALayout pins the zero value's meaning: nothing has
// been rendered, so every box is a change and the first geometry event renders.
func TestAFreshViewerAlwaysNeedsALayout(t *testing.T) {
	iv := newTestViewer()
	for _, b := range []layout.Box{{W: 3840, H: 2160}, {W: 2160, H: 3840}, {W: 1, H: 1}} {
		if !iv.layoutBoxChanged(b) {
			t.Errorf("a fresh viewer reports no layout needed for %+v", b)
		}
	}
}
