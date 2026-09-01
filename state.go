package main

import (
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/comic-flex/internal/layout"
	"github.com/gotk3/gotk3/glib"
)

// This file holds every access to ImageViewer's shared mutable state.
//
// Until now the slideshow was single-threaded in practice: every mutation ran
// on the GTK main thread, so four genuine defects sat latent and unreachable.
// An HTTP control API makes all four reachable, because a handler goroutine can
// touch this state while the GTK thread is midway through a redraw.
//
// The discipline these accessors enforce, and that callers must preserve:
//
//	1. Every accessor acquires and RELEASES the lock itself. No accessor calls
//	   another while holding it, and no caller may hold it across a call.
//	2. Index arithmetic goes through wrapIndex, which is total — it has no
//	   behaviour that depends on the gallery being non-empty.
//	3. currentIndex is clamped whenever the gallery is replaced.
//
// Rule 1 is not stylistic. sync.RWMutex queues a pending writer ahead of new
// readers, so a second RLock taken while the first is still held blocks forever
// the moment any writer is waiting between them.

// wrapIndex returns the index reached by moving delta positions from current in
// a ring of n elements, for any n and any delta.
//
// The four call sites this replaces each open-coded `(i ± step) % len(images)`,
// which panics with a division by zero when the gallery is empty — reachable as
// soon as anything other than the GTK thread can ask for a page turn before the
// first scan completes.
func wrapIndex(current, delta, n int) int {
	if n <= 0 {
		return 0
	}
	i := (current + delta) % n
	if i < 0 {
		i += n
	}
	return i
}

// setImages replaces the gallery and clamps currentIndex back into range.
//
// A rescan can return fewer images than the previous one — objects get deleted
// from the bucket, and a shuffled rescan reorders everything anyway. Without
// the clamp the next read of images[currentIndex] is an out-of-range panic.
func (iv *ImageViewer) setImages(images []string) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.images = images
	iv.clampIndexLocked()
}

// clampIndexLocked forces currentIndex into [0, len(images)). The caller must
// already hold the write lock.
func (iv *ImageViewer) clampIndexLocked() {
	if len(iv.images) == 0 || iv.currentIndex < 0 {
		iv.currentIndex = 0
		return
	}
	if iv.currentIndex >= len(iv.images) {
		iv.currentIndex = len(iv.images) - 1
	}
}

// imageCount reports how many images the gallery currently holds.
func (iv *ImageViewer) imageCount() int {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return len(iv.images)
}

// advance moves currentIndex by delta and reports whether there is anything to
// show afterwards. It is a no-op returning false on an empty gallery.
func (iv *ImageViewer) advance(delta int) bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if len(iv.images) == 0 {
		iv.currentIndex = 0
		return false
	}
	iv.currentIndex = wrapIndex(iv.currentIndex, delta, len(iv.images))
	return true
}

// currentKey returns the index and object key currently selected. ok is false
// when the gallery is empty, in which case idx and key are zero values.
//
// It acquires and releases the read lock; the caller must not already hold it.
func (iv *ImageViewer) currentKey() (idx int, key string, ok bool) {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	if len(iv.images) == 0 {
		return 0, "", false
	}
	// Normalise locally rather than writing currentIndex back: this runs under
	// the READ lock, so mutating shared state here would be a data race.
	i := wrapIndex(iv.currentIndex, 0, len(iv.images))
	return i, iv.images[i], true
}

// pairKeys returns the two object keys shown side by side in the two-up view.
// With a single image in the gallery both halves are the same key.
//
// It acquires and releases the read lock; the caller must not already hold it.
func (iv *ImageViewer) pairKeys() (leftIdx int, left string, rightIdx int, right string, ok bool) {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	n := len(iv.images)
	if n == 0 {
		return 0, "", 0, "", false
	}
	// Normalise locally, not by writing back: this runs under the READ lock.
	l := wrapIndex(iv.currentIndex, 0, n)
	r := wrapIndex(l, 1, n)
	return l, iv.images[l], r, iv.images[r], true
}

