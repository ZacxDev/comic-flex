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
// paints "0 comics" over a comic the operator can see.
//
// 🔴 The rule below is a PARTITION and the guard order is load-bearing. An
// earlier wording of it dropped the `!Scanning` from the last two arms, which
// made cold start — Scanning true, Total 0, Keys empty — match BOTH the first
// arm and the last, and they say opposite things. A client implementing the last
// arm literally would paint "0 comics" during indexing: the exact defect the
// first paragraph of this comment exists to prevent, reintroduced by the
// clarification meant to close a different blindness in it. Test Scanning FIRST:
//
//	Scanning && Total == 0                → "indexing…"
//	!Scanning && Total == 0 && len(Keys)>0 → the bucket is empty but the display
//	                                         still holds the last page it rendered
//	!Scanning && Total == 0 && len(Keys)==0 → the only genuine "no comics"
//
// TestTheEmptiedGalleryStatesAreReachableAndDistinct pins the two Total == 0
// states apart, so neither can quietly stop being reachable.
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
//	Lengths. Empty until the first render completes — that is the boot state and
//	it is not covered by the list below. THEREAFTER, and only in a SETTLED state
//	(a render has completed in the CURRENT view mode): 1 in either single view;
//	2 in landscape_two when two DISTINCT positions are on screen; 1 in
//	landscape_two when both halves are the same position (a one-image gallery).
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
//	🔴 THE CEILING IS A CLAMP, AND IT NO LONGER FREEZES AFTER POST /api/interval.
//	This paragraph used to describe the freeze as an asserted property: SetInterval
//	only stored the value — it "took effect on the next tick" — so a source armed
//	for 3600 s was still 3600 s from firing after the interval was set to 30, and
//	the clamp reported a stationary 30 for the next 59 minutes. That is fixed at
//	the cause: SetInterval now retires the pending timer and arms a fresh one at
//	the new interval, so the countdown restarts from the NEW value and decreases
//	from there. A consumer that lowers the interval should see its next poll
//	report at most the new interval, and fall from there towards 0.
//
//	⚠ That is a statement about the CURRENT cycle, and it is not a licence to
//	assert monotonic decrease across polls. The countdown resets to SlideInterval
//	on every advance, exactly as it always has, so any consumer watching long
//	enough sees it jump back up — that is the timer working, not the freeze
//	returning. The property this change buys is that the reset happens on the NEW
//	interval starting immediately, instead of once the old timer finally expires.
//
//	The clamp itself stays, and stays load-bearing: SecondsUntilNext is still
//	never above SlideInterval. What is gone is the state in which the two could
//	disagree for the better part of an hour.
//
//	The re-arm is CONDITIONAL ON THE VALUE MOVING. Re-POSTing the interval the Pi
//	is already on does not reset the countdown — deliberately, because a settings
//	page that submits its whole form on every change would otherwise push the
//	deadline out on every request and the slideshow would never advance at all.
//
//	🔴 Deliberately a DURATION and not an absolute timestamp. The consumer is a
//	browser on a phone with its own clock, and a duration is immune to skew
//	between the Pi, the pod and the handset. The client decrements locally
//	between polls and re-syncs on each one.
//
// Queue — the play queue's depth and position; see QueueState.
//
// BootID — an opaque identity for THIS RUN of the process, generated at startup
//
//	and constant for the lifetime of the process. It is not a version, not a
//	hostname and not ordered: a client may only compare it for equality.
//
//	🔴 It exists because Queue.ID is a per-process counter and is therefore
//	REUSED after a restart. A client that dedupes a "3 pages were no longer in
//	the library" notification on Queue.ID alone suppresses a real one after the
//	Pi reboots; the key it must use is the PAIR (BootID, Queue.ID). Everything
//	else on this struct is a fact about the slideshow, so a reader looking for
//	"has the Pi restarted?" would otherwise find nothing here at all.
//
//	It is also decision D2 made checkable rather than asserted: the queue does
//	not survive a restart, and a changed BootID is how a client learns that the
//	collection it was watching is gone rather than merely finished.
//
//	Never empty on a Pi that has this field. Absent — like `queue` — identifies a
//	Pi that predates it.
type Snapshot struct {
	Total            int        `json:"total"`
	Index            int        `json:"index"`
	Key              string     `json:"key"`
	Keys             []string   `json:"keys"`
	ViewMode         string     `json:"view_mode"`
	Paused           bool       `json:"paused"`
	SlideInterval    int        `json:"slide_interval"`
	Scanning         bool       `json:"scanning"`
	SecondsUntilNext int        `json:"seconds_until_next"`
	Queue            QueueState `json:"queue"`
	BootID           string     `json:"boot_id"`
}

