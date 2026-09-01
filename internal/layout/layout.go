// Package layout holds the viewer's geometry arithmetic as pure functions.
//
// It exists because the arithmetic was the defect and the arithmetic was the
// one part of the render path that could not be tested: it lived inline in
// updateSingleImage and updateTwoImages, between a 30 s S3 GET and a GTK widget
// call, so exercising it needed a display.
//
// 🔴 This package imports no gotk3 and no glib, for the same reason
// internal/control does not — gotk3 is cgo-bound to GTK3, so anything that
// touches it needs a GTK3 toolchain and cannot be cross-compiled. Keeping the
// geometry here means it is testable in the `go test ./internal/...` tier with
// no display at all. TestLayoutPackageDependenciesIncludeNoGTK enforces it.
package layout

import "math"

// Box is the rectangle a render must fit its content into.
type Box struct{ W, H int }

// SelectBox returns the box a render must lay out into, given the geometry of
// the display the window is on and the window's own current size.
//
// 🔴 THIS IS THE FIX, and the reason it is a function rather than a line is
// that the rule it encodes is the whole bug. The render path used to read
// `iv.window.GetSize()` and nothing else. That is not merely a stale read — it
// is a FEEDBACK LOOP, because the window's size is an OUTPUT of the previous
// render: the scaled pixbuf becomes the GtkImage's size REQUEST, and a request
// larger than the screen pushes the fullscreen window out past the screen edge.
//
// Measured on the Pi (3840x2160 display) on 2026-08-31, switching
// portrait_single -> landscape_single:
//
//	t=+3.65s  screen=3840x2160  win=3840x2160      <- the WM resized it correctly
//	t=+4.88s  screen=3840x2160  win=3840x3513      <- the oversized pixbuf pushed it back out
//
// and it never came back: 30 subsequent slide advances all rendered into a
// 3840x3510 box on a 2160-tall screen, because each one read the inflated
// window and produced another oversized pixbuf. The bug LATCHES. It does not
// self-correct on the next advance, and only a restart cleared it.
//
// The rule is the component-wise MINIMUM of the two inputs. That choice is not
// a heuristic, it is what makes the latch structurally impossible:
//
//	pixbuf <= box <= display, and the window is max(fullscreen, pixbuf) = display
//
// so display is a fixed point and no render can ever grow the window again.
// Preferring the display outright would be wrong in the other direction: GDK
// learns about an xrandr rotation asynchronously, so the display read can ALSO
// be momentarily stale, and taking it alone would trust it blindly. Taking the
// minimum means a stale read can only ever make one frame too SMALL — which is
// centred, harmless, and corrected by the next relayout — never too large,
// which is the case that latches.
//
// A non-positive input is treated as "unknown" and ignored rather than
// propagated: an unrealised window reports 0x0, and a 0-sized box would scale
// every image to nothing.
func SelectBox(displayW, displayH, windowW, windowH int) Box {
	return Box{W: smallerKnown(displayW, windowW), H: smallerKnown(displayH, windowH)}
}

// smallerKnown returns the smaller of two candidate lengths, ignoring either
// when it is not positive, and 0 when neither is known.
func smallerKnown(a, b int) int {
	switch {
	case a <= 0 && b <= 0:
		return 0
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// Fit returns the largest size with the source's aspect ratio that fits inside
// the box.
//
// This is the arithmetic scaleToFit has always used, moved verbatim — the
// float64 division and the truncating int conversion are both preserved, so the
// pixel dimensions this produces are identical to the ones the deployed binary
// produced for the same inputs. The two guards below are the only additions.
func Fit(srcW, srcH int, box Box) (w, h int) {
	// A non-positive input has no aspect ratio and no room to scale into.
	// scaleToFit used to divide by it and hand the NaN/Inf to ScaleSimple.
	if srcW <= 0 || srcH <= 0 || box.W <= 0 || box.H <= 0 {
		return 0, 0
	}

	scale := math.Min(float64(box.W)/float64(srcW), float64(box.H)/float64(srcH))
	w = int(float64(srcW) * scale)
	h = int(float64(srcH) * scale)

	// Truncation can reach 0 for an extreme aspect ratio in a small box, and
	// gdk_pixbuf_scale_simple with a 0 dimension fails rather than returning an
	// empty pixbuf. One pixel is the smallest thing that is still an image.
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

// Two-up spacing. These were unnamed literals inline in updateTwoImages.
const (
	twoUpMargin = 20
	twoUpGap    = 40
)

// Placement is one image's contribution to the two-up canvas: the size its
// source pixbuf must be scaled to, where that scaled pixbuf is drawn, and how
// many pixels of it are actually written once clamped to the canvas.
type Placement struct {
	ScaleW, ScaleH int
	X, Y           int
	DestW, DestH   int
}

// Visible reports whether this placement writes any pixels at all. The caller
// must skip a composite that would write a non-positive rectangle.
func (p Placement) Visible() bool { return p.DestW > 0 && p.DestH > 0 }

// TwoUpPlan is the complete geometry of a side-by-side render.
type TwoUpPlan struct {
	CanvasW, CanvasH int
	Left, Right      Placement
}

// TwoUp computes the whole side-by-side layout for two source images in a box.
//
// The canvas is the box, and each image is centred vertically within it — which
// is exactly why the latch above showed up here as the operator's reported
// symptom rather than as simple cropping. With the box latched to a 3513-tall
// window on a 2160-tall screen, a 2865-tall image centres at y=322 and the
// bottom is off-screen: content sits 322 px too low with a black band above it.
// Measured on the Pi: content y=322..2159, +161 px off screen centre.
//
// The arithmetic is moved verbatim from updateTwoImages, including the
// asymmetry in the negative clamps: leftX, leftY and rightY are floored at 0 and
// rightX is not. That is preserved deliberately rather than tidied, because
// changing it would change rendering, and this change is meant to alter WHICH
// BOX the layout is computed from and nothing else.
func TwoUp(box Box, leftW, leftH, rightW, rightH int) TwoUpPlan {
	plan := TwoUpPlan{CanvasW: box.W, CanvasH: box.H}

	maxImageWidth := (box.W - (2 * twoUpMargin) - twoUpGap) / 2
	inner := Box{W: maxImageWidth, H: box.H}

	lw, lh := Fit(leftW, leftH, inner)
	rw, rh := Fit(rightW, rightH, inner)

	centerX := box.W / 2

	leftX := centerX - twoUpGap/2 - lw
	leftY := (box.H - lh) / 2
	rightX := centerX + twoUpGap/2
	rightY := (box.H - rh) / 2

	if leftX < 0 {
		leftX = 0
	}
	if leftY < 0 {
		leftY = 0
	}
	if rightY < 0 {
		rightY = 0
	}

	plan.Left = Placement{
		ScaleW: lw, ScaleH: lh, X: leftX, Y: leftY,
		DestW: clampSpan(leftX, lw, box.W),
		DestH: clampSpan(leftY, lh, box.H),
	}
	plan.Right = Placement{
		ScaleW: rw, ScaleH: rh, X: rightX, Y: rightY,
		DestW: clampSpan(rightX, rw, box.W),
		DestH: clampSpan(rightY, rh, box.H),
	}
	return plan
}

// clampSpan returns how much of a span of length size starting at pos fits
// inside limit.
func clampSpan(pos, size, limit int) int {
	if pos+size > limit {
		return limit - pos
	}
	return size
}
