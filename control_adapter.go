package main

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/ZacxDev/comic-flex/internal/control"
	"github.com/gotk3/gotk3/glib"
)

// This file is the GTK side of the control API: the ONLY file in package main
// that names internal/control.
//
// 🔴 That was FALSE for one round and is true again. Round 1 added
// `control.DefaultAddr` to main.go, which already imports gtk/gdk/glib, so the
// claim this comment makes was broken by the same commit that wrote it and
// nothing failed. The fix is the deterministic one rather than a reworded
// comment: controlAddr below re-exports the constant, main.go passes THAT, and
// package main's dependency on internal/control is back to this one file.
// TestOnlyTheAdapterImportsTheControlPackage enforces it — the invariant now
// has a guard instead of a sentence.
//
// internal/control deliberately imports neither gotk3 nor glib — that is
// asserted by TestControlPackageDependenciesIncludeNoGTK in
// internal/control/structure_test.go, which asks the toolchain for the whole
// transitive dependency set — because gotk3 is cgo-bound to GTK3, needs a
// GTK3 + X11 toolchain, and cannot be cross-compiled. Keeping the endpoint
// surface free of it means every handler is unit-testable with no display.

// controlAddr is the address main binds the control API to.
//
// It exists so main.go does not have to name internal/control for the one
// constant it needs. That is not cosmetic: main.go imports gtk, gdk and glib, so
// a reference from there is exactly the layering this file's header claims does
// not exist, and round 1 introduced one.
const controlAddr = control.DefaultAddr

// idleOnce schedules fn to run once on the GTK main loop, at the ordinary idle
// priority.
//
// This is the single glib.IdleAdd call site in the program. GTK is not thread
// safe and an http.Handler runs on a goroutine that is not the main loop, so
// every mutation and every render must come through here. Returning false makes
// the source one-shot.
//
// 🔴 NOTHING IS DISCARDED HERE, and that is a measurement rather than a claim.
// A round-4 review asked for the dropped glib.IdleAdd error to be handled, on
// the ground that since round 3 a lost idle source costs a permanently held
// SCAN slot (maxConcurrentScans is released from inside the scheduled closure)
// as well as a missed render. The blast radius is right; the premise is not. At
// the pinned gotk3 v0.6.2, `func IdleAdd(f interface{}) SourceHandle` returns
// ONE value: there is no error, and a bad argument PANICS rather than reporting.
// The SourceHandle is genuinely unused, and correctly so — the closure returns
// false, so the source removes itself and there is nothing left to remove.
//
// The hazard the review named is real and stays OPEN by the dependency's design:
// if the main loop never runs a scheduled closure, its slot is never returned.
// scanImagesAsyncVia states that case and why refusing is the honest answer.
func idleOnce(fn func()) {
	glib.IdleAdd(func() bool {
		fn()
		return false
	})
}

// idleHigh schedules fn on the GTK main loop AHEAD of everything idleOnce has
// queued.
//
// Shutdown is the only caller and must stay that way: the point of the two
// priorities is that ordinary mutations sit in a queue and the quit does not.
// Promoting a mutation to this priority would let a page turn overtake the page
// turns queued before it.
func idleHigh(fn func()) {
	glib.IdleAddPriority(glib.PRIORITY_HIGH, func() bool {
		fn()
		return false
	})
}

// gtkViewer adapts *ImageViewer to control.Viewer.
//
// THREE kinds of method, not two, and the third is the one to read before
// editing anything here:
//
//	Snapshot, Resolve — reads. They run on the HTTP handler goroutine and take
//	                    only the read lock. They must not render.
//	Rescan            — 🔴 ALSO RUNS ON THE HTTP HANDLER GOROUTINE, and it is a
//	                    WRITE. See its own doc below for why. It must not render
//	                    either, and nothing it calls may reach a widget.
//	everything else   — called from inside an idleOnce closure, i.e. on the GTK
//	                    thread, and may render.
//
// 🔴 This comment used to say "everything else is called from inside an idleOnce
// closure … and may render", full stop, with Rescan's exception stated 70 lines
// further down. That is the belief that makes the obvious mistake here look
// safe: Next, Prev, GotoKey and GotoIndex all call updateImage() after their
// state change, so adding the same line to Rescan is what the four neighbours
// do — and it was measured at 250 PASS / 0 FAIL / 0 races under -race, because
// every test viewer's LoadImage errors out before the widget calls.
// TestNothingOffTheGTKThreadCanReachAWidget is the guard; the comment is no
// longer the only thing holding it.
type gtkViewer struct{ iv *ImageViewer }

// Compile-time proof that the adapter satisfies the port.
var _ control.Viewer = gtkViewer{}

// Enqueue schedules fn on the GTK main loop, or refuses when the loop already
// has maxQueuedMutations MUTATION closures outstanding. See enqueueBounded in
// state.go — and note the cap's stated scope there: Rescan does not come through
// here, and the scan-completion closure is bounded by maxConcurrentScans.
func (g gtkViewer) Enqueue(fn func()) bool { return g.iv.enqueueBounded(idleOnce, fn) }

func (g gtkViewer) Snapshot() control.Snapshot {
	s := g.iv.snapshot()
	return control.Snapshot{
		Total:         s.total,
		Index:         s.index,
		Key:           s.key,
		ViewMode:      string(toControlViewMode(s.viewMode)),
		Paused:        s.paused,
		SlideInterval: int(s.slideInterval),
		Scanning:      s.scanning,
	}
}

