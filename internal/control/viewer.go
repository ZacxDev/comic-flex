package control

import "encoding/json"

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
//
// 🔴 Scanning stays true until the DISPLAY CALLBACK HAS RUN, not merely until
// the listing returns — the implementation holds its scan slot across the
// closure it schedules on the display thread (see Viewer.Rescan below, and
// maxConcurrentScans in state.go). The results are published BEFORE that
// closure runs, so `{"total": 3, "scanning": true}` is a legal and expected
// answer, and it can persist for the whole drain latency of the display queue.
// A client that renders a spinner on Scanning ALONE will therefore cover a
// fully populated gallery: the "indexing…" condition is `Scanning && Total == 0`.
// Scanning on its own means "a rescan is not finished with the display yet".
//
// 🔴 SINCE Keys EXISTS THERE IS A FOURTH STATE, and this paragraph did not
// mention it until Keys was added — the addition staled it. `Total == 0 &&
// !Scanning` was described above as flatly "no comics", and it is now reachable
// with Keys POPULATED: a rescan that returns an empty bucket empties the gallery,
// but the last frame is still lit on the glass. Measured: `total 0, key "",
// keys ["a.jpg"], scanning false`. A client following the two-field rule alone
// paints "0 comics" over a comic the operator can see. The full rule is:
// `Scanning && Total == 0` is indexing; `Total == 0 && len(Keys) > 0` is "the
// bucket is empty but the display still holds the last page"; `Total == 0 &&
// len(Keys) == 0` is the only genuine "no comics".
// 🔴 Keys and SecondsUntilNext are the two fields the companion PWA reads, and
// each answers a question the client provably CANNOT answer for itself. Both are
// filled from the SAME lock acquisition as every field above them, so they never
// describe a state the viewer was not in.
//
// Keys — every object key CURRENTLY ON THE DISPLAY, left to right.
//
//	Key is retained unchanged and is Keys[0] in a settled state; this is purely
//	additive and no existing consumer has to move.
//
//	🔴 It comes from THE RENDERER, not from Index. In the two-up view the second
//	image is the one the render loaded, and the client has no way to derive it:
//	the Pi's gallery order is a per-process SHUFFLE (config is_random_order), so
//	the PWA's own list is not the Pi's list and "Index+1" is a guess. Deriving it
//	client-side is the bug this field exists to remove, and an implementation
//	that recomputes it here from Index would reintroduce that bug one hop closer
//	to the data. TestKeysComeFromTheRendererNotFromTheIndex is the guard.
//
//	Consequences of it being the RENDERER's record, stated rather than implied:
//
//	  - a render that FAILED (a 30 s S3 GET that timed out) leaves the previous
//	    keys here, because those are the ones still on the screen;
//	  - between a page turn and the render completing, Key/Index describe the new
//	    SELECTION while Keys still describes the old FRAME. It is deliberate: Key
//	    is where the slideshow is, Keys is what the panel is showing, and
//	    collapsing them would mean lying about one of the two;
//	  - a rescan that empties the gallery leaves Total 0 with Keys still
//	    populated, because the last frame is still lit. Keys can likewise name an
//	    object that has LEFT the bucket, so a consumer presigning them must treat
//	    a 404 as ordinary rather than as an error.
//
//	🔴 THAT DIVERGENCE WINDOW IS UNBOUNDED. This paragraph used to bound it at
//	"a real image load — up to 30 s", which is FALSE and was measured false:
//	nothing re-renders while PAUSED (startSlideshow gates on !isPaused), and
//	onScanComplete only re-renders when currentIndex == 0, so a rescan arriving
//	while paused at a non-zero index leaves Key and Keys disagreeing until an
//	operator does something — total 2, index 1, key "y.jpg", keys ["b.jpg"],
//	where b.jpg is no longer in the gallery at all. Even while PLAYING the
//	ceiling is slide_interval, which POST /api/interval allows up to 3600.
//	A consumer must therefore treat Keys as the display's own answer and never
//	assert it against Key.
//
//	Lengths, IN A SETTLED STATE — i.e. once a render has completed in the CURRENT
//	view mode: 1 in either single view; 2 in landscape_two when two DISTINCT
//	positions are on screen; 1 in landscape_two when both halves are the same
//	position (a one-image gallery); empty when nothing has been rendered yet.
//	🔴 "Settled" is load-bearing and is not decoration: a view-mode switch whose
//	re-render then fails leaves the PREVIOUS mode's frame lit, so
//	`view_mode: "portrait_single"` with two Keys is reachable and legal. A client
//	that lays out from len(Keys) is correct; one that derives the layout from
//	ViewMode and then indexes Keys to match it will be wrong in exactly that case.
//	It is never null on the wire — see MarshalJSON.
//
// SecondsUntilNext — whole seconds until the next AUTOMATIC advance.
//
//	0 when paused (no countdown is running), and 0 when no slide timer is armed.
//	Always within [0, SlideInterval] and never negative.
//
//	🔴 THE CEILING IS A CLAMP, AND AFTER POST /api/interval LOWERS THE INTERVAL IT
//	FREEZES. SetInterval deliberately does not restart the running timer — it
//	"takes effect on the next tick" — so a source armed for 3600 s is still 3600 s
//	from firing after the interval is set to 30, and the clamp reports 30 for the
//	next 59 minutes without moving. Measured on the real accessors; pinned by
//	TestLoweringTheIntervalFreezesTheCountdownUntilTheTimerCatchesUp.
//
//	It is reported this way because the [0, SlideInterval] bound is a fixed part
//	of this wire contract and there is no honest small answer available: the true
//	remaining is the large one the bound forbids. The defect is upstream — a
//	countdown and an interval that can disagree at all — and the fix is for
//	SetInterval to re-arm the timer, which is a change to an EXISTING endpoint's
//	documented semantics and is deliberately not made here. Until it is, a
//	consumer that lowers the interval should expect its own countdown to stall
//	once, and should not read a stalled value as the Pi being wedged.
//
//	🔴 Deliberately a DURATION and not an absolute timestamp. The consumer is a
//	browser on a phone with its own clock, and a duration is immune to skew
//	between the Pi, the pod and the handset. The client decrements locally
//	between polls and re-syncs on each one.
type Snapshot struct {
	Total            int      `json:"total"`
	Index            int      `json:"index"`
	Key              string   `json:"key"`
	Keys             []string `json:"keys"`
	ViewMode         string   `json:"view_mode"`
	Paused           bool     `json:"paused"`
	SlideInterval    int      `json:"slide_interval"`
	Scanning         bool     `json:"scanning"`
	SecondsUntilNext int      `json:"seconds_until_next"`
}

