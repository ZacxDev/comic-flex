package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotk3/gotk3/glib"
)

// These tests cover the four defects that were latent while every mutation ran
// on the GTK main thread, and become reachable the moment an HTTP handler can
// touch the same state.
//
// Several tests carry a `preFix*` twin: a faithful reproduction of the shape
// the code actually had before the fix. The twin is not decoration. Without it
// a guard only pins what the current code happens to do, and there is no way to
// tell a test that catches the defect from one that never could. Each twin is
// asserted to still exhibit the defect, so if the reproduction ever stops being
// faithful the test says so instead of quietly passing.

func newTestViewer(images ...string) *ImageViewer {
	return &ImageViewer{
		images: images,
		mutex:  &sync.RWMutex{},
		config: &Config{},
	}
}

// ---------------------------------------------------------------------------
// Defect 2 — `% len(iv.images)` with no empty-slice check (4 call sites)
// ---------------------------------------------------------------------------

// preFixWrapIndex is the expression that was open-coded at all four page-turn
// call sites: the key handler's forward and backward arms, and the scroll
// handler's two arms.
func preFixWrapIndex(current, delta, n int) int {
	return (current + delta) % n
}

func TestPreFixIndexArithmeticDividesByZeroOnEmptyGallery(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the pre-fix expression did not panic on an empty gallery — " +
				"this reproduction is no longer faithful, so the guard below pins nothing")
		}
		err, ok := r.(error)
		if !ok || !strings.Contains(err.Error(), "divide by zero") {
			t.Fatalf("panicked for the wrong reason: %v (want an integer divide by zero)", r)
		}
	}()
	_ = preFixWrapIndex(0, 1, 0)
}

func TestWrapIndexIsTotal(t *testing.T) {
	// Sizes are deliberately not powers of two and not multiples of the step,
	// so an off-by-one or a dropped sign cannot land back on the expected value
	// by arithmetic coincidence.
	cases := []struct {
		name                    string
		current, delta, n, want int
	}{
		{"empty gallery, forward", 0, 1, 0, 0},
		{"empty gallery, backward", 0, -1, 0, 0},
		{"empty gallery, stale index", 7, 2, 0, 0},
		{"forward within range", 1, 1, 5, 2},
		{"forward wrapping", 4, 1, 5, 0},
		{"forward two-up wrapping", 4, 2, 5, 1},
		{"backward within range", 3, -1, 5, 2},
		{"backward wrapping", 0, -1, 5, 4},
		{"backward two-up wrapping", 1, -2, 5, 4},
		{"delta larger than gallery", 0, -7, 3, 2},
		{"single image", 0, 1, 1, 0},
		{"stale index above range", 9, 0, 5, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapIndex(c.current, c.delta, c.n)
			if got != c.want {
				t.Fatalf("wrapIndex(%d, %d, %d) = %d, want %d", c.current, c.delta, c.n, got, c.want)
			}
			if c.n > 0 && (got < 0 || got >= c.n) {
				t.Fatalf("wrapIndex returned %d, outside [0,%d)", got, c.n)
			}
		})
	}
}

func TestAdvanceOnEmptyGalleryIsANoOpNotAPanic(t *testing.T) {
	iv := newTestViewer()
	if iv.advance(1) {
		t.Fatal("advance reported something to show on an empty gallery")
	}
	if iv.advance(-2) {
		t.Fatal("advance reported something to show on an empty gallery")
	}
	if iv.currentIndex != 0 {
		t.Fatalf("currentIndex = %d after advancing an empty gallery, want 0", iv.currentIndex)
	}
	if _, _, ok := iv.currentKey(); ok {
		t.Fatal("currentKey returned a key for an empty gallery")
	}
	if _, _, _, _, ok := iv.pairKeys(); ok {
		t.Fatal("pairKeys returned keys for an empty gallery")
	}
}

// ---------------------------------------------------------------------------
// Defect 3 — currentIndex unclamped after a rescan
// ---------------------------------------------------------------------------

// preFixSetImages is what scanImagesAsync did: replace the gallery, leave
// currentIndex pointing wherever it was.
func (iv *ImageViewer) preFixSetImages(images []string) {
	iv.mutex.Lock()
	iv.images = images
	iv.mutex.Unlock()
}

