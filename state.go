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
//
// 🔴 THE PLAY QUEUE IS CONSUMED FIRST, AND THIS IS THE ONE PLACE IT HAPPENS.
// Every page turn in the program comes through here — the slide timer, the
// arrow/space keypresses, POST /api/next and POST /api/prev — so putting the
// queue anywhere else would mean a queue the timer honours and the keyboard does
// not, or the reverse. One rule, one place.
//
// When the queue is exhausted in the forward direction it DRAINS: currentIndex
// is restored to the gallery position the queue interrupted and the same delta is
// then applied to that position (decision D4). The queue is an interruption, not
// a seek, so the gallery continues exactly where it would have been had the queue
// never run — not from wherever the last queued key happens to sit in the
// per-process shuffle, which is a position with no meaning to anyone.
func (iv *ImageViewer) advance(delta int) bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()

	if len(iv.queue) > 0 {
		if iv.advanceQueueLocked(delta) {
			return true
		}
		// Drained. Fall through to the gallery from the interrupted position.
		iv.currentIndex = iv.queueResumeIndexLocked()
		iv.endQueueLocked()
	}

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
	// While a queue is running the selection is the QUEUED PAGE, re-resolved by
	// key. See queueLeftLocked.
	if idx, ok := iv.queueLeftLocked(); ok {
		i = idx
	}
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

	// 🔴 WHILE A QUEUE IS RUNNING **BOTH** HALVES COME FROM THE QUEUE, NOT FROM
	// THE GALLERY, and each is resolved BY KEY. Taking images[l+1] for the right
	// half was measured to break the queue's one job: with the gallery a,b,c,d,e
	// and the queue [e,c,a], a two-up run showed `LEFT e RIGHT a` on the first
	// turn — playing the operator's THIRD queued page two turns early — and
	// `LEFT c RIGHT d` on the second, where d was never queued at all. Ordering
	// is the entire point of a queue, so half a screen of gallery neighbours is
	// not a cosmetic wart.
	//
	// The LEFT half goes through queueLeftLocked for the same reason the right
	// one is resolved by key: a rescan reshuffles the gallery, and an index
	// recorded before it names a different comic afterwards. Fixing only the
	// right half left `LEFT d/4 RIGHT c/3` reachable — d/4 never queued — with
	// /api/state's `key` naming it while queue.position named another page.
	//
	// queueTailIndex is the entry the page turn decided shares the screen with
	// the cursor; it is chosen in landQueueLocked, under the write lock, at the
	// same moment the cursor moves. It is NOT recomputed here: this is a read
	// path, and a right half derived independently of the one the advance planned
	// is two answers to one question — the shape Snapshot.Keys exists to remove.
	if idx, ok := iv.queueLeftLocked(); ok {
		l = idx
	}
	r := wrapIndex(l, 1, n)

	// tail == cursor (an odd-length queue's last page, or any single view) leaves
	// r == l, which displayedPair collapses to one key exactly as it does for a
	// one-image gallery.
	//
	// 🔴 THE `r = l` INITIALISER IS THE LOAD-BEARING LINE, not the assignment
	// below it. It covers the tail whose key has LEFT THE GALLERY between the page
	// turn and this render — a rescan lands and onScanComplete renders straight
	// after it — and without it that case falls back to the gallery neighbour,
	// reinstating the exact defect this block exists to remove. The odd-length
	// case cannot cover it: there tail == cursor, so the found-branch always
	// assigns and the initialiser is never read. Only a VANISHED tail exercises
	// it, and deleting this line survived a green 461-test suite until
	// TestATwoUpTailThatLeftTheGalleryCollapsesRatherThanShowingANeighbour existed.
	if len(iv.queue) > 0 && iv.queueTailIndex >= 0 && iv.queueTailIndex < len(iv.queue) {
		r = l
		if idx, found := iv.indexOfKeyLocked(iv.queue[iv.queueTailIndex]); found {
			r = idx
		}
	}
	return l, iv.images[l], r, iv.images[r], true
}

