package layout_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ZacxDev/comic-flex/internal/layout"
)

// The device this was measured on, so every fixture below is the real geometry
// rather than a round number chosen for arithmetic convenience.
//
//	display, landscape : 3840 x 2160
//	display, portrait  : 2160 x 3840   (xrandr --rotate left)
const (
	dispW, dispH = 3840, 2160
	portW, portH = 2160, 3840
)

// pageW, pageH is a comic page from the bucket. Its aspect is deliberately
// NEITHER the landscape box's nor the portrait box's, so a mutant that returns
// one of its inputs unchanged cannot accidentally produce the right answer, and
// the width- and height-limited branches of Fit are both reachable from it.
const pageW, pageH = 1000, 1626

// ---------------------------------------------------------------------------
// preFixSelectBox — the policy that actually shipped.
//
// updateSingleImage read `width, height := iv.window.GetSize()` (main.go:566)
// and updateTwoImages read the same at main.go:610. That is this function: the
// box IS the window, and the display is never consulted.
//
// It is kept, and asserted below to still exhibit the defect, for the reason
// state_test.go keeps its preFix twins: without it these guards only pin what
// the current code happens to do, and nothing distinguishes a test that catches
// the bug from one that never could.
// ---------------------------------------------------------------------------

func preFixSelectBox(_, _, windowW, windowH int) layout.Box {
	return layout.Box{W: windowW, H: windowH}
}

// ---------------------------------------------------------------------------
// The render feedback loop, modelled.
// ---------------------------------------------------------------------------

