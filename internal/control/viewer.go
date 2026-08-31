package control

// This package is the Pi-side control API for comic-flex (clawgate #442 phase 3a).
//
// 🔴 It MUST NOT import gotk3 or glib, and there is a test that fails if it ever
// does — TestControlPackageDependenciesIncludeNoGTK in structure_test.go, which
// asks the toolchain for the whole transitive dependency set rather than reading
// this directory's imports. The reason is not tidiness:
//
//   - gotk3 is cgo-bound to GTK3, so anything importing it needs a GTK3 + X11
//     toolchain to compile at all, and cross-compiling it does not work.
//   - Keeping the whole endpoint surface free of that dependency makes every
//     handler unit-testable with no GTK, no X11 and no display.
//
// The bridge to the GTK main loop is therefore a narrow port — the Viewer
// interface below — implemented in package main, which is the only place
// glib.IdleAdd appears.

// ViewMode is the display layout the slideshow is in. The string values are the
// wire representation used by POST /api/viewmode and GET /api/state; they match
// the values config.yaml's `view_mode` accepts.
type ViewMode string

const (
	ViewLandscapeSingle ViewMode = "landscape_single"
	ViewPortraitSingle  ViewMode = "portrait_single"
	ViewLandscapeTwo    ViewMode = "landscape_two"
)

// ParseViewMode maps a wire value to a ViewMode. ok is false for anything else,
// which the handler turns into 400. Note this deliberately does NOT mirror
// main.parseViewMode, which silently defaults an unknown string to landscape:
// silently accepting a typo over the network would be a lie to the caller.
func ParseViewMode(s string) (ViewMode, bool) {
	switch ViewMode(s) {
	case ViewLandscapeSingle:
		return ViewLandscapeSingle, true
	case ViewPortraitSingle:
		return ViewPortraitSingle, true
	case ViewLandscapeTwo:
		return ViewLandscapeTwo, true
	}
	return "", false
}

// Snapshot is one consistent read of viewer state, and is the exact JSON body
// of GET /api/state.
//
// Scanning is load-bearing: until the first ListImages returns, Total is 0 and
// a client must render "indexing…" rather than "0 comics". An empty gallery and
// an un-scanned gallery are different states and this API distinguishes them.
type Snapshot struct {
	Total         int    `json:"total"`
	Index         int    `json:"index"`
	Key           string `json:"key"`
	ViewMode      string `json:"view_mode"`
	Paused        bool   `json:"paused"`
	SlideInterval int    `json:"slide_interval"`
	Scanning      bool   `json:"scanning"`
}

// Viewer is everything this package needs from the slideshow.
//
// The contract splits into three kinds of method, and the split is the whole
// point (proposal §4.1):
//
//	R2 reads   — Snapshot and Resolve run on the HTTP handler goroutine. They
//	             must take the viewer's read lock, release it before returning,
//	             and touch no widget.
//	   bridge  — Enqueue schedules fn on the GTK main loop and returns
//	             IMMEDIATELY. It must never wait for fn, and it may REFUSE.
//	R1 writes  — every remaining method mutates the viewer and/or renders. They
//	             are called ONLY from inside an Enqueue closure, i.e. on the GTK
//	             thread, and each does its own locking.
//
// R3: a handler must not hold the viewer lock across an Enqueue. That is what
// keeps phase-0 defect 1 (recursive RLock) structurally closed. Snapshot and
// Resolve satisfy it by acquiring and releasing internally and returning plain
// values; the handler then passes those values into the closure.
type Viewer interface {
	// Snapshot returns a consistent read of the state GET /api/state exposes.
	Snapshot() Snapshot

	// Resolve reports the index of key in the gallery. It is a read used only
	// to answer 404 on POST /api/goto; the authoritative resolution happens
	// again inside the enqueued closure, under the lock.
	Resolve(key string) (index int, ok bool)

	// Enqueue schedules fn to run once on the GTK main loop and returns
	// immediately. It must not block on fn: updateSingleImage can sit on an S3
	// GET for 30s and setViewMode shells out to two blocking xrandr calls.
	//
	// It reports false when the main loop already has as much outstanding work
	// as it is allowed, in which case fn is NOT scheduled and the handler
	// answers 503. The bound exists because the loop drains far slower than the
	// API accepts requests and each accepted closure costs a permanent gotk3
	// callback-registry entry.
	Enqueue(fn func()) (scheduled bool)

	// Next advances by one page turn (two images in the two-up view).
	Next()
	// Prev retreats by one page turn.
	Prev()
	// SetPaused pauses or resumes the slideshow timer.
	SetPaused(paused bool)
	// SetViewMode switches layout, rotating the display if orientation changed.
	SetViewMode(mode ViewMode)
	// GotoKey selects the given object key, resolving it under the lock. It is
	// a no-op if the key is no longer in the gallery.
	GotoKey(key string)
	// GotoIndex selects the given index, clamped into range under the lock.
	GotoIndex(index int)
	// SetInterval sets the slide interval in seconds; it takes effect on the
	// next tick without restarting the timer.
	SetInterval(seconds int)
	// Rescan starts a bucket listing in the background and reports whether one
	// was STARTED. It reports false, having started nothing, when the
	// implementation already has as many scans outstanding as it allows, and
	// the handler then answers 503 rather than 202.
	//
	// 🔴 "Outstanding" spans more than the listing: the implementation must not
	// treat a scan as finished until the work it hands to the display has RUN.
	// The comic-flex implementation released its slot as soon as the completion
	// callback was SCHEDULED, and 40 sequential rescans then left 40 callbacks
	// queued on the GTK main loop — bounded listings, unbounded callbacks.
	//
	// 🔴 Rescan is the one write that is NOT an R1 method: it is called
	// synchronously on the HTTP handler goroutine, never from inside an Enqueue
	// closure. That is not an inconsistency, it is the fix for one. Routing it
	// through Enqueue bounded the wrong thing — the closure returns in
	// microseconds because the listing is spawned onto its own goroutine, so the
	// queue slot was freed immediately and 500 concurrent listings were admitted
	// with zero refusals (measured; see maxConcurrentScans in the adapter). The
	// admission decision has to be made while the caller is still there to be
	// told, so it is made here.
	//
	// The implementation must therefore be safe to call off the GTK thread, must
	// not block on the listing, and must clamp currentIndex when it swaps the
	// gallery in.
	Rescan() (started bool)
}