// onScanComplete runs update when a freshly scanned gallery should be shown
// immediately, i.e. nothing has been displayed yet.
//
// 🔴 The read lock is released BEFORE update runs, and folding the call back
// inside the critical section is an UNCONDITIONAL deadlock on the ordinary path —
// not, as this comment used to say, "a deadlock as soon as a writer queues
// between the two acquisitions". That wording described only the early-return
// failure paths, which take read locks; on the SUCCESS path update reaches
// updateSingleImage, which calls noteLayoutBox and noteDisplayed, and both take
// the WRITE lock. sync.RWMutex is not reentrant, so RLock-then-Lock in one
// goroutine hangs with nothing else running.
//
// Corrected here, at the site, because the understated version was read as a
// general rule and copied into rearmSlideTimer's comment — where the same audit
// caught it. Both sites are one rule: RELEASE BEFORE YOU CALL OUT.
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

// noteAdmissionRefusal logs a refusal that internal/control decided on its own,
// at most once per refusalLogInterval.
//
// 🔴 It is the THIRD admission point's voice. The other two write their lines
// from inside this package (scanImagesAsyncVia and enqueueBounded); the
// gallery-not-yet-indexed gate on POST /api/queue is decided in internal/control,
// which imports no logger deliberately, so it hands the reason back through the
// RefuseLog callback startControlAPI installs and the line is written here.
//
// Coalesced for exactly the reason the other two are: a client honouring the
// Retry-After will re-POST every few seconds for the whole first bucket listing,
// and one line per refusal is unbounded journald volume on a Raspberry Pi for a
// condition that is ONE condition.
//
// It is called from an HTTP handler goroutine, before the response is written, so
// it must not block — refusalLog has its own mutex and never touches iv.mutex,
// which is what keeps that true.
func (iv *ImageViewer) noteAdmissionRefusal(reason string) {
	if n, since, report := iv.admissionRefusals.note(time.Now()); report {
		log.Printf("%s; %d refusal(s) %s", reason, n, refusalSpan(since))
	}
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
// /api/{next,prev,pause,resume,toggle,viewmode,goto,interval,queue,queue/cancel}
// today.
//
// 🔴 THAT LIST WAS WRONG FOR TWO ROUNDS AND NOTHING CAUGHT IT: POST /api/queue
// was added to Server.enqueue and not to this sentence, in the same commit, and
// two audit rounds read past it. It is the same failure this comment's own
// preamble describes, committed by the person fixing the preamble. Grep
// `s.enqueue(` in internal/control/control.go rather than trusting the braces
// above — the enumeration is a copy, and the copy is what goes stale.
//
// It does NOT bound every closure on the main loop. Three others exist and are
// bounded elsewhere — count the bullets rather than trusting this number, which
// has been wrong once already:
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
// 🔴 The lock is RELEASED before the closure runs, and holding it would be an
// UNCONDITIONAL deadlock — not a race that needs a competing writer to lose.
// startTimer calls swapTimeout, which takes the WRITE lock, and sync.RWMutex is
// not reentrant: RLock then Lock on the same mutex in the same goroutine hangs on
// its own, with nothing else running. Measured, by holding it: a single-goroutine
// test with no other party died on sync.runtime_SemacquireRWMutex.
//
// onScanComplete above releases for exactly the same reason, and its comment now
// says so. Both sites are one rule, not two: RELEASE BEFORE YOU CALL OUT.
//
// 🔴 A paragraph here once claimed the sibling was the weaker case — that it
// "only deadlocks when a writer queues between the two acquisitions". Measured
// false, and worth recording HOW it got written: by reasoning from
// onScanComplete's own (understated) comment instead of from what its callee
// does. Correcting it only here would have left the source of the error intact,
// ~590 lines from the correction, for the next person to copy again. That is why
// the fix went to state.go's onScanComplete as well.
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
	return iv.indexOfKeyLocked(key)
}

