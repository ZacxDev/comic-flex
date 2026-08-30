package main

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/ZacxDev/comic-flex/internal/control"
	"github.com/gotk3/gotk3/glib"
)

// This file is the GTK side of the control API: the ONLY place that knows both
// about glib and about internal/control.
//
// internal/control deliberately imports neither gotk3 nor glib — that is
// asserted by TestControlPackageDependenciesIncludeNoGTK in
// internal/control/structure_test.go, which asks the toolchain for the whole
// transitive dependency set — because gotk3 is cgo-bound to GTK3, needs a
// GTK3 + X11 toolchain, and cannot be cross-compiled. Keeping the endpoint
// surface free of it means every handler is unit-testable with no display.

// idleOnce schedules fn to run once on the GTK main loop, at the ordinary idle
// priority.
//
// This is the single glib.IdleAdd call site in the program. GTK is not thread
// safe and an http.Handler runs on a goroutine that is not the main loop, so
// every mutation and every render must come through here. Returning false makes
// the source one-shot.
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
// Reads (Snapshot, Resolve) run on the handler goroutine and take only the read
// lock. Everything else is called from inside an idleOnce closure, i.e. on the
// GTK thread, and may render.
type gtkViewer struct{ iv *ImageViewer }

// Compile-time proof that the adapter satisfies the port.
var _ control.Viewer = gtkViewer{}

// Enqueue schedules fn on the GTK main loop, or refuses when the loop already
// has maxQueuedMutations closures outstanding. See enqueueBounded in state.go.
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

func (g gtkViewer) Rescan() { g.iv.scanImagesAsync() }

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