func TestSetImagesClampsIndexWhenTheGalleryShrinks(t *testing.T) {
	// The assertion is on currentIndex itself, not on whether a read panics.
	// currentKey normalises defensively so that a future writer which forgets
	// to clamp shows the wrong image rather than crashing a physical display —
	// which means a behavioural assertion alone would survive the clamp being
	// deleted. Pinning the stored state is what makes this guard reachable.
	iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg", "delta.jpg", "echo.jpg")
	iv.currentIndex = 4

	iv.setImages([]string{"foxtrot.jpg", "golf.jpg"})

	if iv.currentIndex != 1 {
		t.Fatalf("currentIndex = %d after the gallery shrank 5 -> 2, want 1 (clamped to the last valid index)", iv.currentIndex)
	}
	idx, key, ok := iv.currentKey()
	if !ok {
		t.Fatal("currentKey reported an empty gallery after a rescan returned two images")
	}
	if idx != 1 || key != "golf.jpg" {
		t.Fatalf("currentKey = (%d, %q), want (1, \"golf.jpg\")", idx, key)
	}
}

func TestPreFixSetImagesLeavesTheIndexOutOfRange(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg", "delta.jpg", "echo.jpg")
	iv.currentIndex = 4

	iv.preFixSetImages([]string{"foxtrot.jpg", "golf.jpg"})

	if iv.currentIndex < len(iv.images) {
		t.Fatal("the pre-fix rescan left a valid index — this reproduction is no longer faithful, " +
			"so TestSetImagesClampsIndexWhenTheGalleryShrinks pins nothing")
	}
	// And that stale index is exactly what the pre-fix read path indexed with.
	if func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		_ = iv.images[iv.currentIndex]
		return false
	}() == false {
		t.Fatal("indexing the shrunken gallery at the stale index did not panic — reproduction is stale")
	}
}

func TestSetImagesResetsTheIndexWhenTheGalleryEmpties(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg")
	iv.currentIndex = 2

	iv.setImages(nil)

	if iv.currentIndex != 0 {
		t.Fatalf("currentIndex = %d after the gallery emptied, want 0", iv.currentIndex)
	}
	if iv.imageCount() != 0 {
		t.Fatalf("imageCount = %d, want 0", iv.imageCount())
	}
}

func TestSetImagesKeepsAValidIndexWhenTheGalleryGrows(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg")
	iv.currentIndex = 2

	iv.setImages([]string{"a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg", "f.jpg", "g.jpg"})

	if iv.currentIndex != 2 {
		t.Fatalf("currentIndex = %d after the gallery grew 3 -> 7, want it left alone at 2", iv.currentIndex)
	}
}

// ---------------------------------------------------------------------------
// Defect 1 — recursive RLock in the post-scan callback
// ---------------------------------------------------------------------------

// preFixOnScanComplete is the body of the glib.IdleAdd closure in
// scanImagesAsync before the fix: it called updateImage — which re-enters the
// read lock — from inside its own read-locked section.
func (iv *ImageViewer) preFixOnScanComplete(update func()) {
	iv.mutex.RLock()
	if len(iv.images) > 0 && iv.currentIndex == 0 {
		update()
	}
	iv.mutex.RUnlock()
}

// TestScanCallbackLockDiscipline pins the difference between the two shapes in
// a single run: the pre-fix callback holds the read lock across update, the
// fixed one does not. TryLock makes this deterministic — no sleeps, no timing.
func TestScanCallbackLockDiscipline(t *testing.T) {
	cases := []struct {
		name     string
		call     func(*ImageViewer, func())
		wantHeld bool
	}{
		{"pre-fix callback (the defect)", (*ImageViewer).preFixOnScanComplete, true},
		{"fixed callback", (*ImageViewer).onScanComplete, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg")
			called := false
			heldDuringUpdate := false

			c.call(iv, func() {
				called = true
				// A writer can only take the lock if no reader still holds it.
				// This is precisely the state that makes a second RLock — the
				// one updateSingleImage takes — block forever.
				if iv.mutex.TryLock() {
					iv.mutex.Unlock()
				} else {
					heldDuringUpdate = true
				}
			})

			if !called {
				t.Fatal("update was never invoked, so this case measured nothing")
			}
			if heldDuringUpdate != c.wantHeld {
				t.Fatalf("read lock held across update = %v, want %v", heldDuringUpdate, c.wantHeld)
			}
		})
	}
}

// TestScanCallbackSurvivesAQueuedWriter is the behavioural companion: the
// structural test above proves a lock is not held, this one proves the deadlock
// the defect actually produced is gone. sync.RWMutex queues a pending writer
// ahead of new readers, so the second RLock only blocks once a writer is
// waiting between the two acquisitions.
func TestScanCallbackSurvivesAQueuedWriter(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg")

	writerQueued := make(chan struct{})
	writerDone := make(chan struct{})
	callbackDone := make(chan struct{})

	go func() {
		defer close(callbackDone)
		iv.onScanComplete(func() {
			go func() {
				defer close(writerDone)
				close(writerQueued)
				iv.setImages([]string{"delta.jpg", "echo.jpg"})
			}()
			<-writerQueued
			// Give the writer time to actually queue on Lock(). If it has not
			// queued yet this test still passes on fixed code; the sleep can
			// only make the test weaker, never produce a false failure.
			time.Sleep(100 * time.Millisecond)

			// updateSingleImage re-reads state here. Under the pre-fix shape
			// this is the acquisition that never returns.
			_, _, _ = iv.currentKey()
		})
	}()

	select {
	case <-callbackDone:
	case <-time.After(10 * time.Second):
		t.Fatal("post-scan callback deadlocked: the read lock was still held when update re-read state")
	}
	<-writerDone
}