// onScanComplete runs update when a freshly scanned gallery should be shown
// immediately, i.e. nothing has been displayed yet.
//
// 🔴 The read lock is released BEFORE update runs. update re-enters these
// accessors, and the recursive RLock that produces is a deadlock as soon as a
// writer queues between the two acquisitions. Do not fold the call back inside
// the critical section.
func (iv *ImageViewer) onScanComplete(update func()) {
	iv.mutex.RLock()
	show := len(iv.images) > 0 && iv.currentIndex == 0
	iv.mutex.RUnlock()

	if show {
		update()
	}
}

// getViewMode returns the current view mode.
func (iv *ImageViewer) getViewMode() ViewMode {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.viewMode
}

// setViewModeState stores the new view mode and reports whether the display
// orientation changed as a result, so the caller can re-run xrandr outside the
// critical section. Rotating the screen shells out and must never happen with
// the lock held.
func (iv *ImageViewer) setViewModeState(mode ViewMode) (orientationChanged bool) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	wasPortrait := iv.viewMode == ViewPortraitSingle
	isPortrait := mode == ViewPortraitSingle
	iv.viewMode = mode
	return wasPortrait != isPortrait
}

// isPaused reports whether the slideshow is paused.
func (iv *ImageViewer) isPaused() bool {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.paused
}

// togglePaused flips the paused flag and returns the new value.
func (iv *ImageViewer) togglePaused() bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.paused = !iv.paused
	return iv.paused
}

// swapTimeout stores the new GLib timeout handle AND the instant that timer will
// fire, and returns the handle it replaced so the caller can remove the old
// source. Returning the old handle rather than removing it here keeps the GLib
// call out of the critical section.
//
// 🔴 The deadline is a PARAMETER OF THIS CALL, not a second setter, and that is
// the whole mechanism behind GET /api/state's seconds_until_next. Two clocks
// that can disagree is a defect, not an implementation detail: an independent
// "when does the next slide land" field, written anywhere other than where the
// timer is armed, drifts the moment someone adds a third arming site or reorders
// the two writes. Here the handle and the deadline are one atomic write, from
// one interval read, at the one place that calls glib.TimeoutAdd
// (startSlideshow). Retiring a timer passes the zero Time, which countdownFrom
// reads as "nothing armed".
//
// ⚠ It is still not the GLib source's OWN clock, because there is not one to
// read: at the pinned gotk3 v0.6.2 SourceHandle is a bare uint and *Source
// exposes only Destroy/IsDestroyed/Ref/Unref — g_source_get_ready_time is not
// wrapped. So this is the same INSTANT and the same DURATION as the GLib
// timeout, measured on Go's monotonic clock instead of GLib's. The one way they
// diverge is a main loop that fires late — an image load holding it for up to
// 30 s — and that shows up as the countdown sitting at 0, which is the honest
// answer while an advance is overdue.
func (iv *ImageViewer) swapTimeout(next glib.SourceHandle, firesAt time.Time) (previous glib.SourceHandle) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	previous = iv.timeoutID
	iv.timeoutID = next
	iv.nextAdvanceAt = firesAt
	return previous
}

// ---------------------------------------------------------------------------
// Added for the control API (clawgate #442 phase 3a).
//
// Everything below obeys the same rule 1 as the accessors above: acquire, do
// the smallest possible amount of work, release. Nothing here calls back into
// the viewer, and nothing here shells out or touches a widget.
// ---------------------------------------------------------------------------

// setPausedState sets the paused flag to an absolute value.
//
// togglePaused is the right primitive for a click — the user means "the other
// one". It is the WRONG primitive for POST /api/pause, which means "be paused"
// regardless of what it was: a retry of a toggle silently resumes.
func (iv *ImageViewer) setPausedState(paused bool) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.paused = paused
}

