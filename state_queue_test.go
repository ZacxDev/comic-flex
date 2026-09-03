package main

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gotk3/gotk3/gdk"
)

// These cover the play queue's BEHAVIOUR — the part internal/control cannot
// reach, because that package has no gallery and no cursor. The wire contract
// (bounds, refusals, the always-present `queue` object) is pinned there, in
// internal/control/queue_test.go.
//
// 🔴 NONE OF THESE IS A REGRESSION TEST, and they are labelled rather than
// counted as one. The play queue is new: there is no pre-change code in which a
// defect they describe ever shipped, so "red at origin/trunk" is not available —
// the symbols do not exist there. What makes them worth their run time is the
// mutation matrix in the PR: every guard below was watched to fail against a
// deliberately broken version of the specific expression it claims to cover, with
// its own message.

// queueGallery is the fixture gallery. Five keys, so the interruption point (2)
// is neither the first nor the last, and so no arithmetic on it lands on another
// fixture's index by coincidence.
func queueGallery() []string {
	return []string{"a/1.jpg", "b/2.jpg", "c/3.jpg", "d/4.jpg", "e/5.jpg"}
}

// queueTestViewer returns a viewer sitting on c/3.jpg — index 2 — which is the
// position every drain test expects the gallery to come back to.
func queueTestViewer(t *testing.T) *ImageViewer {
	t.Helper()
	iv := newControlTestViewer(30, queueGallery()...)
	if !iv.gotoIndex(2) {
		t.Fatal("gotoIndex(2) reported nothing to show on a five-image gallery")
	}
	return iv
}

// keyNow reports the object key currently selected.
func keyNow(t *testing.T, iv *ImageViewer) string {
	t.Helper()
	_, key, ok := iv.currentKey()
	if !ok {
		t.Fatal("currentKey reported an empty gallery")
	}
	return key
}

// wantQueue asserts the three numbers GET /api/state carries.
func wantQueue(t *testing.T, iv *ImageViewer, length, position, skipped int) {
	t.Helper()
	l, p, s := iv.queueState()
	if l != length || p != position || s != skipped {
		t.Fatalf("queueState = (length %d, position %d, skipped %d), want (%d, %d, %d)",
			l, p, s, length, position, skipped)
	}
}

// TestTheQueueIsPlayedBeforeTheGallery is the core of W4: while a queue is
// running, a page turn shows the QUEUED key and not the next shuffled one.
//
// The queue is deliberately not in gallery order (e then a, against a gallery
// running a..e), so a mutant that ignored the queue and walked the gallery
// produces d/4.jpg on the first turn — a key no row below expects.
func TestTheQueueIsPlayedBeforeTheGallery(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"e/5.jpg", "a/1.jpg"})

	// Installed, nothing played: position 0 is "no queued key is selected", and
	// the display has not moved.
	wantQueue(t, iv, 2, 0, 0)
	if got := keyNow(t, iv); got != "c/3.jpg" {
		t.Fatalf("installing a queue moved the display to %q; it must not move until a page turn", got)
	}

	if !iv.advance(1) {
		t.Fatal("advance reported nothing to show with a queue installed")
	}
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("first page turn showed %q, want the first queued key e/5.jpg — the queue is not "+
			"being consumed before the gallery", got)
	}
	wantQueue(t, iv, 2, 1, 0)

	if !iv.advance(1) {
		t.Fatal("advance reported nothing to show mid-queue")
	}
	if got := keyNow(t, iv); got != "a/1.jpg" {
		t.Fatalf("second page turn showed %q, want the second queued key a/1.jpg", got)
	}
	wantQueue(t, iv, 2, 2, 0)
}