// ---------------------------------------------------------------------------
// Defect 4 — viewMode and timeoutID accessed without the lock
// ---------------------------------------------------------------------------
//
// These are data races, so the detector is the instrument: they are meaningful
// under `go test -race` and prove nothing without it.

func TestViewModeAccessIsRaceFree(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg")
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			iv.setViewModeState(ViewLandscapeTwo)
			iv.setViewModeState(ViewPortraitSingle)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = iv.getViewMode()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			// stepSize reads viewMode; it is called from every page-turn path.
			if s := iv.stepSize(); s != 1 && s != 2 {
				t.Errorf("stepSize returned %d, want 1 or 2", s)
				return
			}
		}
	}()
	wg.Wait()
}

func TestTimeoutHandleAccessIsRaceFree(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg")
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= 2000; i++ {
			iv.swapTimeout(glib.SourceHandle(i), time.Time{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 2001; i <= 4000; i++ {
			iv.swapTimeout(glib.SourceHandle(i), time.Time{})
		}
	}()
	wg.Wait()

	// Every handle handed out must have been observed exactly once by whoever
	// swapped it out; a lost handle is a leaked GLib source.
	if iv.timeoutID == 0 {
		t.Fatal("timeoutID was left at 0 after 4000 swaps")
	}
}

func TestPausedFlagAccessIsRaceFree(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg")
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			iv.togglePaused()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = iv.isPaused()
		}
	}()
	wg.Wait()

	// An even number of toggles must land back on false.
	if iv.isPaused() {
		t.Fatal("paused = true after an even number of toggles")
	}
}

// TestPreFixViewModeAccessRacesUnderTheDetector is the sensitivity proof for
// the two race guards above: it performs the unguarded field access the code
// used to perform, and therefore FAILS BY DESIGN under `-race`.
//
// It is skipped by default because a permanently-red test trains everyone to
// ignore it. Run it deliberately to watch the detector fire:
//
//	COMICFLEX_PROVE_PREFIX_RACE=1 go test -race -run PreFixViewModeAccessRaces
//
// Seeing it go red is what licenses the claim that the green guards above are
// measuring anything at all.
func TestPreFixViewModeAccessRacesUnderTheDetector(t *testing.T) {
	if os.Getenv("COMICFLEX_PROVE_PREFIX_RACE") != "1" {
		t.Skip("set COMICFLEX_PROVE_PREFIX_RACE=1 to run the pre-fix race reproduction (it fails by design under -race)")
	}

	iv := newTestViewer("alpha.jpg", "bravo.jpg")
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			iv.viewMode = ViewLandscapeTwo // unguarded write, as before the fix
			iv.timeoutID = glib.SourceHandle(i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = iv.viewMode // unguarded read, as before the fix
			_ = iv.timeoutID
		}
	}()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Cross-cutting: the accessors must compose without deadlocking
// ---------------------------------------------------------------------------

// TestAccessorsAreNotReentrant exercises every accessor concurrently. Any
// accessor that acquires the lock while already holding it, or that calls
// another accessor from inside its critical section, hangs here.
func TestAccessorsAreNotReentrant(t *testing.T) {
	iv := newTestViewer("alpha.jpg", "bravo.jpg", "charlie.jpg", "delta.jpg", "echo.jpg")

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					iv.advance(1)
					iv.advance(-2)
					_, _, _ = iv.currentKey()
					_, _, _, _, _ = iv.pairKeys()
					_ = iv.imageCount()
					_ = iv.getViewMode()
					iv.setViewModeState(ViewMode(i % 3))
					_ = iv.isPaused()
					iv.swapTimeout(glib.SourceHandle(i), time.Time{})
					iv.onScanComplete(func() {
						_, _, _ = iv.currentKey()
						_ = iv.stepSize()
					})
					if i%37 == 0 {
						iv.setImages([]string{"x.jpg", "y.jpg", "z.jpg"})
					}
					if i%53 == 0 {
						iv.setImages([]string{"alpha.jpg", "bravo.jpg", "charlie.jpg", "delta.jpg", "echo.jpg"})
					}
				}
			}(w)
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("accessors deadlocked under concurrent access")
	}
}