func (g gtkViewer) Resolve(key string) (int, bool) { return g.iv.indexOfKey(key) }

func (g gtkViewer) Next() {
	if g.iv.advance(g.iv.stepSize()) {
		g.iv.updateImage()
	}
}

func (g gtkViewer) Prev() {
	if g.iv.advance(-g.iv.stepSize()) {
		g.iv.updateImage()
	}
}

func (g gtkViewer) SetPaused(paused bool) { g.iv.setPausedState(paused) }

// TogglePaused flips the paused flag and reports what it landed on.
//
// 🔴 It delegates to iv.togglePaused, which is the SAME primitive the `p`
// keypress in main.go uses, and that is deliberate: one rule, one place. The
// keypress and POST /api/toggle mean exactly the same thing — "the other one" —
// and a second implementation here would be a second chance to get the flip
// wrong, in a code path only one of the two callers exercises.
//
// togglePaused reads and writes under a single write-lock acquisition, so the
// flip is atomic against the keypress handler and against any other queued
// mutation. Nothing here renders: the flag is read by the slide timer on its
// next tick, exactly as SetPaused's is, so there is no widget to touch and no
// updateImage() to add by symmetry with Next/Prev.
func (g gtkViewer) TogglePaused() bool { return g.iv.togglePaused() }

func (g gtkViewer) SetViewMode(mode control.ViewMode) {
	// setViewMode does the whole live switch, including the two blocking xrandr
	// calls — which is precisely why this runs on the GTK thread behind a 202
	// rather than under an HTTP connection.
	g.iv.setViewMode(fromControlViewMode(mode))
}

func (g gtkViewer) GotoKey(key string) {
	if g.iv.gotoKey(key) {
		g.iv.updateImage()
	}
}

func (g gtkViewer) GotoIndex(index int) {
	if g.iv.gotoIndex(index) {
		g.iv.updateImage()
	}
}

func (g gtkViewer) SetInterval(seconds int) {
	if seconds < 0 {
		return // the handler already rejects this; belt for the direct caller
	}
	g.iv.setSlideInterval(uint(seconds))
}

// Rescan starts a bucket listing, or refuses when maxConcurrentScans scans are
// already outstanding. Unlike every other write here it runs on the HTTP handler
// goroutine — see the Viewer.Rescan contract and maxConcurrentScans in state.go.
//
// 🔴 So it MUST NOT RENDER, and neither may anything it calls. Do not add
// `g.iv.updateImage()` here by symmetry with Next/Prev/GotoKey/GotoIndex: those
// four run inside an Enqueue closure on the GTK thread and this does not.
// TestNothingOffTheGTKThreadCanReachAWidget fails if it ever can.
func (g gtkViewer) Rescan() bool { return g.iv.scanImagesAsync() }

// toControlViewMode maps the internal enum to the wire value.
func toControlViewMode(m ViewMode) control.ViewMode {
	switch m {
	case ViewPortraitSingle:
		return control.ViewPortraitSingle
	case ViewLandscapeTwo:
		return control.ViewLandscapeTwo
	default:
		return control.ViewLandscapeSingle
	}
}

// fromControlViewMode maps a wire value to the internal enum. It is only ever
// called with a mode control.ParseViewMode already accepted.
func fromControlViewMode(m control.ViewMode) ViewMode {
	switch m {
	case control.ViewPortraitSingle:
		return ViewPortraitSingle
	case control.ViewLandscapeTwo:
		return ViewLandscapeTwo
	default:
		return ViewLandscapeSingle
	}
}

// startControlAPI builds and starts the control server, or explains why it did
// not start.
//
// 🔴 The decision, stated rather than implied: a missing or too-short token
// DISABLES THE API and the slideshow carries on. It does not kill the process.
//
// Fail-closed is a claim about the control surface — that no unauthenticated
// one exists — and refusing to bind satisfies it completely. Exiting would
// additionally take down the display, and under the Restart=always the unit
// wants it would produce a permanent crash loop: a blank screen, forever, from
// a missing environment variable, with the one log line that explains it buried
// in restart spam. The slideshow is the product; the API is an accessory to it.
// So: no listener, one loud log line, comics keep playing.
//
// addr is a parameter rather than control.DefaultAddr read inline so that the
// LIVENESS direction is testable: TestStartControlAPIServesOnItsAddress binds an
// ephemeral loopback port and drives the real listener. Pinning only the refusal
// direction left `if true { return nil }` here — the whole control API inert —
// passing the entire suite.
func startControlAPI(iv *ImageViewer, addr string) *control.Server {
	srv, err := control.New(control.Config{
		Addr:    addr,
		Token:   os.Getenv(control.TokenEnvVar),
		Viewer:  gtkViewer{iv: iv},
		Version: version,
	})
	if err != nil {
		log.Printf("CONTROL API DISABLED (fail-closed): %v", err)
		log.Printf("CONTROL API DISABLED: set %s to at least %d bytes in the systemd unit's "+
			"Environment to enable it. The slideshow is unaffected and continues running.",
			control.TokenEnvVar, control.MinTokenBytes)
		return nil
	}

	go func() {
		log.Printf("control API listening on %s", srv.Addr())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A bind failure must not take the display with it either.
			log.Printf("control API stopped: %v", err)
		}
	}()
	return srv
}
