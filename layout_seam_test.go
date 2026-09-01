package main

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/comic-flex/internal/layout"
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
// The ledger: which code may read the window's own size.
// ---------------------------------------------------------------------------

// TestOnlyLayoutBoxReadsTheWindowSizeForLayout is an asserted ledger of every
// gtk.Window.GetSize call site in the program, and it fails when the set GROWS
// as well as when it shrinks.
//
// 🔴 It is a STRUCTURAL guard and is labelled as one: it proves the render paths
// no longer name GetSize, not that they lay out correctly. The behavioural half
// is in internal/layout. It earns its place because the regression it guards is
// a one-line edit that types perfectly — someone reinstating
// `w, h := iv.window.GetSize()` in updateSingleImage would reintroduce the exact
// latch measured on the Pi, and every other test in this repo would stay green.
//
// The cursor-hiding handler is a legitimate reader: it is comparing a pointer
// coordinate against the window it arrived in, which is a question about the
// WINDOW and not about layout.
func TestOnlyLayoutBoxReadsTheWindowSizeForLayout(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	// 🔴 Comments are stripped BEFORE matching. The first version of this guard
	// did not, and it counted the sentence in layoutBox's own doc comment that
	// names `iv.window.GetSize()` as a third call site. A guard that cannot tell
	// code from prose reports a violation for a documentation edit and, worse,
	// would let a real call site hide behind a trailing comment.
	lines := stripComments(strings.Split(string(src), "\n"))

	// Positive control: the pattern must be able to match something, or a
	// renamed API would make this test vacuously clean forever.
	call := regexp.MustCompile(`\.window\.GetSize\(\)`)
	var sites []string
	for i, line := range lines {
		if call.MatchString(line) {
			sites = append(sites, strings.TrimSpace(line))
			t.Logf("main.go:%d  %s", i+1, strings.TrimSpace(line))
		}
	}
	if len(sites) == 0 {
		t.Fatalf("found no .window.GetSize() call sites at all — the pattern no longer matches " +
			"this codebase, so this guard is inspecting nothing")
	}

	// The ledger. Each entry is the enclosing function, in source order.
	wantOwners := []string{"layoutBox", "setupUI"}
	owners := enclosingFuncs(lines, call)
	if len(owners) != len(wantOwners) {
		t.Fatalf("window.GetSize() is called from %v; the sanctioned set is %v.\n"+
			"If you added a layout read, route it through iv.layoutBox() instead — feeding the "+
			"window's own size back into layout is the feedback loop that latched the rotation bug.",
			owners, wantOwners)
	}
	for i := range owners {
		if owners[i] != wantOwners[i] {
			t.Errorf("call site %d is in %q, want %q", i, owners[i], wantOwners[i])
		}
	}
}

// TestBothRenderPathsUseTheSameLayoutBox pins that the single-image and two-up
// paths agree on where geometry comes from. They disagreed by construction
// before: each had its own inline `iv.window.GetSize()`.
func TestBothRenderPathsUseTheSameLayoutBox(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	for _, fn := range []string{"updateSingleImage", "updateTwoImages"} {
		body := funcBody(t, string(src), fn)
		if !strings.Contains(body, "iv.layoutBox()") {
			t.Errorf("%s does not call iv.layoutBox(); it must not compute geometry any other way", fn)
		}
		if strings.Contains(body, "window.GetSize()") {
			t.Errorf("%s reads the window size directly — that is the latch", fn)
		}
		if !strings.Contains(body, "iv.noteLayoutBox(") {
			t.Errorf("%s does not record the box it rendered at, so the relayout guard cannot "+
				"tell a completed render from a pending one", fn)
		}
	}
}

// stripComments blanks out `//` line comments, keeping the slice the same
// length so reported line numbers still line up with the file.
//
// It is deliberately naive — it does not understand `//` inside a string
// literal, and this file has none. It is not a Go parser and must not be
// mistaken for one; it exists so the ledger counts code.
func stripComments(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		if idx := strings.Index(line, "//"); idx >= 0 {
			out[i] = line[:idx]
			continue
		}
		out[i] = line
	}
	return out
}

func enclosingFuncs(lines []string, pat *regexp.Regexp) []string {
	fnDecl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)
	current := ""
	var owners []string
	for _, line := range lines {
		if m := fnDecl.FindStringSubmatch(line); m != nil {
			current = m[1]
		}
		if pat.MatchString(line) {
			owners = append(owners, current)
		}
	}
	return owners
}

func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "func (iv *ImageViewer) "+name+"(")
	if start < 0 {
		t.Fatalf("could not find func %s in main.go — this guard is inspecting nothing", name)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		rest = rest[:end]
	}
	// Comments stripped for the same reason as the ledger above: these checks
	// are about what the function DOES, and a comment naming GetSize is not a
	// call to it.
	return strings.Join(stripComments(strings.Split(rest, "\n")), "\n")
}

// ---------------------------------------------------------------------------
// The relayout bound.
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

// TestRelayoutCoalescesABurstOfConfigureEvents is the bound that keeps the new
// handler from being a denial of service on the Pi.
//
// A rotation emits several configure events in a row. Each one that got through
// would queue a closure able to block for 30 s in an S3 GET, and they all ask
// the same question.
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
		t.Errorf("25 configure events admitted %d relayouts, want exactly 1", admitted)
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

// TestRelayoutSkipsTheRenderWhenTheBoxIsUnchanged is what stops the handler
// looping. Setting a pixbuf resizes the window, which emits another configure
// event; if that re-rendered unconditionally the program would render forever.
func TestRelayoutSkipsTheRenderWhenTheBoxIsUnchanged(t *testing.T) {
	iv := newTestViewer()
	sched := &manualScheduler{}
	box := func() layout.Box { return layout.Box{W: 3840, H: 2160} }

	// Pretend a render at this box already completed.
	iv.noteLayoutBox(3840, 2160)

	iv.scheduleRelayoutVia(sched.schedule, box)
	sched.runAll() // must return without rendering; an empty gallery keeps it off widgets

	if iv.layoutBoxChanged(3840, 2160) {
		t.Error("layoutBoxChanged reports a change for the box that was just recorded")
	}
	if !iv.layoutBoxChanged(2160, 3840) {
		t.Error("layoutBoxChanged does not report a change for a genuinely different box")
	}
}

// TestAFreshViewerAlwaysNeedsALayout pins the zero value's meaning: nothing has
// been rendered, so every box is a change and the first geometry event renders.
func TestAFreshViewerAlwaysNeedsALayout(t *testing.T) {
	iv := newTestViewer()
	for _, b := range []layout.Box{{W: 3840, H: 2160}, {W: 2160, H: 3840}, {W: 1, H: 1}} {
		if !iv.layoutBoxChanged(b.W, b.H) {
			t.Errorf("a fresh viewer reports no layout needed for %+v", b)
		}
	}
}