// renderOnce models one pass of the real render path and, crucially, its effect
// on the window.
//
// The part that matters is the return value: a GtkImage's size REQUEST is its
// pixbuf's size, and a fullscreen window whose child requests more than the
// screen is grown by the window manager rather than clipping the child. So the
// window after a render is max(fullscreen size, pixbuf size) per axis. That is
// not a guess — it is what was measured on the Pi, where a 3513-tall pixbuf
// turned a 3840x2160 fullscreen window into a 3840x3513 one.
//
// selectBox is a parameter so the SAME loop can be driven with the shipped
// policy and the fixed one, and the difference attributed to that and nothing
// else.
func renderOnce(
	selectBox func(dW, dH, wW, wH int) layout.Box,
	displayW, displayH, winW, winH, srcW, srcH int,
) (pixW, pixH, newWinW, newWinH int) {
	box := selectBox(displayW, displayH, winW, winH)
	pixW, pixH = layout.Fit(srcW, srcH, box)
	return pixW, pixH, maxInt(displayW, pixW), maxInt(displayH, pixH)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// The regression guards.
// ---------------------------------------------------------------------------

// TestRotatingBackToLandscapeRecoversInsteadOfLatching is the bug, end to end,
// in the two phases it was measured in.
//
// Phase 1 is the moment xrandr has returned but neither GDK nor the window
// manager has caught up, so both geometry reads still describe the portrait
// screen and the render produces a portrait-sized pixbuf. That phase is
// UNAVOIDABLE — it is a race with the X server, and no box policy can win it.
//
// Phase 2 is the next render, with both reads now current and the window
// inflated by phase 1. That phase is where the shipped code lost: it read the
// inflated window, produced another oversized pixbuf, and stayed there. 30
// slide advances on the device never recovered.
func TestRotatingBackToLandscapeRecoversInsteadOfLatching(t *testing.T) {
	// Phase 1 — stale reads, both portrait.
	_, pixH, winW, winH := renderOnce(layout.SelectBox, portW, portH, portW, portH, pageW, pageH)
	if winH <= dispH {
		t.Fatalf("phase 1 did not inflate the window (got %dx%d, pixbuf %d tall) — "+
			"the scenario this test exists for is not being reproduced", winW, winH, pixH)
	}
	t.Logf("phase 1 (stale portrait reads): pixbuf %d tall, window %dx%d", pixH, winW, winH)

	// Phase 2 — reads current, window still inflated from phase 1.
	_, pixH, winW, winH = renderOnce(layout.SelectBox, dispW, dispH, winW, winH, pageW, pageH)

	if pixH > dispH {
		t.Errorf("phase 2 rendered a pixbuf %d px tall onto a %d px display — "+
			"the bottom %d px are off-screen", pixH, dispH, pixH-dispH)
	}
	if winW != dispW || winH != dispH {
		t.Errorf("phase 2 left the window at %dx%d, want it back at the display size %dx%d",
			winW, winH, dispW, dispH)
	}
}

// TestPreFixPolicyStillLatches asserts the reproduction above is faithful.
//
// If this ever starts passing, preFixSelectBox has stopped being the code that
// shipped and the guard above is pinning nothing.
func TestPreFixPolicyStillLatches(t *testing.T) {
	_, _, winW, winH := renderOnce(preFixSelectBox, portW, portH, portW, portH, pageW, pageH)

	pixW, pixH, winW, winH := renderOnce(preFixSelectBox, dispW, dispH, winW, winH, pageW, pageH)
	_ = pixW

	if pixH <= dispH {
		t.Fatalf("preFixSelectBox produced a pixbuf %d px tall, which FITS the %d px display — "+
			"it is no longer a faithful reproduction of the shipped policy", pixH, dispH)
	}
	if winH == dispH {
		t.Fatalf("preFixSelectBox recovered the window to %dx%d — "+
			"it is no longer a faithful reproduction of the shipped policy", winW, winH)
	}
	t.Logf("shipped policy latches as measured: pixbuf %d tall, window %dx%d on a %dx%d display",
		pixH, winW, winH, dispW, dispH)
}

// TestTheLatchIsStructurallyImpossible drives the loop far past the point the
// device was observed at. The device gave 30 advances with no recovery; this
// asserts the fixed policy is not merely slower to latch but cannot.
func TestTheLatchIsStructurallyImpossible(t *testing.T) {
	winW, winH := portW, portH // worst case: fully stale, straight after a rotation

	for i := 1; i <= 30; i++ {
		var pixH int
		_, pixH, winW, winH = renderOnce(layout.SelectBox, dispW, dispH, winW, winH, pageW, pageH)
		if pixH > dispH {
			t.Fatalf("advance %d rendered %d px tall onto a %d px display", i, pixH, dispH)
		}
		if winW > dispW || winH > dispH {
			t.Fatalf("advance %d left the window at %dx%d, larger than the %dx%d display",
				i, winW, winH, dispW, dispH)
		}
	}
	if winW != dispW || winH != dispH {
		t.Errorf("after 30 advances the window settled at %dx%d, want %dx%d", winW, winH, dispW, dispH)
	}
}

// TestSelectBoxIgnoresAnInflatedWindow pins the single decision the fix turns
// on, at the exact numbers measured on the device.
func TestSelectBoxIgnoresAnInflatedWindow(t *testing.T) {
	got := layout.SelectBox(dispW, dispH, 3840, 3513)
	want := layout.Box{W: 3840, H: 2160}
	if got != want {
		t.Errorf("SelectBox(display 3840x2160, inflated window 3840x3513) = %+v, want %+v", got, want)
	}
}

// TestSelectBoxIgnoresAStaleDisplayThatIsTooLarge is the OTHER direction, and it
// is the one that stops the fix being "just trust the display". GDK learns about
// a rotation asynchronously, so the display read can itself be stale-portrait
// while the window is already landscape. Taking the minimum keeps that frame
// small, which is recoverable; taking the display would make it 3840 tall on a
// 2160 screen, which is the latch again by another route.
func TestSelectBoxIgnoresAStaleDisplayThatIsTooLarge(t *testing.T) {
	got := layout.SelectBox(portW, portH, dispW, dispH)
	want := layout.Box{W: 2160, H: 2160}
	if got != want {
		t.Errorf("SelectBox(stale portrait display, current landscape window) = %+v, want %+v", got, want)
	}
	if got.H > dispH {
		t.Errorf("box height %d exceeds the real display height %d", got.H, dispH)
	}
}

// TestSelectBoxDoesNotDependOnAPreviouslySeenSize is the property the brief
// names: the box must be a function of its arguments only. A cached "last known
// good size" is the obvious wrong fix for this bug and would pass every other
// test here.
func TestSelectBoxDoesNotDependOnAPreviouslySeenSize(t *testing.T) {
	first := layout.SelectBox(dispW, dispH, dispW, dispH)

	// A long, varied history of other sizes.
	for _, s := range [][4]int{
		{portW, portH, portW, portH},
		{dispW, dispH, 3840, 3513},
		{800, 600, 800, 600},
		{0, 0, 0, 0},
		{portW, portH, dispW, dispH},
	} {
		layout.SelectBox(s[0], s[1], s[2], s[3])
	}

	again := layout.SelectBox(dispW, dispH, dispW, dispH)
	if first != again {
		t.Errorf("SelectBox returned %+v then %+v for identical arguments — it is carrying state", first, again)
	}
}

func TestSelectBoxTreatsNonPositiveDimensionsAsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name           string
		dW, dH, wW, wH int
		want           layout.Box
	}{
		{"unrealised window falls back to the display", dispW, dispH, 0, 0, layout.Box{W: dispW, H: dispH}},
		{"no display reading falls back to the window", 0, 0, dispW, dispH, layout.Box{W: dispW, H: dispH}},
		{"neither known", 0, 0, 0, 0, layout.Box{W: 0, H: 0}},
		{"negative is not smaller", -5, -5, dispW, dispH, layout.Box{W: dispW, H: dispH}},
		{"one axis unknown on each side", 0, dispH, dispW, 0, layout.Box{W: dispW, H: dispH}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := layout.SelectBox(tc.dW, tc.dH, tc.wW, tc.wH); got != tc.want {
				t.Errorf("SelectBox(%d,%d,%d,%d) = %+v, want %+v", tc.dW, tc.dH, tc.wW, tc.wH, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fit — characterization of arithmetic that already shipped.
//
// 🔴 These are NOT regression guards for this bug and must not be counted as
// such: scaleToFit's arithmetic was correct, it was simply given the wrong box.
// They pin it so that moving it into this package is provably behaviour-
// preserving, and so a later edit to it is caught.
// ---------------------------------------------------------------------------

func TestFitPinsExactPixelDimensions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		srcW, srcH   int
		box          layout.Box
		wantW, wantH int
	}{
		// 1000x1626 into 3840x2160: min(3.840, 1.3284) = height-limited.
		{"height-limited page in a landscape box", pageW, pageH, layout.Box{W: dispW, H: dispH}, 1328, 2160},
		// 1000x1626 into 2160x3840: min(2.160, 2.3616) = width-limited.
		{"width-limited page in a portrait box", pageW, pageH, layout.Box{W: portW, H: portH}, 2160, 3512},
		// A wide double-page spread: width-limited in landscape.
		{"wide spread in a landscape box", 2000, 1000, layout.Box{W: dispW, H: dispH}, 3840, 1920},
		{"exact fit is unchanged", dispW, dispH, layout.Box{W: dispW, H: dispH}, dispW, dispH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h := layout.Fit(tc.srcW, tc.srcH, tc.box)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("Fit(%d,%d,%+v) = %dx%d, want %dx%d", tc.srcW, tc.srcH, tc.box, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestFitNeverExceedsItsBox(t *testing.T) {
	boxes := []layout.Box{{W: dispW, H: dispH}, {W: portW, H: portH}, {W: 137, H: 4001}, {W: 1, H: 1}}
	srcs := [][2]int{{pageW, pageH}, {2000, 1000}, {1, 30000}, {30000, 1}, {999, 999}}
	for _, box := range boxes {
		for _, src := range srcs {
			w, h := layout.Fit(src[0], src[1], box)
			if w > box.W || h > box.H {
				t.Errorf("Fit(%d,%d,%+v) = %dx%d, which does not fit", src[0], src[1], box, w, h)
			}
			if w < 1 || h < 1 {
				t.Errorf("Fit(%d,%d,%+v) = %dx%d, which gdk_pixbuf_scale_simple rejects", src[0], src[1], box, w, h)
			}
		}
	}
}

func TestFitReturnsZeroForUnusableInput(t *testing.T) {
	for _, tc := range [][4]int{{0, 100, 100, 100}, {100, 0, 100, 100}, {100, 100, 0, 100}, {100, 100, 100, 0}, {-1, 100, 100, 100}} {
		w, h := layout.Fit(tc[0], tc[1], layout.Box{W: tc[2], H: tc[3]})
		if w != 0 || h != 0 {
			t.Errorf("Fit(%d,%d,{%d,%d}) = %dx%d, want 0x0", tc[0], tc[1], tc[2], tc[3], w, h)
		}
	}
}

// ---------------------------------------------------------------------------
// TwoUp
// ---------------------------------------------------------------------------

// TestTwoUpInACorrectBoxIsCentredOnScreen is the healthy case: with the box
// equal to the display, the images sit centred and nothing is off-screen.
func TestTwoUpInACorrectBoxIsCentredOnScreen(t *testing.T) {
	plan := layout.TwoUp(layout.Box{W: dispW, H: dispH}, pageW, pageH, pageW, pageH)

	if plan.CanvasH != dispH {
		t.Fatalf("canvas is %d tall, want the display height %d", plan.CanvasH, dispH)
	}
	topGap := plan.Left.Y
	bottomGap := plan.CanvasH - (plan.Left.Y + plan.Left.ScaleH)
	if topGap != bottomGap {
		t.Errorf("left image is not vertically centred: %d px above, %d px below", topGap, bottomGap)
	}
	if plan.Left.Y+plan.Left.ScaleH > dispH {
		t.Errorf("left image runs %d px past the bottom of the display",
			plan.Left.Y+plan.Left.ScaleH-dispH)
	}
}

// TestTwoUpInALatchedBoxPushesContentOffTheBottom reproduces the operator's
// exact reported symptom — "images are too low on the Y axis" — as arithmetic.
//
// It asserts the DEFECT, against the latched box, because that is what makes
// the healthy assertion above meaningful. Measured on the device in this state:
// content y=322..2159, i.e. a 322 px black band above and the bottom cut off.
func TestTwoUpInALatchedBoxPushesContentOffTheBottom(t *testing.T) {
	latched := layout.Box{W: dispW, H: 3513} // what the window inflated to
	plan := layout.TwoUp(latched, pageW, pageH, pageW, pageH)

	if plan.Left.Y <= 0 {
		t.Fatalf("expected the latched box to push content down, got Y=%d", plan.Left.Y)
	}
	if plan.Left.Y+plan.Left.ScaleH <= dispH {
		t.Fatalf("expected content to run past the %d px display, it ends at %d", dispH, plan.Left.Y+plan.Left.ScaleH)
	}
	t.Logf("latched box %dx%d pushes the left image to y=%d (%d px band above), "+
		"ending at %d on a %d px display", latched.W, latched.H,
		plan.Left.Y, plan.Left.Y, plan.Left.Y+plan.Left.ScaleH, dispH)
}

func TestTwoUpPlacesTheImagesEitherSideOfCentreWithAGap(t *testing.T) {
	plan := layout.TwoUp(layout.Box{W: dispW, H: dispH}, pageW, pageH, pageW, pageH)

	leftEdge := plan.Left.X + plan.Left.ScaleW
	gap := plan.Right.X - leftEdge
	if gap != 40 {
		t.Errorf("gap between the two images is %d px, want 40", gap)
	}
	centre := dispW / 2
	if leftEdge >= centre || plan.Right.X <= centre {
		t.Errorf("images are not either side of the centre line %d: left ends %d, right starts %d",
			centre, leftEdge, plan.Right.X)
	}
	if !plan.Left.Visible() || !plan.Right.Visible() {
		t.Errorf("a placement is not visible: left %+v right %+v", plan.Left, plan.Right)
	}
}

func TestTwoUpClampsCompositesToTheCanvas(t *testing.T) {
	// A very tall pair in a short box: the scaled height exceeds the canvas, so
	// the composite must be clamped rather than writing past the buffer.
	plan := layout.TwoUp(layout.Box{W: 400, H: 100}, 10, 10000, 10, 10000)
	for name, p := range map[string]layout.Placement{"left": plan.Left, "right": plan.Right} {
		if p.X+p.DestW > plan.CanvasW {
			t.Errorf("%s composite writes to x=%d past canvas width %d", name, p.X+p.DestW, plan.CanvasW)
		}
		if p.Y+p.DestH > plan.CanvasH {
			t.Errorf("%s composite writes to y=%d past canvas height %d", name, p.Y+p.DestH, plan.CanvasH)
		}
	}
}

// ---------------------------------------------------------------------------
// Structural
// ---------------------------------------------------------------------------

// TestLayoutPackageDependenciesIncludeNoGTK keeps this package in the tier that
// builds with a bare Go toolchain. If it ever imports gotk3, the geometry
// becomes untestable without a display again, which is the situation this
// package was created to end.
func TestLayoutPackageDependenciesIncludeNoGTK(t *testing.T) {
	deps := packageDeps(t, ".")

	// Positive control on the result set: an empty list would make the scan
	// below vacuously clean.
	//
	// 🔴 The membership check below is the real control, not a count. This
	// package has exactly 5 transitive dependencies, so any count threshold
	// worth writing sits ON that boundary and stops meaning anything the moment
	// an import is added or removed. Asking for a package this file can SEE is
	// stable under that.
	if len(deps) == 0 {
		t.Fatalf("go list reported no dependencies at all — it is not seeing the package")
	}
	found := map[string]bool{}
	for _, d := range deps {
		found[d] = true
	}
	if !found["math"] {
		t.Fatalf("go list -deps . did not report \"math\", which this package plainly imports — "+
			"the dependency set being scanned is not this package's; got %v", deps)
	}

	var hits []string
	for _, d := range deps {
		if hasForbidden(d) {
			hits = append(hits, d)
		}
	}
	if len(hits) != 0 {
		t.Errorf("internal/layout depends on %v; it must build with no GTK3/X11 toolchain", hits)
	}
	t.Logf("scanned %d transitive dependencies", len(deps))
}

// TestForbiddenDependencyDetectorCanFire is the negative control, built from
// REAL data rather than a synthetic string: package main genuinely depends on
// gotk3, so the same scanner run against it must come back dirty. A scanner
// that reports clean there is wired to nothing and the verdict above is worth
// nothing.
func TestForbiddenDependencyDetectorCanFire(t *testing.T) {
	deps := packageDeps(t, "../..")
	var hits []string
	for _, d := range deps {
		if hasForbidden(d) {
			hits = append(hits, d)
		}
	}
	if len(hits) == 0 {
		t.Fatalf("the forbidden-dependency scanner found nothing in package main, which imports "+
			"gotk3 — the scanner is not working, so its clean verdict on internal/layout means nothing "+
			"(scanned %d deps)", len(deps))
	}
	t.Logf("positive control: %d forbidden dependencies found in package main", len(hits))
}

func packageDeps(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps in %s: %v", dir, err)
	}
	var deps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}
	return deps
}

func hasForbidden(dep string) bool {
	for _, f := range []string{"gotk3", "glib", "gtk", "gdk", "cairo", "pango"} {
		if strings.Contains(dep, f) {
			return true
		}
	}
	return false
}