// maxConcurrentScans bounds how many SCANS may be outstanding at once, where a
// scan spans the bucket listing AND the completion closure it schedules on the
// GTK main loop.
//
// 🔴 The two halves are one unit on purpose, and saying so is the round-3 fix.
// The slot used to be returned by a `defer iv.endScan()` in the listing
// goroutine, so it came back the moment the closure was SCHEDULED. That bounded
// concurrent listings and bounded nothing about the closures: measured at
// 99a9dca, 40 sequential admitted rescans produced `admitted=40 refused=0
// queueDepth=0` and left 40 completion closures queued on the main context at
// once, each one an updateImage and a gotk3 callback-registry entry. The release
// now happens inside the closure (scanImagesAsyncVia in main.go), so this
// constant bounds both.
//
// 🔴 This is the bound that actually protects the Pi, and the GTK queue cap
// below is NOT it. POST /api/rescan reaches scanImagesAsync, which spawns a
// goroutine and returns in microseconds — so a rescan routed through
// enqueueBounded frees its slot immediately and the cap never engages. Measured
// on the round-1 code, driving the real enqueueBounded + gtkViewer.Rescan 500
// times: `attempts=500 refused(503)=0 queueDepth=0 scansInFlight=500`, i.e. 500
// concurrent MinIO ListObjects calls each able to hold for listTimeout (2 min),
// on a Raspberry Pi.
//
// 4 is deliberately small. Every listing enumerates the SAME bucket, so more
// than a couple in flight buys nothing but sockets and memory; the value has to
// leave room for the startup scan to overlap an operator's rescan, and no more.
// It is above the 2 that TestScanningIsACountNotAFlag drives on purpose — a cap
// chosen to equal a fixture is a cap no fixture can see move.
const maxConcurrentScans = 4

// refusalLogInterval is how often a refusing admission point may write a log
// line. Refusals are caused by the caller, so one line per refusal lets an
// authenticated client that merely retries turn its own backpressure into
// unbounded journald volume on a Raspberry Pi.
const refusalLogInterval = time.Minute

// refusalLog coalesces a refusal message to at most one line per
// refusalLogInterval, carrying the count of everything it suppressed so the
// operator still sees the magnitude.
//
// It has its own mutex rather than sharing the viewer's, so a refusal cannot
// contend with a render for the lock the whole slideshow serialises on.
type refusalLog struct {
	mu         sync.Mutex
	suppressed int
	last       time.Time
	started    bool
}

// note records one refusal at time now. It returns the number of refusals the
// caller should report, the span that count covers, and whether it should report
// at all; total counts every refusal since the previous report, including this
// one, and since is the time elapsed since that previous report.
//
// 🔴 since exists because the caller cannot derive it. refusalLogInterval is the
// MINIMUM silence, not the actual one: a burst that stops for an hour and then
// refuses once reports that single refusal an hour later, and printing "in the
// last 1m0s" there overstated the rate by 60x. For the first report of a run
// there is no previous line, so since is 0 — see refusalSpan.
func (r *refusalLog) note(now time.Time) (total int, since time.Duration, report bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suppressed++
	if r.started && now.Sub(r.last) < refusalLogInterval {
		return 0, 0, false
	}
	if r.started {
		since = now.Sub(r.last)
	}
	r.started = true
	r.last = now
	total, r.suppressed = r.suppressed, 0
	return total, since, true
}

// refusalSpan renders the silence a coalesced count covers, for the log line.
// The first report of a run covers no previous window at all — it is emitted
// immediately — and there is no honest duration to name for it.
func refusalSpan(since time.Duration) string {
	if since <= 0 {
		return "since this process started refusing"
	}
	return "in the last " + since.Round(time.Second).String()
}

// tryBeginScan reserves one of the maxConcurrentScans slots and records that a
// scan is OUTSTANDING. It reports false, having reserved nothing, when the bound
// is reached.
//
// 🔴 "Outstanding", not "in flight" — round 4 corrected this wording, and the
// difference is observable. The slot is released inside the completion closure
// (scanImagesAsyncVia), so a scan stays counted after ListImages has returned
// and after setImages has already published the results, for as long as that
// closure sits on the GTK main loop. Measured: with the completion closure
// QUEUED, `snapshot() -> total=3 scanning=true`.
//
// The counter is what lets GET /api/state distinguish "not yet scanned" from
// "empty". Until the first ListImages returns, total is 0 and a client must
// render "indexing…" rather than "0 comics"; they are different states and the
// API must not collapse them. Note which way round that is: the state a client
// must key on is scanning AND total == 0. scanning alone now stays true past the
// point where total is populated — see Snapshot.Scanning in
// internal/control/viewer.go, which states the contract.
//
// 🔴 A COUNTER, NOT A FLAG — and that is a bug fix, not a style choice. Listings
// overlap the moment the control API exists: POST /api/rescan twice, or once
// while the startup scan is still running, and two goroutines are in ListImages
// at the same time. With a boolean, `defer setScanning(false)` in whichever one
// finished FIRST cleared the flag while the other was still listing, and
// GET /api/state then answered total 0 / scanning false — the "no comics"
// answer — mid-scan. That is exactly the collapse the paragraph above says must
// not happen. The counter makes scanning stay true until the LAST listing ends.
//
// 🔴 There is deliberately NO unconditional beginScan. One existed and was the
// only way in; adding a bounded entry point beside it would have left the
// unbounded one available to the next caller who did not know the difference.
func (iv *ImageViewer) tryBeginScan() bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if iv.scansInFlight >= maxConcurrentScans {
		return false
	}
	iv.scansInFlight++
	return true
}