// TestADrainedQueueResumesAtTheInterruptedPosition is decision D4, and it is the
// single most mutable line in the feature.
//
// The fixture discriminates: the queue was installed at c/3.jpg (index 2) and its
// last entry is a/1.jpg (index 0). A drain that resumed from where the QUEUE left
// off would land on b/2.jpg; the correct answer, resuming from the interrupted
// position, is d/4.jpg. The two are different keys, so the mutant cannot pass.
func TestADrainedQueueResumesAtTheInterruptedPosition(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"e/5.jpg", "a/1.jpg"})
	iv.advance(1)
	iv.advance(1)

	if !iv.advance(1) {
		t.Fatal("the draining page turn reported nothing to show")
	}
	if got := keyNow(t, iv); got != "d/4.jpg" {
		t.Fatalf("after the queue drained the display shows %q, want d/4.jpg. The queue is an "+
			"INTERRUPTION, not a seek: the gallery must continue from the page it was on when "+
			"the queue arrived (c/3.jpg), not from wherever the last queued key happens to sit "+
			"in the per-process shuffle (b/2.jpg would be that answer).", got)
	}
	wantQueue(t, iv, 0, 0, 0)

	// And the gallery keeps walking normally from there.
	iv.advance(1)
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("the page turn after the drain showed %q, want e/5.jpg", got)
	}
}

// TestAQueuedKeyThatLeftTheGalleryIsSkippedAndCounted is decision D3.
//
// 🔴 The COUNT is half the decision and the half a mutant is most likely to drop:
// skipping silently is exactly what D3 exists to prevent, and a viewer that
// skipped correctly while reporting 0 would look right on the display and lie to
// the client that has to say "3 pages were no longer in the library".
func TestAQueuedKeyThatLeftTheGalleryIsSkippedAndCounted(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg"})

	if !iv.advance(1) {
		t.Fatal("advance reported nothing to show")
	}
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("the page turn showed %q, want e/5.jpg — the missing key was not skipped", got)
	}
	// position 2, not 1: the cursor is on the SECOND entry, and the skipped one
	// still occupies its place in the list the client sent.
	wantQueue(t, iv, 2, 2, 1)
}

// TestAQueueOfKeysThatAllLeftTheGalleryDrainsAndReportsEveryOne is the extreme of
// D3, and it is a state the PWA will genuinely hit: a collection assembled weeks
// ago against a bucket that has been re-scanned since.
func TestAQueueOfKeysThatAllLeftTheGalleryDrainsAndReportsEveryOne(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "gone/8.jpg", "gone/9.jpg"})

	if !iv.advance(1) {
		t.Fatal("advance reported nothing to show")
	}
	if got := keyNow(t, iv); got != "d/4.jpg" {
		t.Fatalf("a wholly-missing queue left the display on %q, want d/4.jpg — the gallery must "+
			"carry on from the interrupted position as though the queue had drained, which it has", got)
	}
	wantQueue(t, iv, 0, 0, 3)
}

// TestASkippedKeyIsCountedExactlyOnce pins the property queueScanned exists for.
//
// Stepping back and forward over a missing entry must not inflate the number the
// operator is shown, and the display looks identical either way — the count is
// the only observable. Measured against the mutant that drops the high-water
// check (`i >= iv.queueScanned` made unconditional), this fixture reports 3
// instead of 2: the forward re-pass over gone/8.jpg counts it a second time.
func TestASkippedKeyIsCountedExactlyOnce(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "gone/8.jpg", "a/1.jpg"})

	iv.advance(1) // skips gone/7, lands e/5   -> skipped 1
	iv.advance(1) // skips gone/8, lands a/1   -> skipped 2
	wantQueue(t, iv, 4, 4, 2)

	iv.advance(-1) // back over gone/8 to e/5
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("stepping back landed on %q, want e/5.jpg", got)
	}
	iv.advance(1) // forward over gone/8 again to a/1
	if got := keyNow(t, iv); got != "a/1.jpg" {
		t.Fatalf("stepping forward again landed on %q, want a/1.jpg", got)
	}

	l, p, s := iv.queueState()
	if s != 2 {
		t.Fatalf("skipped = %d after stepping back and forward over the same two missing keys, "+
			"want 2. Every pass over a missing entry is being counted, so the number the client "+
			"shows the operator grows with the arrow keys. (length %d, position %d)", s, l, p)
	}
}