// gotoKey selects key and reports whether it was found. A key that has since
// left the gallery is a no-op rather than an error: the caller is an enqueued
// closure with nobody left to answer.
//
// 🔴 A SUCCESSFUL GOTO ENDS ANY RUNNING PLAY QUEUE. An explicit "show me this
// page" is the operator taking over, and leaving the queue installed would make
// the very next page turn jump back into a collection the operator just left —
// the display disobeying the last thing it was told. It is also the only way to
// cancel a queue: POST /api/queue refuses an empty list rather than overloading
// itself with a second meaning.
//
// A FAILED goto changes nothing, queue included: the key was not in the gallery,
// so nothing was taken over.
func (iv *ImageViewer) gotoKey(key string) bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	i, ok := iv.indexOfKeyLocked(key)
	if !ok {
		return false
	}
	iv.currentIndex = i
	iv.endQueueLocked()
	return true
}

// gotoIndex selects an absolute position, clamped into range, and reports
// whether there is anything to show.
//
// The clamp is not belt-and-braces: the handler bounds-checked against a
// snapshot taken before this closure was queued, and a rescan in between can
// have shortened the gallery. That is defect 3's shape exactly.
//
// 🔴 Like gotoKey, a successful goto ENDS any running play queue — same reason,
// and it is stated in both places rather than in one because the two are separate
// entry points a maintainer edits separately. An empty gallery is not a goto that
// happened, so it leaves the queue alone; there is nothing to take over.
func (iv *ImageViewer) gotoIndex(index int) bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if len(iv.images) == 0 {
		iv.currentIndex = 0
		return false
	}
	iv.currentIndex = index
	iv.clampIndexLocked()
	iv.endQueueLocked()
	return true
}

// ---------------------------------------------------------------------------
// The play queue (clawgate #442 phase 2, W4).
//
// A queue is an ordered list of object keys to show BEFORE the shuffled gallery
// resumes. Everything below obeys the same rule 1 as the accessors above:
// acquire, do the smallest possible amount of work, release. Nothing here calls
// back into the viewer, shells out, or touches a widget — the `Locked` helpers
// are the exception that proves it, and they are called only from a caller that
// already holds the write lock and are never exported past this file.
//
// 🔴 IT IS TRANSIENT BY DESIGN (decision D2). There is no persistence here and
// none is wanted: a queue is a "play this now" instruction, and a Pi that came
// back from a reboot replaying a collection nobody asked for would be a defect,
// not a feature. Do not add a file, a config key or a MinIO object for it.
// ---------------------------------------------------------------------------