func (iv *ImageViewer) endScan() {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if iv.scansInFlight > 0 {
		iv.scansInFlight--
	}
}

// isScanning reports whether any SCAN is still outstanding — which is not the
// same as a listing being in flight. A scan spans its completion closure, so
// this stays true after ListImages returned and after setImages published the
// results, until that closure has run on the GTK main loop. It is what
// Snapshot.Scanning carries; see the contract there.
func (iv *ImageViewer) isScanning() bool {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.scansInFlight > 0
}

// scanCount reports how many scans are outstanding — listings still running plus
// listings whose completion closure has not yet run. It exists so a test can
// assert the COUNT rather than only the boolean derived from it — a boolean
// cannot distinguish "one of two finished" from "both finished".
func (iv *ImageViewer) scanCount() int {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.scansInFlight
}

// ---------------------------------------------------------------------------
// The GTK main loop's work queue (bounded).
// ---------------------------------------------------------------------------

// maxQueuedMutations caps how many control-API MUTATION closures may be
// outstanding on the GTK main loop at once.
//
// The loop drains at up to 30 s per image, an authenticated caller can POST as
// fast as the Pi accepts connections, and every scheduled closure holds a gotk3
// callback-registry entry until it runs. Unbounded, a client that merely retries
// grows both without limit. 64 is far more page turns than any operator queues
// deliberately and still bounded memory.
//
// 🔴 SCOPE, stated exactly, because the round-1 version of this comment claimed
// more than the code delivers and the round-2 version claimed an invariant the
// code did not enforce. This bounds the closures enqueueBounded schedules — the
// R1 mutation endpoints that go through Server.enqueue, which is POST
// /api/{next,prev,pause,resume,toggle,viewmode,goto,interval} today. It does NOT
// bound every closure on the main loop. Three others exist and are bounded
// elsewhere — count the bullets rather than trusting this number, which has
// been wrong once already:
//
//   - scanImagesAsyncVia schedules its completion callback (the one that reaches
//     updateSingleImage and a 30 s S3 GET) DIRECTLY, outside this accounting. It
//     is bounded by maxConcurrentScans, and that is true only because the scan
//     slot is released INSIDE that closure rather than by the goroutine that
//     scheduled it. Round 2 released it in the goroutine's defer and asserted the
//     bound here anyway; 40 sequential rescans then left 40 closures outstanding.
//   - scheduleQuit schedules the shutdown at PRIORITY_HIGH, once per process.
//   - scheduleRelayout schedules the post-geometry-change re-render, bounded to
//     ONE outstanding closure by beginRelayout below. It is not control-
//     originated at all — a configure-event from the window manager is what
//     drives it — but it reaches the same main loop and can block for the same
//     30 s, so it is counted here.
//
// So the invariant is: at most maxQueuedMutations + maxConcurrentScans + 1 + 1
// closures outstanding on the main loop — and it holds only while every
// scheduled closure RELEASES THE SLOT THAT ADMITTED IT, from inside itself.
// Anything new that schedules onto the main loop without going through
// enqueueBounded must state its own bound here, and must free that bound in the
// closure, or it makes this comment false again.
// TestTheScanSlotIsHeldUntilTheCompletionClosureRuns is the guard.
const maxQueuedMutations = 64

// reserveQueueSlot takes one of the maxQueuedMutations slots, or reports false.
func (iv *ImageViewer) reserveQueueSlot() bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if iv.queuedMutations >= maxQueuedMutations {
		return false
	}
	iv.queuedMutations++
	return true
}

