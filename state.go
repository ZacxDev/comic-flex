package main

import (
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

// setScanning records whether a bucket listing is in flight.
//
// This is what lets GET /api/state distinguish "not yet scanned" from "empty".
// Until the first ListImages returns, total is 0 and a client must render
// "indexing…" rather than "0 comics"; they are different states and the API
// must not collapse them.
func (iv *ImageViewer) setScanning(scanning bool) {
	iv.mutex.Lock()
	defer iv.mutex.Unlock()
	iv.scanning = scanning
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
		scanning:      iv.scanning,
	}
	if s.total > 0 {
		s.index = wrapIndex(iv.currentIndex, 0, s.total)
		s.key = iv.images[s.index]
	}
	return s
}