// setQueue REPLACES the play queue and records the gallery position it is
// interrupting, so that a drain can return to it (decision D4).
//
// Decision D7 is the `replace`: a second POST /api/queue while one is running
// wins outright, rather than appending to it or being refused. That keeps the
// state machine on the Pi to one list and one cursor, and it matches what the
// operator means by tapping "play" on a different collection.
//
// 🔴 It COPIES the caller's slice, for the reason noteDisplayed does: the slice
// arrives from a JSON decode on an HTTP goroutine and is captured by a closure,
// and retaining that backing array would let the request's memory alias state the
// snapshot reader walks under a different lock.
//
// queueSkipped is reset HERE and nowhere else. It has to outlive the queue that
// produced it — a drained queue reports length 0, and a count cleared at the same
// instant could only be read by a client that happened to poll mid-queue — so the
// next instruction is the only honest place to zero it.
func (iv *ImageViewer) setQueue(keys []string) {
	stored := make([]string, len(keys))
	copy(stored, keys)

	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	// 🔴 The interruption point is recorded ONLY when no queue is already
	// running, and that is not a micro-optimisation. currentIndex during a queue
	// is a QUEUED key's position in the shuffle, so recording it here would make
	// a second POST /api/queue overwrite the real interruption point with a page
	// from the collection the operator just abandoned — and the gallery would
	// then resume somewhere it was never interrupted. The gallery is interrupted
	// once; replacing the queue (decision D7) does not interrupt it again.
	if len(iv.queue) == 0 {
		iv.queueReturnIndex = iv.currentIndex
		// 🔴 The KEY as well as the index, because a rescan can reorder the
		// gallery under a running queue and an index then names a different
		// comic. Decision D4 says the gallery resumes at the page it was
		// interrupted on, and after a reshuffle the page and the position are
		// different facts — the position is the meaningless one. The index stays
		// as the fallback for a page that has left the bucket entirely.
		iv.queueReturnKey = ""
		if len(iv.images) > 0 {
			iv.queueReturnKey = iv.images[wrapIndex(iv.currentIndex, 0, len(iv.images))]
		}
	}
	iv.queue = stored
	// -1, not 0: nothing has been played yet, and 0 would mean "the first entry
	// is on the display" before any page turn has happened.
	iv.queueIndex = -1
	iv.queueTailIndex = -1
	iv.queueScanned = 0
	iv.queueSkipped = 0
	// The generation. It is what makes queue.skipped ATTRIBUTABLE: a drained
	// queue reports length 0 with the count still standing, and without an
	// identity a polling client cannot tell "the collection that just finished
	// skipped 2" from "some queue an hour ago skipped 2" — so it would either
	// show a stale toast on every poll or never show one at all.
	//
	// 🔴 Monotonic and unique WITHIN ONE RUN OF THE PROCESS. It starts from 0
	// again after a restart, so ids ARE reused across boots and the key a client
	// dedupes on is the PAIR (boot_id, queue.id). This sentence used to read
	// "never reused, and it starts from 0 again after a restart" — self-
	// contradicting in one line. One rule, three places (here, ImageViewer.queueSeq
	// in main.go, and control.QueueState.ID); they must agree.
	iv.queueSeq++
}

// queueResumeIndexLocked is where the gallery resumes when the queue drains: the
// PAGE the queue interrupted, re-resolved by key, falling back to the recorded
// position when that page has left the gallery. The caller must hold the lock.
func (iv *ImageViewer) queueResumeIndexLocked() int {
	if iv.queueReturnKey != "" {
		if idx, ok := iv.indexOfKeyLocked(iv.queueReturnKey); ok {
			return idx
		}
	}
	return iv.queueReturnIndex
}

// endQueueLocked clears the running queue, keeping the skip count. The caller
// must already hold the write lock.
//
// 🔴 queueSkipped is deliberately NOT cleared. See setQueue.
func (iv *ImageViewer) endQueueLocked() {
	iv.queue = nil
	iv.queueIndex = -1
	iv.queueTailIndex = -1
	iv.queueScanned = 0
	iv.queueReturnIndex = 0
	iv.queueReturnKey = ""
	// queueSeq is NOT reset either: it identifies the queue queueSkipped came
	// from, so a client can attribute a count that outlives its queue.
}