// releaseQueueSlot gives a slot back once the closure has run.
func (iv *ImageViewer) releaseQueueSlot() {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if iv.queuedMutations > 0 {
		iv.queuedMutations--
	}
}

// queueDepth reports how many closures are outstanding.
func (iv *ImageViewer) queueDepth() int {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.queuedMutations
}

// ---------------------------------------------------------------------------
// The layout box, and the relayout that follows a geometry change.
// ---------------------------------------------------------------------------

// noteLayoutBox records the box the render that just completed scaled into.
//
// 🔴 It takes a layout.Box, NOT a (w, h) pair, and that is a correctness choice
// rather than a stylistic one. With two ints, `iv.noteLayoutBox(box.H, box.W)`
// is a transposition that compiles, type-checks and reads fine — an audit
// mutation did exactly that and SURVIVED the whole suite. A single struct
// argument makes the mistake unrepresentable instead of relying on a test to
// notice it.
func (iv *ImageViewer) noteLayoutBox(b layout.Box) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.lastLayoutBox = b
}

// layoutBoxChanged reports whether b differs from the box the last completed
// render used — i.e. whether a re-render would actually produce anything new.
//
// 🔴 BOTH AXES. An audit mutation that compared width only survived the suite,
// and that is the sharpest possible miss here: the latch this whole change
// exists to prevent was measured as 3840x2160 -> 3840x3513, in which the WIDTH
// IS IDENTICAL and only the height moves. A width-only comparator would decline
// to re-render in precisely the case that matters.
//
// A render that has not completed leaves the zero Box, which differs from every
// real box, so the first geometry change after a failed load re-renders rather
// than being suppressed.
func (iv *ImageViewer) layoutBoxChanged(b layout.Box) bool {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return b != iv.lastLayoutBox
}

// beginRelayout reserves the single relayout slot, or reports false when one is
// already outstanding.
//
// 🔴 ONE, not maxQueuedMutations. A rotation emits a burst of configure events —
// the screen resize, the window manager's reconfigure, and the window's own
// resize as the new pixbuf lands — and every one of them would otherwise queue
// a closure that can block for 30 s in an S3 GET. Coalescing them is the whole
// point: they all ask the same question, and the closure reads the geometry
// itself when it runs, so the one that runs is the one with the current answer.
//
// Like maxConcurrentScans and unlike a naive bound, the slot is released from
// INSIDE the scheduled closure (scheduleRelayout in main.go), so it covers the
// render and not merely the scheduling of it.
func (iv *ImageViewer) beginRelayout() bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if iv.relayoutPending {
		return false
	}
	iv.relayoutPending = true
	return true
}

// endRelayout gives the slot back once the relayout closure has run.
func (iv *ImageViewer) endRelayout() {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.relayoutPending = false
}

// relayoutIsPending reports whether a relayout closure is outstanding. It exists
// so a test can assert the COALESCING rather than only the scheduling.
func (iv *ImageViewer) relayoutIsPending() bool {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.relayoutPending
}

// ---------------------------------------------------------------------------
// What is on the display, and when the next slide lands.
// ---------------------------------------------------------------------------

// noteDisplayed records the object keys the render that just completed put on
// the screen, left to right. It is what GET /api/state's `keys` reports.
//
// 🔴 CALL IT AFTER SetFromPixbuf, NOT BEFORE THE LOAD — the same rule
// noteLayoutBox lives by, and for the same reason. Between selecting a page and
// the pixbuf reaching the widget there is an S3 GET that is allowed to take 30 s
// and allowed to fail. Recording the intent up front would make this field claim
// a frame that never appeared, and a failed load would leave that claim standing
// forever with the PREVIOUS comic still lit.
//
// 🔴 It COPIES, and that is not defensive habit. The two-up path builds its slice
// from pairKeys under one lock and hands it here under another; retaining the
// caller's backing array would let a later render alias into state the HTTP
// goroutine is reading, which is a data race -race would find only sometimes.
func (iv *ImageViewer) noteDisplayed(keys []string) {
	stored := make([]string, len(keys))
	copy(stored, keys)
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.displayedKeys = stored
}