// TestASecondQueueReplacesTheFirst is decision D7: newest instruction wins.
func TestASecondQueueReplacesTheFirst(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "a/1.jpg"})
	iv.advance(1) // skips one, lands e/5

	iv.setQueue([]string{"b/2.jpg", "d/4.jpg"})
	// Length is the NEW list's, not the sum: appending would report 5.
	// Skipped is back to 0: the count belongs to the queue that produced it.
	wantQueue(t, iv, 2, 0, 0)

	if !iv.advance(1) {
		t.Fatal("advance reported nothing to show after the queue was replaced")
	}
	if got := keyNow(t, iv); got != "b/2.jpg" {
		t.Fatalf("the page turn after a replacing queue showed %q, want the new queue's first key "+
			"b/2.jpg. A queue that appended, or that refused the second instruction, would carry "+
			"on with the first collection.", got)
	}
}

// TestReplacingAQueueKeepsTheOriginalInterruptionPoint is the half of D7 that
// interacts with D4, and it is invisible on the display until the SECOND queue
// drains.
//
// The gallery was interrupted once, at c/3.jpg. While a queue is running,
// currentIndex is a queued key's position in the shuffle — so a setQueue that
// re-recorded the interruption point would resume the gallery at a page from the
// collection the operator just abandoned. Here that mutant lands on e/5.jpg (the
// key showing when the replacement arrived, +1) instead of d/4.jpg.
func TestReplacingAQueueKeepsTheOriginalInterruptionPoint(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"d/4.jpg"})
	iv.advance(1) // now showing d/4.jpg, which is gallery index 3

	iv.setQueue([]string{"a/1.jpg"})
	iv.advance(1) // showing a/1.jpg
	iv.advance(1) // drains

	if got := keyNow(t, iv); got != "d/4.jpg" {
		t.Fatalf("after the replacing queue drained the gallery resumed at %q, want d/4.jpg — "+
			"the interruption point is where the gallery was when the FIRST queue arrived "+
			"(c/3.jpg), and replacing a queue does not interrupt the gallery a second time", got)
	}
}

// TestTheSkipCountOutlivesTheQueueThatProducedIt is the reporting property D3
// actually needs.
//
// A drained queue reports length 0. If the count were cleared at the same instant,
// the only client that could ever read it is one that happened to poll during the
// queue — so the message it exists for ("3 pages were no longer in the library")
// could never be shown after the collection finished playing, which is precisely
// when a client would show it.
func TestTheSkipCountOutlivesTheQueueThatProducedIt(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "gone/8.jpg"})
	iv.advance(1) // skip gone/7, land e/5
	iv.advance(1) // skip gone/8, drain

	wantQueue(t, iv, 0, 0, 2)

	// And the NEXT instruction is what clears it, not the drain.
	iv.setQueue([]string{"a/1.jpg"})
	wantQueue(t, iv, 1, 0, 0)
}

// TestPrevStepsBackThroughTheQueueAndStopsAtItsFront pins the backward arm.
//
// Prev inside a collection means "the previous page of this collection". Falling
// out of the front of a queue into the shuffled gallery would be a page turn to a
// comic the operator was not reading, so the front is a floor rather than a
// second exit — the queue ends forwards, or by an explicit goto, and nowhere else.
func TestPrevStepsBackThroughTheQueueAndStopsAtItsFront(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"b/2.jpg", "d/4.jpg", "e/5.jpg"})
	iv.advance(1)
	iv.advance(1)
	iv.advance(1)
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("three page turns landed on %q, want e/5.jpg", got)
	}

	iv.advance(-1)
	if got := keyNow(t, iv); got != "d/4.jpg" {
		t.Fatalf("stepping back landed on %q, want d/4.jpg", got)
	}
	iv.advance(-1)
	if got := keyNow(t, iv); got != "b/2.jpg" {
		t.Fatalf("stepping back twice landed on %q, want b/2.jpg", got)
	}

	// At the front: another Prev holds, and the queue is still running.
	iv.advance(-1)
	if got := keyNow(t, iv); got != "b/2.jpg" {
		t.Fatalf("stepping back off the front of the queue landed on %q, want to hold on "+
			"b/2.jpg — the front of a queue is a floor, not a second exit into the gallery", got)
	}
	wantQueue(t, iv, 3, 1, 0)
}