// advanceQueueLocked moves the queue on by ONE SCREEN in the direction of delta,
// skipping keys that have left the gallery, and reports whether it landed on one.
// false means the queue is exhausted forwards and the caller should return to the
// gallery. The caller must already hold the write lock.
//
// "One screen" is |delta| entries — see the page-size note in the body. Consuming
// one entry per turn regardless of the view was a measured defect, not a
// simplification: in the two-up view it played queued pages out of order and put
// unqueued gallery neighbours on half the screen.
//
// Skips are counted per decision D3, AT MOST ONCE each — a played entry that
// later leaves the gallery is counted zero times, which is the case F4 is about.
// queueScanned is the high-water mark that makes that true, and its scope is
// stated at
// scanQueueForwardLocked rather than here, because the case that matters — an
// entry that was PLAYED and then left the gallery in a mid-queue rescan — is a
// property of that scan and not of this cursor arithmetic. The backward arm
// counts nothing at all; every index it can reach is below the high-water mark by
// construction.
//
// 🔴 A BACKWARD STEP OFF THE FRONT STAYS PUT rather than draining into the
// gallery. Prev inside a collection means "the previous page of this collection";
// falling out of the front of a queue into the shuffle would be a page turn to a
// comic the operator was not reading. The queue is left over the ONE way it is
// documented to end: forward exhaustion, or an explicit POST /api/goto.
func (iv *ImageViewer) advanceQueueLocked(delta int) bool {
	// |delta| is the PAGE SIZE the caller turns by — 1 in either single view, 2
	// in the two-up view, because every caller passes ±stepSize(). The queue
	// consumes that many entries per turn, and it must: consuming ONE while the
	// screen shows TWO was measured to play the operator's third queued page on
	// the first turn.
	step := delta
	if step < 0 {
		step = -step
	}
	if step == 0 {
		step = 1
	}

	if delta >= 0 {
		// From the last entry that was ON THE SCREEN, not from the cursor. In the
		// two-up view they differ by one, and starting from the cursor would show
		// the right-hand page again as the next left-hand page.
		cursor, ok := iv.scanQueueForwardLocked(iv.queueTailIndex + 1)
		if !ok {
			return false
		}
		return iv.landQueueLocked(cursor, step)
	}

	at := iv.queueIndex
	moved := false
	for n := 0; n < step; n++ {
		prev, ok := iv.scanQueueBackwardLocked(at - 1)
		if !ok {
			break
		}
		at = prev
		moved = true
	}
	if !moved {
		// Nothing playable behind the cursor. If an entry is currently on the
		// display, hold on it; if none is (a Prev before the first page turn into
		// a freshly installed queue), there is nothing to hold and the gallery
		// takes over.
		//
		// ⚠ A HOLD REPORTS true, SO THE CALLER RE-RENDERS. gtkViewer.Prev calls
		// updateImage() on true, which re-fetches the image already on the screen
		// — one S3 GET for no visible change. That is a KNOWN, DELIBERATE cost,
		// not an oversight: it is the same shape a one-image gallery already has
		// (advance wraps to the same index and re-renders), the alternative is a
		// third return value threaded through advance for a case an operator
		// reaches by pressing prev at the front of a collection, and the honest
		// answer to "is there something to show?" here is yes. If the re-fetch
		// ever matters, the fix is a no-op check in updateImage against
		// displayedKeys, which would cover the one-image gallery too — not a
		// special case here.
		if iv.queueIndex < 0 {
			return false
		}
		// 🔴 THE ENTRY ON THE DISPLAY CAN HAVE LEFT THE GALLERY TOO — a rescan
		// under a running queue — and then there is nothing to hold ON. This is
		// the ONLY path on which landQueueLocked's re-check can fail: the forward
		// and backward arms both hand it an index a scan just resolved under this
		// same lock.
		//
		// The decision, made deliberately because two roads lead out of here and
		// the code used to take a third by accident: a Prev FALLS FORWARD to the
		// nearest still-playable entry rather than ending the queue. Draining
		// would abandon a collection that still has playable pages ahead because
		// the operator pressed BACK — the display leaving the comic they are
		// reading for an unrelated one, which is the worse of the two surprises.
		// Falling forward keeps `advanceQueueLocked`'s stated invariant TRUE
		// rather than making the docstring apologise for the code: a queue ends by
		// forward exhaustion or an explicit POST /api/goto, and by nothing else.
		//
		// ⚠ Yes, a Prev can therefore move the display FORWARD by one entry. That
		// is the honest answer when every entry at and behind the cursor has been
		// deleted from the bucket: there is no earlier page of this collection
		// left to show. It is bounded — one page turn, still inside the
		// collection — where draining is unbounded (a different comic entirely).
		//
		// 🔴 AND A PREV CAN THEREFORE RAISE queue.skipped, WHICH IS
		// CLIENT-VISIBLE. The fall-forward runs scanQueueForwardLocked, which
		// counts every entry it passes over that is above the high-water mark — so
		// a back-press can make the PWA say "2 pages were no longer in the
		// library". That is intended and correct (those pages really have left the
		// bucket, and D3 exists to report exactly that), but it is surprising
		// enough to state: the count is a property of the QUEUE, not of the
		// direction the operator was travelling, and it is the only path on which
		// a backward page turn changes it. TestAPrevDrainsOnlyWhenNoQueuedPageIsPlayableAtAll
		// asserts skipped == 1 through this path. It is written into the W5 §4.2
		// notes for the same reason: a client that treats a rising skip count as
		// "the collection is playing forward" would be wrong.
		if iv.landQueueLocked(iv.queueIndex, step) {
			return true
		}
		ahead, ok := iv.scanQueueForwardLocked(iv.queueIndex + 1)
		if !ok {
			return false
		}
		return iv.landQueueLocked(ahead, step)
	}
	return iv.landQueueLocked(at, step)
}