// MarshalJSON renders a Snapshot with Keys guaranteed to be an ARRAY.
//
// 🔴 It exists for one reason: a nil []string marshals to `null`, and the
// contract is that both new fields are ALWAYS PRESENT with a value a consumer
// can branch on. `keys: null` forces every client to write
// `(s.keys || []).length`, and the one that forgets crashes on the empty-gallery
// state — which is the state the Pi is in at every boot.
//
// It is done HERE, on the type, rather than in the handler, so it holds for
// every marshaller of a Snapshot including a future second endpoint. One rule,
// one place: normalising at each call site is how the second call site gets it
// wrong. TestStateKeysIsNeverNullOnTheWire is the guard.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	// alias drops the methods, so json.Marshal below cannot recurse into this one.
	type alias Snapshot
	a := alias(s)
	if a.Keys == nil {
		a.Keys = []string{}
	}
	return json.Marshal(a)
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

	// TogglePaused flips the paused flag and reports the value it landed on.
	//
	// 🔴 IT TAKES NO ARGUMENT, AND THAT IS THE WHOLE POINT. The flip must read
	// and write the flag under ONE lock acquisition, in the implementation,
	// because there is no other place it can be atomic. A caller that reads
	// Snapshot().Paused and then calls SetPaused(!paused) has two lock
	// acquisitions with a gap between them, and in that gap the keypress handler
	// on the Pi (main.go's `p` key calls togglePaused) or another queued closure
	// can move the flag — so the second half writes an absolute value derived
	// from a state that no longer exists, and the user's tap does nothing or
	// undoes someone else's.
	//
	// That is not a hypothetical: it is what the PWA does today, from across the
	// network, with a poll interval's worth of staleness rather than a
	// microsecond's. POST /api/toggle exists to move that decision to the one
	// place it can be made correctly, and a signature with no parameter is what
	// makes the wrong version fail to compile rather than fail in the field.
	//
	// The return value is the state the flip landed on. It is NOT reported to
	// the HTTP caller — the closure runs after the handler has answered 202 (see
	// handleToggle) — but it is what the implementation logs and what a test
	// asserts, and it is the honest shape for the operation.
	//
	// It is an R1 write: called ONLY from inside an Enqueue closure. It does its
	// own locking and must not render.
	TogglePaused() (paused bool)
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