// displayedPair reports the keys a two-up render actually put on the display.
//
// 🔴 The two halves are the SAME POSITION on a one-image gallery — pairKeys wraps
// (right = left + 1 mod n), so with n == 1 both halves load images[0] and the
// screen shows one comic twice. Reporting it twice would tell the PWA two
// different comics are up, which is the exact class of wrongness `keys` exists to
// remove. The test is on the INDEX, not on the key string: two distinct positions
// holding an identical key is a duplicated object in the bucket, and those really
// are two comics on the screen.
func displayedPair(leftIdx int, left string, rightIdx int, right string) []string {
	if leftIdx == rightIdx {
		return []string{left}
	}
	return []string{left, right}
}

// countdownFrom renders the time left before the next automatic advance as whole
// seconds, clamped into [0, interval].
//
// It takes the remaining duration rather than reading a clock so the boundaries
// are exercisable: the caller has already subtracted under the lock.
//
// Ceiling, not truncation: a countdown that has 0.4 s to run has not reached
// zero, and a client that renders "0" while the slide is still up is describing
// a state that has not happened. The ceiling means the value walks 30, 29 … 1, 0
// and reaches 0 only when the advance is actually due or overdue.
//
// 🔴 The ceiling comparison is done in uint64 rather than by converting interval
// to int, and that is what makes "never negative" true BY CONSTRUCTION instead of
// by a third clamp nothing could ever reach. interval is a uint straight out of
// config.yaml, so `int(interval)` is negative for a pathological value, and a
// high clamp against a negative ceiling returns a negative number for a field
// documented as never negative. In uint64 there is no such value: secs starts at
// 1 or more, only ever shrinks to interval, and a Duration caps at ~292 years so
// it cannot exceed the int range on the way back. A guard that cannot fire is
// worse than none — it reads as coverage and stops anyone looking — so there
// isn't one.
//
// ⚠ CORRECTED, and said plainly because the previous wording made a reachability
// claim that is FALSE. It read "interval genuinely reaches 0 … so SetInterval(0)
// lands a zero", which was wrong on both halves: loadConfig rewrites a 0 config
// to 30 (main.go), the handler bounds POST /api/interval to 1..3600, and
// gtkViewer.SetInterval has no caller but that handler's closure — so 0 does NOT
// reach here in production. countdownFrom is a pure function and its interval==0
// row is a TOTALITY test of that function, an invariant guard; it is not
// regression coverage for a bug that has happened, and must not be counted as
// any. Note also which way the damage would run if a 0 ever did arrive: the
// countdown reading 0 is the harmless part, and glib.TimeoutAdd(0, …) in
// startSlideshow spinning the main loop through S3 GETs is the real one.
func countdownFrom(remaining time.Duration, interval uint) int {
	if remaining <= 0 {
		return 0
	}
	// Integer ceiling of remaining/1s: 0.4 s left has not reached zero, and a
	// client rendering "0" while the slide is still up describes a state that
	// has not happened. Truncation would show 0 for a whole second.
	secs := uint64((remaining + time.Second - 1) / time.Second)
	if secs > uint64(interval) {
		secs = uint64(interval)
	}
	return int(secs)
}

// enqueueBounded reserves a slot, hands fn to schedule, and releases the slot
// once fn has run. It reports false, having scheduled nothing, when the queue is
// full.
//
// schedule is idleOnce in production. It is a parameter so that BOTH directions
// — the cap and the release — are exercisable without a GTK main loop: a test
// passes a stand-in that runs the closure when it chooses.
func (iv *ImageViewer) enqueueBounded(schedule func(func()), fn func()) bool {
	if !iv.reserveQueueSlot() {
		// Symmetric with the scan-refusal line in scanImagesAsyncVia, and
		// coalesced for the same reason: this is the other admission point a
		// client can drive as fast as the Pi accepts connections. Round 2 logged
		// one line per refused rescan and nothing at all here; both are now the
		// same shape.
		// 🔴 Read the depth BEFORE note(), not in the argument list: arguments are
		// evaluated left to right, so reading it after note() returned reported a
		// depth another goroutine may already have drained — a number that did not
		// describe the refusal it was printed for.
		depth := iv.queueDepth()
		if n, since, report := iv.queueRefusals.note(time.Now()); report {
			log.Printf("mutation refused: the GTK queue holds %d of %d closures; "+
				"%d refusal(s) %s", depth, maxQueuedMutations, n, refusalSpan(since))
		}
		return false
	}
	schedule(func() {
		defer iv.releaseQueueSlot()
		fn()
	})
	return true
}