// scanQueueForwardLocked returns the first playable entry at or after from,
// counting every entry it passes over as a skip — AT MOST ONCE each. ok is false
// when nothing playable remains. The caller must hold the write lock.
//
// 🔴 "At most once", NOT "exactly once", and the difference is the whole of F4.
// An entry that was PLAYED and then left the gallery is counted ZERO times, not
// once: see the high-water paragraph below. The stronger summary was here for a
// round, and it is the kind that gets the conditional at the bottom of this
// function deleted as redundant — which is precisely the mutant that survived a
// green 461-test suite.
//
// 🔴 THE HIGH-WATER MARK IS WHAT MAKES "EXACTLY ONCE" TRUE, AND IT IS WIDER THAN
// "the operator pressed prev". queueScanned records that an entry has been LOOKED
// UP, whatever the answer was — so an entry that was played and then LEFT THE
// GALLERY IN A MID-QUEUE RESCAN is not counted when a later forward pass finds it
// missing. Counting it would make queue.skipped a function of how many times the
// bucket was re-listed rather than of how many pages the operator cannot see, and
// it would grow without bound on a Pi that rescans. Setting this to i instead of
// i+1 on the landing branch survived a fully green 447-test suite until
// TestARescanThatRemovesAPlayedPageDoesNotCountItTwice existed.
func (iv *ImageViewer) scanQueueForwardLocked(from int) (int, bool) {
	if from < 0 {
		from = 0
	}
	for i := from; i < len(iv.queue); i++ {
		if _, ok := iv.indexOfKeyLocked(iv.queue[i]); ok {
			if i+1 > iv.queueScanned {
				iv.queueScanned = i + 1
			}
			return i, true
		}
		if i >= iv.queueScanned {
			iv.queueSkipped++
			iv.queueScanned = i + 1
		}
	}
	return 0, false
}

// scanQueueBackwardLocked returns the nearest playable entry at or before from.
// It counts NOTHING: every index it can reach is below the cursor and has
// therefore already been looked up by a forward scan. The caller must hold the
// write lock.
func (iv *ImageViewer) scanQueueBackwardLocked(from int) (int, bool) {
	if from >= len(iv.queue) {
		from = len(iv.queue) - 1
	}
	for i := from; i >= 0; i-- {
		if _, ok := iv.indexOfKeyLocked(iv.queue[i]); ok {
			return i, true
		}
	}
	return 0, false
}