// QueueState is the play queue's depth and position, reported by GET /api/state
// under the key "queue".
//
// 🔴 THE OBJECT IS ALWAYS PRESENT, AND ITS ABSENCE IS THE VERSION SIGNAL. A Pi
// that predates the play queue omits "queue" from the state object entirely; a
// Pi that has one always emits the object, zero-valued when nothing is queued.
// So `"queue" in state` answers "does this Pi support a play queue at all" and
// `state.queue.length == 0` answers "is one running" — two different questions
// that a single nullable integer would have collapsed into one. That collapse is
// not hypothetical: seconds_until_next has a legitimate 0 (paused, or no timer
// armed), and a consumer that read an ABSENT seconds_until_next as 0 would have
// rendered "stopped" forever against an older Pi.
//
// 🔴 It is a VALUE and not a pointer, deliberately. encoding/json's omitempty is
// inert on a struct value — structs are not among the values it treats as empty
// — so the absent-vs-empty collapse cannot be reintroduced by the one-character
// edit that would reintroduce it on a *QueueState or on a bare int field. That
// is measured rather than assumed: TestQueueIsAlwaysPresentInTheStateObject
// drives a zero-valued queue through the real handler and reads the raw bytes.
type QueueState struct {
	// ID identifies WHICH queue the other three fields describe. It is a
	// per-process generation: 0 before any queue has been installed, and
	// incremented by every POST /api/queue.
	//
	// 🔴 IT IS UNIQUE WITHIN ONE RUN OF THE PROCESS AND NOWHERE WIDER. A Pi that
	// restarts counts from 0 again, so ids ARE reused across boots — which is why
	// the pair a client must dedupe on is (BootID, Queue.ID) and never Queue.ID
	// alone. This paragraph claimed "never reused" for a round, three lines above
	// stating the restart behaviour that falsifies it, and the failure that
	// contradiction ships is concrete: Pi reports {id:3, skipped:2}, the client
	// records "handled 3", the Pi restarts, three new collections are queued, the
	// Pi reports {id:3, skipped:1} — and the client suppresses a real
	// notification. Snapshot.BootID exists to close exactly that.
	//
	// 🔴 IT IS WHAT MAKES Skipped ACTIONABLE, and without it that field cannot be
	// rendered correctly at all. Skipped outlives the queue that produced it (see
	// below), so a drained queue reports `{"id":7,"length":0,"skipped":2}` — and
	// goes on reporting it through fifty unrelated page turns. A polling client
	// with no identity to hang that on cannot tell "the collection that just
	// finished skipped 2 pages" from "some queue an hour ago skipped 2", so it
	// either shows the toast on every poll forever or never shows it. With an id
	// it shows it once, for that queue.
	//
	// The client learns its own queue's id by polling after its 202: the id it
	// sees increase is the one its POST installed. That is a guess in the
	// presence of a second client, and deliberately so — an endpoint that
	// returned the id would have to allocate it on the handler goroutine, before
	// the closure that installs the queue has run, and a caller told "your queue
	// is #8" for a queue a later POST replaced before it was ever installed is a
	// worse lie than an ambiguity between two operators of one television.
	ID int `json:"id"`
	// Length is how many keys the running queue holds — including ones already
	// played and ones that turn out to have left the gallery. 0 means no queue
	// is running, which is also the state a queue returns to when it drains.
	Length int `json:"length"`
	// Position is the 1-BASED position of the queued key currently selected.
	// 0 means no queued key is selected: either because Length is 0, or in the
	// instant between a queue being installed and the first page turn into it.
	//
	// 🔴 One-based so that 0 can mean "none" without a -1 sentinel. A consumer
	// renders "page 3 of 12" straight from Position and Length.
	//
	// In the two-up view it names the LEFT-hand entry, and the queue consumes TWO
	// entries per page turn there — so Position advances by two, and a client must
	// not assume consecutive polls differ by one.
	Position int `json:"position"`
	// Skipped counts queued keys passed over because they were no longer in the
	// gallery when the queue reached them (decision D3). It is what lets a client
	// say "3 pages were no longer in the library" rather than silently playing 12
	// pages of a 15-page collection.
	//
	// 🔴 IT OUTLIVES THE QUEUE, and that is the point. A queue that drains sets
	// Length back to 0, so a count cleared at the same moment could only ever be
	// read by a client that happened to poll mid-queue — i.e. never, for the
	// message it exists to make possible. It is reset by the next POST /api/queue
	// and by nothing else. READ IT WITH ID: on its own it is unattributable, and
	// a client that renders it without checking which queue it belongs to shows a
	// stale toast forever.
	//
	// It counts each queued key AT MOST ONCE for the life of one queue, including
	// across a mid-queue rescan: a page that was played and then left the bucket
	// is not counted when a later pass finds it gone. Otherwise the number would
	// measure how often the Pi re-listed the bucket rather than how many pages the
	// operator cannot see.
	Skipped int `json:"skipped"`
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
	// SetInterval sets the slide interval in seconds and makes it take effect
	// IMMEDIATELY: the implementation must cancel the timer that is pending and
	// arm a fresh one at the new interval, so the next advance is one new
	// interval away rather than whatever remained of the old one.
	//
	// 🔴 It must cancel, not merely re-arm. Arming a second source without
	// retiring the first leaves two live timers, which advances the display twice
	// per period and reads to a viewer as the slideshow skipping pages.
	//
	// Setting the interval it is ALREADY on must be a no-op, countdown included:
	// a client that re-sends its current value on every poll must not be able to
	// push the next advance out indefinitely.
	//
	// 🔴 An implementation MUST REFUSE a non-positive value outright — neither
	// storing it nor arming anything — rather than trusting the handler's
	// 1..3600 bound to have filtered it. The bound is this package's; the danger
	// is the implementation's, and it is specific: a 0 interval arms a zero-delay
	// timer that re-arms itself from its own callback, which on the comic-flex
	// implementation wedges the GTK main loop and takes the display down with it.
	// The requirement belongs here rather than only in that implementation,
	// because a second implementer has no other way to learn it.
	//
	// It is an R1 write, so it runs inside an Enqueue closure — which is what
	// makes arming a GLib source from it legal.
	SetInterval(seconds int)
	// SetQueue REPLACES the play queue with keys and selects the first of them
	// that is still in the gallery.
	//
	// Decision D7: the newest instruction wins. It does not append to a running
	// queue and it does not refuse one — that is the simplest state machine on
	// the Pi, and "play this collection" from a phone means the collection the
	// operator just tapped.
	//
	// What the implementation owes, beyond storing the list:
	//
	//   - A key that is no longer in the gallery is SKIPPED as the queue reaches
	//     it, and counted into Snapshot.Queue.Skipped (decision D3). Skipping in
	//     silence is the failure that decision exists to prevent: a 40-page
	//     collection plays 12 and nobody knows why.
	//   - When the queue drains, the gallery resumes AT THE POSITION THE QUEUE
	//     INTERRUPTED (decision D4). The queue is an interruption, not a seek:
	//     the page turn that drains it lands where the gallery would have been
	//     had the queue never run, NOT wherever the last queued key happens to
	//     sit in the shuffle.
	//   - It is TRANSIENT (decision D2). Nothing persists it and a restarted Pi
	//     comes back with no queue. Do not add persistence.
	//
	// 🔴 The LENGTH BOUND IS THE HANDLER'S, not the implementation's:
	// handleQueue rejects an empty list, a list longer than maxQueueKeys and a
	// list containing an empty key, all with 400, before anything is enqueued.
	// An implementation must still not assume it — but it is not the guard, and
	// duplicating the bound here would be the second copy that gets it wrong.
	//
	// It is an R1 write: called ONLY from inside an Enqueue closure, on the GTK
	// thread, and it may render.
	SetQueue(keys []string)

	// CancelQueue ends any running play queue AND returns the gallery to the page
	// the queue interrupted — the same decision-D4 resume the draining page turn
	// performs, through the same code.
	//
	// 🔴 THE RESTORE IS THE WHOLE POINT, and it is what makes this a different
	// operation from POST /api/goto. Goto already ends a queue, but as a SEEK: it
	// puts the display somewhere NEW and discards the interruption point, so an
	// operator who only wanted to stop the collection ends up on a page they did
	// not choose with no way back to where the slideshow was. Cancel is the
	// inverse — it undoes the interruption instead of adding a second one.
	//
	// 🔴 CANCEL WITH NO QUEUE RUNNING IS A NO-OP, NOT AN ERROR, and the
	// implementation must not move the display in that case. The queue can drain
	// between a UI rendering its cancel button and the operator tapping it, so
	// the request that arrives second describes a state the Pi is already in. A
	// client told 4xx for having achieved the desired outcome would show a
	// spurious failure; a client whose cancel re-seeked the gallery would see the
	// display jump for no reason. Both are worse than doing nothing.
	//
	// It is an R1 write: called ONLY from inside an Enqueue closure, and it may
	// render.
	CancelQueue()

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