// slideInterval reports the configured seconds between slides.
//
// The value lives in config, which the slideshow timer re-reads every tick.
// Once an HTTP handler can read it (GET /api/state) and an enqueued closure can
// write it (POST /api/interval), that unsynchronised access is a data race, so
// both sides go through these two accessors.
func (iv *ImageViewer) slideInterval() uint {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.config.SlideInterval
}

// setSlideInterval changes the seconds between slides and reports whether the
// value actually MOVED.
//
// 🔴 CORRECTED. This used to say "It takes effect on the next tick; the timer is
// not restarted", which was an accurate description of a defect. Nothing
// cancelled or re-armed the pending GLib timeout, so lowering the interval from
// 3600 to 30 changed nothing on the display for up to an hour, and
// GET /api/state's countdown — clamped to [0, slide_interval] against a deadline
// an hour out — sat frozen on 30 for ~59 minutes. The store is still all THIS
// function does; what changed is that its caller now re-arms (gtkViewer.
// SetInterval in control_adapter.go), so a POST /api/interval takes effect
// immediately. Keeping the store and the re-arm as two steps is deliberate: the
// GLib calls must not happen under this lock, and they must not happen off the
// GTK main loop.
//
// changed is why the return value exists, and it is not decoration. The caller
// re-arms only when the value moved, because re-arming resets the countdown: a
// client that POSTs the same interval on every poll — which a settings page that
// submits its whole form is entitled to do — would otherwise push the deadline
// out on every request and starve the advance completely. A no-op write stays a
// no-op.
func (iv *ImageViewer) setSlideInterval(seconds uint) (changed bool) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	changed = iv.config.SlideInterval != seconds
	iv.config.SlideInterval = seconds
	return changed
}

// setArmTimer records the closure that arms the slide timer, so that a later
// interval change can run it again.
//
// startSlideshow is the only caller and its startTimer closure is the only
// value: that closure IS the single arming site — it retires the pending source
// and calls glib.TimeoutAdd, writing the handle and the deadline through one
// swapTimeout. Re-arming by calling it again is therefore not a second arming
// site, which is exactly what TestTheSlideTimerHasExactlyOneArmingSite requires
// and what keeps seconds_until_next honest.
func (iv *ImageViewer) setArmTimer(fn func()) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.armTimer = fn
}

// rearmSlideTimer cancels the pending slide timeout and arms a fresh one at the
// CURRENT interval, and reports whether it did.
//
// 🔴 IT MUST RUN ON THE GTK MAIN LOOP. It calls glib.SourceRemove and
// glib.TimeoutAdd through the closure it invokes, and neither is safe from an
// HTTP handler goroutine. Its one production caller is gtkViewer.SetInterval,
// which is an R1 write and therefore only ever runs inside an Enqueue closure —
// see the Viewer contract in internal/control/viewer.go. Do not call this from
// gtkViewer.Rescan or from any other off-thread path.
//
// 🔴 The lock is RELEASED before the closure runs. startTimer calls swapTimeout,
// which takes the WRITE lock; holding the read lock across it is a deadlock the
// moment a writer queues between the two acquisitions. Same shape, same reason,
// as onScanComplete above.
//
// false means nothing was armed because startSlideshow has not run yet. That is
// reachable only during boot — main() calls startSlideshow before it starts the
// control API — and it is harmless: the value is already stored, so the first
// arm picks it up.
func (iv *ImageViewer) rearmSlideTimer() (rearmed bool) {
	iv.mutex.RLock()
	arm := iv.armTimer
	iv.mutex.RUnlock()

	if arm == nil {
		return false
	}
	arm()
	return true
}

// indexOfKey reports the position of key in the gallery.
//
// It is a read, made from the HTTP handler goroutine, purely to answer 404 on
// POST /api/goto. It is NOT the resolution the page turn uses — gotoKey does
// that again under the write lock, because the gallery can be replaced between
// the two.
func (iv *ImageViewer) indexOfKey(key string) (int, bool) {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	for i, k := range iv.images {
		if k == key {
			return i, true
		}
	}
	return 0, false
}