// landQueueLocked points the display at the entry at cursor and works out which
// further entries share the screen with it — step-1 of them, so one in the
// two-up view and none in either single view. It reports false when cursor's own
// key has left the gallery.
//
// 🔴 FALSE DOES NOT MEAN "DRAIN", and an earlier version of this sentence said it
// did while the code disagreed — a contradiction shipped between two comments
// twelve lines apart. The ONLY caller that can see false is advanceQueueLocked's
// backward HOLD path (the forward and backward arms hand in an index a scan just
// resolved under this same lock), and that path answers it by falling forward to
// the next playable entry, NOT by ending the queue. A queue still ends only by
// forward exhaustion or an explicit POST /api/goto.
//
// 🔴 The re-check below is therefore load-bearing, not belt-and-braces, and it is
// reachable from exactly one place. Deleting it does not fail loudly: cursor's
// key is missing, indexOfKeyLocked returns its zero value, and the display
// silently lands on GALLERY INDEX 0 — an unqueued comic — while queue.position
// goes on naming a queued page. That mutant survived a green 461-test suite until
// TestAPrevWhoseOwnPageVanishedFallsForwardInsteadOfEndingTheQueue existed.
//
// 🔴 The tail is decided HERE, under the write lock, at the same moment the
// cursor moves — and pairKeys reads it rather than recomputing one. Two
// independently-derived answers to "what is on the right?" is the divergence
// Snapshot.Keys exists to remove, one layer down.
func (iv *ImageViewer) landQueueLocked(cursor, step int) bool {
	if cursor < 0 || cursor >= len(iv.queue) {
		return false
	}
	idx, ok := iv.indexOfKeyLocked(iv.queue[cursor])
	if !ok {
		return false
	}
	iv.queueIndex = cursor
	iv.currentIndex = idx

	tail := cursor
	for n := 1; n < step; n++ {
		next, ok := iv.scanQueueForwardLocked(tail + 1)
		if !ok {
			break
		}
		tail = next
	}
	iv.queueTailIndex = tail
	return true
}

// indexOfKeyLocked is indexOfKey's body without the locking. The caller must
// already hold the lock — read or write.
//
// 🔴 It exists so the queue's per-entry resolution happens under the SAME lock
// acquisition as the currentIndex write it feeds. Calling indexOfKey from
// advanceQueueLocked would be an RLock taken while the write lock is held, which
// sync.RWMutex answers with a permanent hang (see this file's rule 1), and doing
// the lookup before the lock would resolve against a gallery a rescan can replace
// in between — defect 3's shape.
func (iv *ImageViewer) indexOfKeyLocked(key string) (int, bool) {
	for i, k := range iv.images {
		if k == key {
			return i, true
		}
	}
	return 0, false
}

// cancelQueue ends any running queue and returns the display to the page the
// queue interrupted, reporting whether a queue was actually running.
//
// 🔴 IT GOES THROUGH queueResumeIndexLocked — the SAME resume the draining page
// turn in advance() uses, key-preferred with the recorded index as the fallback.
// That is decision D4 applied to a cancel: the queue was an interruption, so
// ending it early has to undo the interruption rather than leave the display on
// a queued page or seek somewhere new. A cancel that open-coded its own restore
// would be the second copy of a predicate that is already subtle (a rescan
// reshuffles the gallery, so the recorded index names a different comic), and the
// second copy is the one that gets it wrong.
//
// 🔴 NO DELTA IS APPLIED, unlike the drain. advance() lands on
// `resume + delta` because a page turn asked for the NEXT page; a cancel is not a
// page turn — the operator asked to stop, so the display returns to the page that
// was interrupted, not to the one after it.
//
// false means no queue was running and NOTHING was touched — not even
// currentIndex. The reason is in Viewer.CancelQueue: a UI cannot see the queue
// drain between rendering its cancel button and the operator tapping it, so a
// cancel arriving after the drain must be a no-op rather than a re-seek of the
// gallery. The return value is what lets the adapter skip the render too.
//
// queueSkipped and queueSeq SURVIVE, because endQueueLocked keeps them: the
// cancelled queue's skip count is still attributable through (boot_id, queue.id),
// so a client can still report "3 pages were no longer in the library" for a
// collection the operator stopped early.
func (iv *ImageViewer) cancelQueue() bool {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	if len(iv.queue) == 0 {
		return false
	}
	iv.currentIndex = iv.queueResumeIndexLocked()
	iv.endQueueLocked()
	// The resume can be the recorded INDEX rather than a resolved key, and a
	// rescan may have shortened the gallery since it was recorded. advance()'s
	// drain path is followed by wrapIndex; this one has no delta to wrap through,
	// so it clamps explicitly.
	iv.clampIndexLocked()
	return true
}

