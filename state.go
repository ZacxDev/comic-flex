package main

import (
	"log"
	"sync"
	"time"

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

// swapTimeout stores the new GLib timeout handle and returns the one it
// replaced, so the caller can remove the old source. Returning the old handle
// rather than removing it here keeps the GLib call out of the critical section.
func (iv *ImageViewer) swapTimeout(next glib.SourceHandle) (previous glib.SourceHandle) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	previous = iv.timeoutID
	iv.timeoutID = next
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
// caller should report and whether it should report at all; total counts every
// refusal since the previous report, including this one.
func (r *refusalLog) note(now time.Time) (total int, report bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suppressed++
	if r.started && now.Sub(r.last) < refusalLogInterval {
		return 0, false
	}
	r.started = true
	r.last = now
	total, r.suppressed = r.suppressed, 0
	return total, true
}

// tryBeginScan reserves one of the maxConcurrentScans slots and records that a
// bucket listing is in flight. It reports false, having reserved nothing, when
// the bound is reached.
//
// The counter is what lets GET /api/state distinguish "not yet scanned" from
// "empty". Until the first ListImages returns, total is 0 and a client must
// render "indexing…" rather than "0 comics"; they are different states and the
// API must not collapse them.
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

// isScanning reports whether any listing is still in flight.
func (iv *ImageViewer) isScanning() bool {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.scansInFlight > 0
}

// scanCount reports how many listings are in flight. It exists so a test can
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
// eight R1 mutation endpoints. It does NOT bound every closure on the main loop.
// Two others exist and are bounded elsewhere:
//
//   - scanImagesAsyncVia schedules its completion callback (the one that reaches
//     updateSingleImage and a 30 s S3 GET) DIRECTLY, outside this accounting. It
//     is bounded by maxConcurrentScans, and that is true only because the scan
//     slot is released INSIDE that closure rather than by the goroutine that
//     scheduled it. Round 2 released it in the goroutine's defer and asserted the
//     bound here anyway; 40 sequential rescans then left 40 closures outstanding.
//   - scheduleQuit schedules the shutdown at PRIORITY_HIGH, once per process.
//
// So the invariant is: at most maxQueuedMutations + maxConcurrentScans + 1
// control-originated closures outstanding — and it holds only while every
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
		if n, report := iv.queueRefusals.note(time.Now()); report {
			log.Printf("mutation refused: the GTK queue holds %d of %d closures; "+
				"%d refusal(s) in the last %s", iv.queueDepth(), maxQueuedMutations, n, refusalLogInterval)
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

// setSlideInterval changes the seconds between slides. It takes effect on the
// next tick; the timer is not restarted.
func (iv *ImageViewer) setSlideInterval(seconds uint) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.config.SlideInterval = seconds
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
type viewerSnapshot struct {
	total         int
	index         int
	key           string
	viewMode      ViewMode
	paused        bool
	slideInterval uint
	scanning      bool
}

// snapshot reads every exported field under a SINGLE read lock.
//
// Composing it from the individual accessors instead would take and release the
// lock seven times, and the result could describe a state the viewer was never
// in — a key from before a page turn beside an index from after it.
func (iv *ImageViewer) snapshot() viewerSnapshot {
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
	return s
}