// gotoKey selects key and reports whether it was found. A key that has since
// left the gallery is a no-op rather than an error: the caller is an enqueued
// closure with nobody left to answer.
func (iv *ImageViewer) gotoKey(key string) bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	for i, k := range iv.images {
		if k == key {
			iv.currentIndex = i
			return true
		}
	}
	return false
}

// gotoIndex selects an absolute position, clamped into range, and reports
// whether there is anything to show.
//
// The clamp is not belt-and-braces: the handler bounds-checked against a
// snapshot taken before this closure was queued, and a rescan in between can
// have shortened the gallery. That is defect 3's shape exactly.
func (iv *ImageViewer) gotoIndex(index int) bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if len(iv.images) == 0 {
		iv.currentIndex = 0
		return false
	}
	iv.currentIndex = index
	iv.clampIndexLocked()
	return true
}

// viewerSnapshot is one consistent read of everything GET /api/state reports.
//
// 🔴 It is NOT comparable with == any more, because keys is a slice. Tests
// compare it with reflect.DeepEqual — deliberately, rather than a hand-written
// field-by-field helper, because a helper is a second copy of the field list and
// the field it forgets is invisible. That is the price of reporting what is on
// the display rather than a scalar the handler could recompute, and recomputing
// it is precisely the defect this field exists to close.
type viewerSnapshot struct {
	total            int
	index            int
	key              string
	keys             []string
	viewMode         ViewMode
	paused           bool
	slideInterval    uint
	scanning         bool
	secondsUntilNext int
}

// snapshot reads every reported field under a SINGLE read lock, as of now.
func (iv *ImageViewer) snapshot() viewerSnapshot { return iv.snapshotAt(time.Now()) }

// snapshotAt reads every reported field under a SINGLE read lock.
//
// Composing it from the individual accessors instead would take and release the
// lock nine times, and the result could describe a state the viewer was never
// in — a key from before a page turn beside an index from after it, or a
// countdown measured against an interval that had already been replaced.
//
// 🔴 keys is READ, not DERIVED. It is whatever the last completed render put on
// the screen (see noteDisplayed); it is deliberately NOT rebuilt here from
// currentIndex and viewMode, because the client's whole reason for asking is
// that it cannot do that arithmetic itself — the gallery is shuffled per process
// and its second two-up key is not "the one after mine". Rebuilding it here
// would move that guess from the browser into the Pi and call it truth.
// TestKeysComeFromTheRendererNotFromTheIndex kills exactly that mutant.
//
// now is a parameter so the countdown's boundaries — armed, mid-flight, overdue,
// paused — are exercisable without sleeping through a 30 s slide interval.
func (iv *ImageViewer) snapshotAt(now time.Time) viewerSnapshot {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()

	s := viewerSnapshot{
		total:         len(iv.images),
		viewMode:      iv.viewMode,
		paused:        iv.paused,
		slideInterval: iv.config.SlideInterval,
		scanning:      iv.scansInFlight > 0,
	}
	if s.total > 0 {
		s.index = wrapIndex(iv.currentIndex, 0, s.total)
		s.key = iv.images[s.index]
	}
	if len(iv.displayedKeys) > 0 {
		s.keys = make([]string, len(iv.displayedKeys))
		copy(s.keys, iv.displayedKeys)
	}
	// Paused means the countdown is not running: the timer still ticks and
	// re-arms, but it advances nothing, so a client counting down to a page turn
	// that will not happen would be showing a lie. A retired timer (handle 0)
	// has no scheduled advance at all.
	if !s.paused && iv.timeoutID != 0 {
		// s.slideInterval, NOT a second read of iv.config.SlideInterval. They are
		// the same value under this lock today, but clamping against the field
		// that is REPORTED makes "seconds_until_next is never above
		// slide_interval" true of this response by construction, rather than true
		// of two reads a later edit could separate.
		s.secondsUntilNext = countdownFrom(iv.nextAdvanceAt.Sub(now), s.slideInterval)
	}
	return s
}