// TestPrevBeforeTheFirstPageTurnFallsBackToTheGallery covers the one backward
// case that legitimately leaves the queue: no entry is on the display yet, so
// there is nothing to hold on to.
func TestPrevBeforeTheFirstPageTurnFallsBackToTheGallery(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"e/5.jpg"})

	if !iv.advance(-1) {
		t.Fatal("advance reported nothing to show")
	}
	if got := keyNow(t, iv); got != "b/2.jpg" {
		t.Fatalf("a Prev before the queue had played anything landed on %q, want b/2.jpg — the "+
			"gallery moving back one from the interrupted position", got)
	}
	wantQueue(t, iv, 0, 0, 0)
}

// TestAGotoEndsTheQueue pins the only cancel path this API has, from both entry
// points.
//
// An explicit "show me this page" is the operator taking over. Leaving the queue
// installed would make the very next page turn jump back into the collection they
// just left — the display disobeying the last instruction it was given.
func TestAGotoEndsTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		goto_ func(*ImageViewer) bool
		want  string
	}{
		{"gotoKey", func(iv *ImageViewer) bool { return iv.gotoKey("b/2.jpg") }, "c/3.jpg"},
		{"gotoIndex", func(iv *ImageViewer) bool { return iv.gotoIndex(1) }, "c/3.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iv := queueTestViewer(t)
			iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "a/1.jpg"})
			iv.advance(1) // skip one, land e/5

			if !tc.goto_(iv) {
				t.Fatal("the goto reported failure on a key that is in the gallery")
			}
			// The queue is gone; the skip count is not, because it describes what
			// happened and is only reset by the next POST /api/queue.
			wantQueue(t, iv, 0, 0, 1)

			if !iv.advance(1) {
				t.Fatal("advance reported nothing to show after the goto")
			}
			if got := keyNow(t, iv); got != tc.want {
				t.Fatalf("the page turn after an explicit goto showed %q, want %q — the gallery "+
					"must continue from where the operator went, not jump back into the queue "+
					"they left", got, tc.want)
			}
		})
	}
}

// TestAFailedGotoLeavesTheQueueRunning is the other direction, and it is what
// stops the guard above from being satisfied by an unconditional clear. A goto
// for a key that is not in the gallery is a no-op: nothing was taken over, so
// there is nothing to end.
func TestAFailedGotoLeavesTheQueueRunning(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"e/5.jpg", "a/1.jpg"})
	iv.advance(1)

	if iv.gotoKey("not-in-the-bucket.jpg") {
		t.Fatal("gotoKey reported success for a key that is not in the gallery")
	}
	wantQueue(t, iv, 2, 1, 0)

	iv.advance(1)
	if got := keyNow(t, iv); got != "a/1.jpg" {
		t.Fatalf("after a FAILED goto the page turn showed %q, want the queue to carry on with "+
			"a/1.jpg", got)
	}
}

// TestAFreshViewerHasNoQueue is an INVARIANT GUARD, not regression coverage, and
// it is labelled as one: no defect has ever put a queue on a fresh viewer.
//
// It is here because it is the only place decision D2 — the queue does NOT
// survive a restart — is observable at all. A process that came back with a queue
// would have had to load it from somewhere, and there is nowhere: this asserts
// the zero value is the empty queue, so adding persistence would have to break it
// deliberately rather than by accident.
func TestAFreshViewerHasNoQueue(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	wantQueue(t, iv, 0, 0, 0)

	if got := iv.snapshot(); got.queueLength != 0 || got.queuePosition != 0 || got.queueSkipped != 0 {
		t.Fatalf("a fresh viewer's snapshot reports %+v; the queue is transient by design (D2) "+
			"and a process that started with one would have loaded it from somewhere", got)
	}
}

// TestTheSnapshotCarriesTheQueueUnderTheSameLock joins the queue to the state
// read. Without it the accessors could be right while GET /api/state reported
// zeros forever.
func TestTheSnapshotCarriesTheQueueUnderTheSameLock(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "a/1.jpg"})
	iv.advance(1)

	got := iv.snapshot()
	if got.queueLength != 3 || got.queuePosition != 2 || got.queueSkipped != 1 {
		t.Fatalf("snapshot queue = (length %d, position %d, skipped %d), want (3, 2, 1)",
			got.queueLength, got.queuePosition, got.queueSkipped)
	}
	// The rest of the snapshot still describes the same instant.
	if got.key != "e/5.jpg" || got.index != 4 {
		t.Fatalf("snapshot = (index %d, key %q), want (4, e/5.jpg)", got.index, got.key)
	}
}