// queueLeftLocked resolves the LEFT-hand entry the running queue is on, BY KEY,
// and reports whether it could. false means there is nothing for the queue to
// say about the selection: no queue is running, no entry is on the display yet,
// or the entry's own key has left the gallery. The caller must hold the lock.
//
// 🔴 IT IS THE ONE PLACE the queue's cursor becomes a gallery index on a READ
// path, and it exists because currentIndex goes stale. The gallery is shuffled
// once per process, so a rescan replaces it with a DIFFERENT ORDER — and the
// index landQueueLocked stored then names a different comic. Measured before this
// existed: two-up, gallery reshuffled under the queue [e/5, c/3], the screen
// showed `d/4  c/3` with d/4 never queued, and currentKey() — so GET
// /api/state's `key`, and both single views — returned d/4 while queue.position
// still named e/5.
//
// Three readers go through it (currentKey, pairKeys, snapshotAt) rather than each
// open-coding the resolution: a predicate at three call sites is wrong at two of
// them, in the same direction, and this one was wrong at three until the round
// that added it fixed only the right-hand panel.
//
// ⚠ It does NOT write currentIndex back. These are read paths holding the READ
// lock, and mutating shared state under it is a data race. currentIndex stays the
// index the page turn recorded; what a client is TOLD is what is on the screen.
func (iv *ImageViewer) queueLeftLocked() (int, bool) {
	if len(iv.queue) == 0 || iv.queueIndex < 0 || iv.queueIndex >= len(iv.queue) {
		return 0, false
	}
	return iv.indexOfKeyLocked(iv.queue[iv.queueIndex])
}

// queueState reports the generation, depth, 1-based position and skip count —
// the four numbers GET /api/state carries under "queue".
//
// position is 1-BASED, and 0 means "no queued key is selected". See
// control.QueueState, which states the wire contract; this is the one place the
// internal -1 cursor is translated, so the off-by-one cannot be made twice.
//
// In the two-up view position names the LEFT-hand entry. The right-hand one is
// deliberately not reported: a client renders "page 3 of 12" from a position, and
// a second number would only let it disagree with the screen.
func (iv *ImageViewer) queueState() (id, length, position, skipped int) {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.queueStateLocked()
}

// queueStateLocked is queueState's body. The caller must already hold the lock.
func (iv *ImageViewer) queueStateLocked() (id, length, position, skipped int) {
	length = len(iv.queue)
	if length > 0 && iv.queueIndex >= 0 {
		position = iv.queueIndex + 1
	}
	return iv.queueSeq, length, position, iv.queueSkipped
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
	// The play queue's generation, depth, 1-based position and skip count. They
	// are read under the same lock as everything above, so they cannot describe a
	// queue the viewer was not on — which is the whole reason queueID travels
	// beside queueSkipped rather than being fetched by a second call.
	queueID       int
	queueLength   int
	queuePosition int
	queueSkipped  int
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
	// Through queueStateLocked rather than open-coded here, so the 1-based
	// translation of the cursor exists once. Two copies of an off-by-one is two
	// chances to make it, and only one of them has a test pointed at it.
	s.queueID, s.queueLength, s.queuePosition, s.queueSkipped = iv.queueStateLocked()
	if s.total > 0 {
		s.index = wrapIndex(iv.currentIndex, 0, s.total)
		// The queued page, re-resolved by key, for the reason queueLeftLocked
		// states: after a rescan the recorded index names a different comic, and
		// reporting it would make `key` disagree with `queue.position`.
		if idx, ok := iv.queueLeftLocked(); ok {
			s.index = idx
		}
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