// TestAQueueOnAnEmptyGalleryIsANoOpNotAPanic is the totality case: every key in
// the queue is missing because there are no keys at all, and wrapIndex is asked
// to work on a ring of zero.
func TestAQueueOnAnEmptyGalleryIsANoOpNotAPanic(t *testing.T) {
	iv := newControlTestViewer(30)
	iv.setQueue([]string{"a/1.jpg", "b/2.jpg"})

	if iv.advance(1) {
		t.Fatal("advance reported something to show on an empty gallery with a queue installed")
	}
	wantQueue(t, iv, 0, 0, 2)
	if _, _, ok := iv.currentKey(); ok {
		t.Fatal("currentKey found a key in an empty gallery")
	}
}

// TestQueueAccessIsRaceFree drives the writer, the page turn and both readers
// concurrently. It asserts nothing beyond "no data race": -race is the detector.
func TestQueueAccessIsRaceFree(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); iv.setQueue([]string{"e/5.jpg", "gone/7.jpg", "a/1.jpg"}) }()
		go func() { defer wg.Done(); iv.advance(1) }()
		go func() { defer wg.Done(); iv.queueState() }()
		go func() { defer wg.Done(); iv.snapshot() }()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// The adapter
// ---------------------------------------------------------------------------

// recordingStore records the keys a render asked for and then fails, which is how
// "what did the viewer try to display?" becomes observable with no X display.
// updateSingleImage calls LoadImage before it touches any widget and returns as
// soon as it errors.
type recordingStore struct {
	mu     sync.Mutex
	loaded []string
}

func (r *recordingStore) ListImages() ([]string, error) {
	return nil, fmt.Errorf("recordingStore does not list")
}

func (r *recordingStore) LoadImage(key string) (*gdk.Pixbuf, error) {
	r.mu.Lock()
	r.loaded = append(r.loaded, key)
	r.mu.Unlock()
	return nil, fmt.Errorf("recordingStore never loads (%s)", key)
}

func (r *recordingStore) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.loaded))
	copy(out, r.loaded)
	return out
}

// TestAdapterSetQueueTurnsToTheFirstPlayableKeyImmediately pins the decision that
// makes the endpoint usable.
//
// 🔴 Storing the list without advancing would leave the display on whatever it
// was showing until the slide timer next fired — up to slide_interval, which
// POST /api/interval allows to be 3600. An operator who tapped "play collection"
// would watch the wrong comic for an hour and reasonably report the button as
// broken, while every state-level test above passed.
//
// The render is observed through the store rather than asserted from the cursor,
// so a mutant that moved the cursor and forgot updateImage is visible.
func TestAdapterSetQueueTurnsToTheFirstPlayableKeyImmediately(t *testing.T) {
	iv := queueTestViewer(t)
	store := &recordingStore{}
	iv.store = store

	gtkViewer{iv: iv}.SetQueue([]string{"gone/7.jpg", "e/5.jpg", "a/1.jpg"})

	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("SetQueue left the display selecting %q, want the first PLAYABLE queued key "+
			"e/5.jpg", got)
	}
	if got := store.keys(); len(got) != 1 || got[0] != "e/5.jpg" {
		t.Fatalf("the store was asked for %v, want exactly [e/5.jpg] — SetQueue moved the cursor "+
			"without rendering, so the screen still shows the previous comic until the slide "+
			"timer fires (up to an hour later)", got)
	}
	wantQueue(t, iv, 3, 2, 1)
}

// TestAdapterSnapshotCarriesTheQueue is the field-by-field half: the three
// numbers have to survive the hop into control.Snapshot. They are pairwise
// distinct here for the usual reason — a mutant that crossed two of them must not
// land on the right answer.
func TestAdapterSnapshotCarriesTheQueue(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"gone/7.jpg", "gone/8.jpg", "e/5.jpg", "a/1.jpg"})
	iv.advance(1) // two skips, cursor on the third entry

	got := gtkViewer{iv: iv}.Snapshot().Queue
	if got.Length != 4 || got.Position != 3 || got.Skipped != 2 {
		t.Fatalf("adapter Snapshot().Queue = %+v, want {Length:4 Position:3 Skipped:2}", got)
	}
}
