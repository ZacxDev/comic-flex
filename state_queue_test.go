package main

import (
	"fmt"
	"reflect"
	"strings"
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

// wantQueue asserts the depth, position and skip count GET /api/state carries.
// The generation is asserted separately, by wantQueueID — it is the one number
// that is a function of how many queues this viewer has ever been given rather
// than of the one it is on, so folding it in here would make every fixture
// count its own setQueue calls.
func wantQueue(t *testing.T, iv *ImageViewer, length, position, skipped int) {
	t.Helper()
	_, l, p, s := iv.queueState()
	if l != length || p != position || s != skipped {
		t.Fatalf("queueState = (length %d, position %d, skipped %d), want (%d, %d, %d)",
			l, p, s, length, position, skipped)
	}
}

// wantQueueID asserts the generation.
func wantQueueID(t *testing.T, iv *ImageViewer, id int) {
	t.Helper()
	got, _, _, _ := iv.queueState()
	if got != id {
		t.Fatalf("queue id = %d, want %d", got, id)
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
	//
	// 🔴 That is a claim about setQueue, the STATE accessor — NOT about the
	// endpoint. POST /api/queue turns to the first queued page immediately, by
	// design (gtkViewer.SetQueue calls advance right after this), and
	// TestAdapterSetQueueTurnsToTheFirstPlayableKeyImmediately is what pins that.
	// The message says which layer it pins because a maintainer reading only the
	// failure would otherwise come away believing the endpoint is lazy.
	wantQueue(t, iv, 2, 0, 0)
	if got := keyNow(t, iv); got != "c/3.jpg" {
		t.Fatalf("setQueue moved the display to %q. Installing a queue is a STATE change only — "+
			"the page turn is the caller's, and gtkViewer.SetQueue makes it immediately "+
			"afterwards. (This does NOT say POST /api/queue is lazy; it is not.)", got)
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

	_, l, p, s := iv.queueState()
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
	// ID 1 because this viewer has been given exactly one queue. It is asserted
	// here rather than only in the state tests because the adapter is where a
	// field is most cheaply dropped: the mapping is four lines of struct literal.
	if got.ID != 1 || got.Length != 4 || got.Position != 3 || got.Skipped != 2 {
		t.Fatalf("adapter Snapshot().Queue = %+v, want {ID:1 Length:4 Position:3 Skipped:2}", got)
	}
}

// ---------------------------------------------------------------------------
// The two-up view (finding F1)
// ---------------------------------------------------------------------------

// screen returns what the two-up render would put on the display, left to right,
// through the same displayedPair the renderer records into GET /api/state's
// `keys`. It is the closest a test with no X server can get to looking at it.
func screen(t *testing.T, iv *ImageViewer) []string {
	t.Helper()
	l, left, r, right, ok := iv.pairKeys()
	if !ok {
		t.Fatal("pairKeys reported an empty gallery")
	}
	return displayedPair(l, left, r, right)
}

// TestATwoUpQueuePlaysInOrderAndShowsNothingUnqueued is the regression test for
// finding F1, and it is the auditor's exact scenario.
//
// Measured against the first implementation — which moved the cursor by ONE
// entry per turn while pairKeys took the GALLERY neighbour for the right half:
//
//	turn 1: LEFT e/5 RIGHT a/1   <- a/1 is queue[2], on screen TWO TURNS EARLY
//	turn 2: LEFT c/3 RIGHT d/4   <- d/4 was never queued at all
//	turn 3: LEFT a/1 RIGHT b/2   <- b/2 was never queued either
//
// Ordering is the entire point of a queue, so that is not a cosmetic wart in the
// right-hand panel: the collection played 1st, 3rd, 2nd, 3rd with three unqueued
// pages mixed in. Reachable at runtime from POST /api/viewmode even though
// config.yaml ships landscape_single.
func TestATwoUpQueuePlaysInOrderAndShowsNothingUnqueued(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	iv.setViewModeState(ViewLandscapeTwo)
	if got := iv.stepSize(); got != 2 {
		t.Fatalf("stepSize = %d in the two-up view, want 2 — the fixture is not exercising it", got)
	}
	iv.gotoIndex(0) // interrupted at a/1

	iv.setQueue([]string{"e/5.jpg", "c/3.jpg", "a/1.jpg"})

	for turn, want := range [][]string{
		// Two queued pages per turn, in the order they were sent.
		{"e/5.jpg", "c/3.jpg"},
		// An odd-length queue's last page has no partner. It collapses to one
		// key, exactly as a one-image gallery does — showing an unqueued comic
		// beside it is the defect this test exists for.
		{"a/1.jpg"},
	} {
		if !iv.advance(iv.stepSize()) {
			t.Fatalf("turn %d reported nothing to show", turn+1)
		}
		if got := screen(t, iv); !reflect.DeepEqual(got, want) {
			t.Fatalf("turn %d shows %v, want %v — a queued page is out of order, or half the "+
				"screen is showing a gallery neighbour nobody queued", turn+1, got, want)
		}
	}

	// Drained: back to the gallery, two pages on from the interruption at a/1.
	if !iv.advance(iv.stepSize()) {
		t.Fatal("the draining page turn reported nothing to show")
	}
	if got, want := screen(t, iv), []string{"c/3.jpg", "d/4.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the drain the screen shows %v, want %v — the gallery resumes two pages "+
			"on from the interrupted position, and both halves come from the gallery again",
			got, want)
	}
	wantQueue(t, iv, 0, 0, 0)
}

// TestATwoUpQueueAdvancesTwoEntriesPerTurn pins the cursor arithmetic directly,
// separately from what lands on the glass. An even-length queue is used so that
// the collapse case above cannot mask a cursor that moved by one.
func TestATwoUpQueueAdvancesTwoEntriesPerTurn(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	iv.setViewModeState(ViewLandscapeTwo)
	iv.gotoIndex(0)
	iv.setQueue([]string{"e/5.jpg", "d/4.jpg", "c/3.jpg", "b/2.jpg"})

	iv.advance(iv.stepSize())
	wantQueue(t, iv, 4, 1, 0) // position names the LEFT entry
	if got, want := screen(t, iv), []string{"e/5.jpg", "d/4.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first screen = %v, want %v", got, want)
	}

	iv.advance(iv.stepSize())
	wantQueue(t, iv, 4, 3, 0) // 3, not 2: a page turn is two entries here
	if got, want := screen(t, iv), []string{"c/3.jpg", "b/2.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second screen = %v, want %v — the queue advanced by one entry, so the second "+
			"screen repeats a page the first one already showed", got, want)
	}

	// And back: a Prev returns the PREVIOUS SCREEN, not a screen overlapping it.
	iv.advance(-iv.stepSize())
	if got, want := screen(t, iv), []string{"e/5.jpg", "d/4.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after Prev the screen = %v, want %v", got, want)
	}
	wantQueue(t, iv, 4, 1, 0)

	// Forward again from the front: the same second screen, not an overlap.
	iv.advance(iv.stepSize())
	if got, want := screen(t, iv), []string{"c/3.jpg", "b/2.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("forward again = %v, want %v", got, want)
	}
}

// TestATwoUpQueueSkipsMissingKeysOnBothHalves: a key that has left the gallery is
// skipped wherever it falls, including in the right-hand slot, and each is
// counted once.
func TestATwoUpQueueSkipsMissingKeysOnBothHalves(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	iv.setViewModeState(ViewLandscapeTwo)
	iv.gotoIndex(0)
	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "gone/8.jpg", "c/3.jpg"})

	iv.advance(iv.stepSize())
	if got, want := screen(t, iv), []string{"e/5.jpg", "c/3.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("screen = %v, want %v — a missing key must not leave a gallery neighbour on "+
			"the screen beside a queued one", got, want)
	}
	wantQueue(t, iv, 4, 2, 2)
}

// ---------------------------------------------------------------------------
// A rescan under a running queue (finding F4)
//
// 🔴 The whole of this block is coverage the first round did not have, and its
// absence let a mutant survive a fully green 447-test suite. state_queue_test.go
// mentioned setImages and Rescan zero times, so every claim about what happens
// when the gallery changes MID-QUEUE was a docstring and nothing else.
// ---------------------------------------------------------------------------

// TestARescanThatRemovesAPlayedPageDoesNotCountItTwice is the F4 regression test.
//
// The mutant it kills is `queueScanned = i` instead of `i + 1` on the LANDING
// branch of the forward scan, which survived the round-1 suite. It is not an
// equivalent mutant: the two values differ exactly when a forward scan later
// passes over an entry that was ALREADY PLAYED, and the only way that happens is
// a step back followed by that entry leaving the gallery — i.e. a rescan under a
// running queue, which nothing exercised.
//
// The contract being pinned: queue.skipped counts each queued key at most ONCE
// for the life of one queue. A page that played and then vanished from the bucket
// is not re-counted, because otherwise the number measures how often the Pi
// re-listed the bucket rather than how many pages the operator cannot see — and
// it grows without bound on a Pi that rescans on a timer.
func TestARescanThatRemovesAPlayedPageDoesNotCountItTwice(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"a/1.jpg", "b/2.jpg", "c/3.jpg"})

	iv.advance(1) // land a/1  (entry 0 looked up: present)
	iv.advance(1) // land b/2  (entry 1 looked up: present)
	iv.advance(-1)
	if got := keyNow(t, iv); got != "a/1.jpg" {
		t.Fatalf("stepping back landed on %q, want a/1.jpg", got)
	}
	wantQueue(t, iv, 3, 1, 0)

	// The rescan: b/2 has left the bucket while the queue is mid-play.
	iv.setImages([]string{"a/1.jpg", "c/3.jpg", "d/4.jpg", "e/5.jpg"})

	if !iv.advance(1) {
		t.Fatal("advance reported nothing to show after the rescan")
	}
	if got := keyNow(t, iv); got != "c/3.jpg" {
		t.Fatalf("after the rescan the page turn showed %q, want c/3.jpg — b/2 has left the "+
			"gallery and must be passed over", got)
	}
	_, _, _, skipped := iv.queueState()
	if skipped != 0 {
		t.Fatalf("skipped = %d after a rescan removed a page that had ALREADY PLAYED, want 0. "+
			"queue.skipped counts each queued key at most once for the life of the queue; "+
			"counting a played page when a later pass finds it gone makes the number a "+
			"function of how often the bucket was re-listed, and it grows without bound on a "+
			"Pi that rescans.", skipped)
	}
}

// TestARescanThatRemovesAnUnplayedPageCountsItOnce is the other direction, and it
// is what stops the guard above being satisfied by a viewer that never counts
// anything after a rescan.
func TestARescanThatRemovesAnUnplayedPageCountsItOnce(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"a/1.jpg", "b/2.jpg", "c/3.jpg"})
	iv.advance(1) // land a/1; b/2 and c/3 not yet looked up

	iv.setImages([]string{"a/1.jpg", "c/3.jpg", "d/4.jpg", "e/5.jpg"})

	iv.advance(1)
	if got := keyNow(t, iv); got != "c/3.jpg" {
		t.Fatalf("page turn after the rescan showed %q, want c/3.jpg", got)
	}
	wantQueue(t, iv, 3, 3, 1)

	// And not again on a second pass over it.
	iv.advance(-1)
	iv.advance(1)
	wantQueue(t, iv, 3, 3, 1)
}

// TestARescanThatEmptiesTheGalleryDrainsTheQueue: the extreme case, and the one
// that would panic if any index arithmetic here assumed a non-empty ring.
func TestARescanThatEmptiesTheGalleryDrainsTheQueue(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"a/1.jpg", "b/2.jpg"})
	iv.advance(1)

	iv.setImages(nil)

	if iv.advance(1) {
		t.Fatal("advance reported something to show after the gallery emptied")
	}
	wantQueue(t, iv, 0, 0, 1) // b/2 was still unplayed, so it counts
}

// TestADrainAfterARescanReturnsToTheSamePageNotTheSameIndex is finding F7.
//
// The gallery is shuffled once per process, so a rescan replaces it with a
// DIFFERENT ORDER. Decision D4 says the gallery resumes where the queue
// interrupted it — and after a reshuffle "the page" and "the position" are
// different facts. The position is the meaningless one: index 2 in the new order
// is an arbitrary comic. Here the interrupted page c/3.jpg moves from index 2 to
// index 1, so a drain that trusted the index resumes at x/9.jpg instead of a/1.
func TestADrainAfterARescanReturnsToTheSamePageNotTheSameIndex(t *testing.T) {
	iv := queueTestViewer(t) // interrupted at c/3.jpg, index 2
	iv.setQueue([]string{"e/5.jpg"})
	iv.advance(1)

	// A rescan that keeps c/3.jpg but moves it.
	iv.setImages([]string{"x/9.jpg", "c/3.jpg", "a/1.jpg"})

	if !iv.advance(1) {
		t.Fatal("the draining page turn reported nothing to show")
	}
	if got := keyNow(t, iv); got != "a/1.jpg" {
		t.Fatalf("after a rescan the drained queue resumed at %q, want a/1.jpg — the page after "+
			"the interrupted one (c/3.jpg, now index 1). Trusting the recorded INDEX resumes at "+
			"x/9.jpg, an arbitrary comic in the new shuffle.", got)
	}
}

// TestADrainFallsBackToTheIndexWhenTheInterruptedPageIsGone covers the other arm
// of that decision: the interrupted page has left the bucket entirely, so there
// is no key to resolve and the recorded position is the best answer left.
func TestADrainFallsBackToTheIndexWhenTheInterruptedPageIsGone(t *testing.T) {
	iv := queueTestViewer(t) // interrupted at c/3.jpg, index 2
	iv.setQueue([]string{"e/5.jpg"})
	iv.advance(1)

	// c/3.jpg is gone; the remaining four keep their relative order, so index 2
	// is now d/4.jpg and the drain lands one on from it.
	iv.setImages([]string{"a/1.jpg", "b/2.jpg", "d/4.jpg", "e/5.jpg"})

	if !iv.advance(1) {
		t.Fatal("the draining page turn reported nothing to show")
	}
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("after the interrupted page left the bucket the drain landed on %q, want "+
			"e/5.jpg — index 2 (d/4.jpg) plus one", got)
	}
}

// TestAPrevWhoseOwnPageVanishedFallsForwardInsteadOfEndingTheQueue is finding
// 🟡-1, and it is the guard that makes landQueueLocked's re-check load-bearing.
//
// Every entry at and behind the cursor has been deleted from the bucket by a
// rescan, and the operator presses Prev. There is no earlier page of the
// collection left to show. The decision: fall forward to the nearest still
// playable entry, NOT drain — draining would abandon a collection that still has
// playable pages ahead because the operator pressed BACK, taking the display to
// an unrelated comic. It also keeps advanceQueueLocked's stated invariant true: a
// queue ends by forward exhaustion or an explicit goto, and by nothing else.
//
// Deleting the re-check inside landQueueLocked does not fail loudly — the cursor
// key resolves to indexOfKeyLocked's zero value and the display lands on GALLERY
// INDEX 0, an unqueued comic, while queue.position goes on naming a queued page.
// That mutant survived a green 461-test suite.
func TestAPrevWhoseOwnPageVanishedFallsForwardInsteadOfEndingTheQueue(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"a/1.jpg", "b/2.jpg", "c/3.jpg"})
	iv.advance(1) // a/1
	iv.advance(1) // b/2

	// Both played pages leave the bucket. c/3 is still there and still unplayed.
	// d/4 is kept at gallery index 0 so that a viewer which silently fell back to
	// "index 0" would land on a key no assertion below expects.
	iv.setImages([]string{"d/4.jpg", "c/3.jpg", "e/5.jpg"})

	if !iv.advance(-1) {
		t.Fatal("a Prev reported nothing to show while the queue still had a playable page")
	}
	if got := keyNow(t, iv); got != "c/3.jpg" {
		t.Fatalf("a Prev whose own page had left the bucket landed on %q, want c/3.jpg — the "+
			"nearest still-playable queued page. d/4.jpg means it silently fell back to gallery "+
			"index 0; anything else means it dropped out of the collection entirely.", got)
	}
	// The queue is STILL RUNNING. Length 3, cursor on the third entry.
	wantQueue(t, iv, 3, 3, 0)
}

// TestAPrevDrainsOnlyWhenNoQueuedPageIsPlayableAtAll is the other arm, and it is
// what stops the guard above being satisfied by a viewer that never drains. When
// the WHOLE queue has left the bucket there is nothing to fall forward to, and
// the gallery is the only honest answer.
func TestAPrevDrainsOnlyWhenNoQueuedPageIsPlayableAtAll(t *testing.T) {
	iv := queueTestViewer(t) // interrupted at c/3.jpg
	iv.setQueue([]string{"a/1.jpg", "b/2.jpg"})
	iv.advance(1)

	iv.setImages([]string{"c/3.jpg", "d/4.jpg", "e/5.jpg"})

	if !iv.advance(-1) {
		t.Fatal("advance reported nothing to show")
	}
	// skipped 1, not 0: the fall-forward attempt walked b/2.jpg, which was queued,
	// never played, and is no longer in the library — exactly the page decision D3
	// exists to report. a/1.jpg is NOT counted, because it played before it
	// vanished (the F4 rule).
	wantQueue(t, iv, 0, 0, 1)
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("with no playable queued page left, the Prev landed on %q, want e/5.jpg — one "+
			"back in the gallery from the interrupted page c/3.jpg (now index 0)", got)
	}
}

// TestATwoUpTailThatLeftTheGalleryCollapsesRatherThanShowingANeighbour is finding
// 🟡-2: the `r = l` initialiser in pairKeys, which the round that added it called
// the whole point of the fix and then left untested.
//
// 🔴 THE ODD-LENGTH CASE CANNOT COVER IT. There tail == cursor, so the
// found-branch always assigns and the initialiser is never read. Only a tail whose
// key has LEFT THE GALLERY exercises it — reachable because onScanComplete renders
// straight after setImages — and deleting the line survived a green 461-test
// suite, reinstating exactly the gallery-neighbour fallback this block removes.
//
// The fixture keeps c/3.jpg at index 2 across the rescan, so the LEFT half is not
// in question and this test is about the right half alone.
func TestATwoUpTailThatLeftTheGalleryCollapsesRatherThanShowingANeighbour(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	iv.setViewModeState(ViewLandscapeTwo)
	iv.gotoIndex(0)
	iv.setQueue([]string{"c/3.jpg", "e/5.jpg"})
	iv.advance(iv.stepSize())
	if got, want := screen(t, iv), []string{"c/3.jpg", "e/5.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("screen = %v, want %v — the fixture is not set up as this test assumes", got, want)
	}

	// The TAIL leaves the bucket. c/3.jpg keeps index 2; d/4.jpg is its gallery
	// neighbour at index 3, and is what a fallback would put on the screen.
	iv.setImages([]string{"a/1.jpg", "b/2.jpg", "c/3.jpg", "d/4.jpg"})

	if got, want := screen(t, iv), []string{"c/3.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with the tail's page gone the screen shows %v, want %v. Showing nothing extra "+
			"is honest; falling back to the gallery neighbour puts d/4.jpg — a comic nobody "+
			"queued — on half the display, which is the defect this block exists to remove.",
			got, want)
	}
}

// TestTheQueueCursorIsResolvedByKeyNotByIndex is finding 🟡-3.
//
// The gallery is shuffled once per process, so a rescan replaces it with a
// different ORDER and the index landQueueLocked recorded then names a different
// comic. Fixing only the right-hand panel left `LEFT d/4 RIGHT c/3` reachable —
// d/4 never queued — with currentKey(), and therefore GET /api/state's `key` and
// both single views, naming d/4 while queue.position named e/5.
//
// The rescan below moves e/5.jpg from index 4 to index 1 and puts d/4.jpg where
// the stale index 4 now lands, so an index-authoritative cursor produces d/4.jpg
// and a key-authoritative one produces e/5.jpg.
func TestTheQueueCursorIsResolvedByKeyNotByIndex(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	iv.setViewModeState(ViewLandscapeTwo)
	iv.gotoIndex(0)
	iv.setQueue([]string{"e/5.jpg", "c/3.jpg"})
	iv.advance(iv.stepSize())

	iv.setImages([]string{"a/1.jpg", "e/5.jpg", "b/2.jpg", "c/3.jpg", "d/4.jpg"})

	if got, want := screen(t, iv), []string{"e/5.jpg", "c/3.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after a reshuffling rescan the two-up screen shows %v, want %v — the LEFT half "+
			"is still being taken from the stale recorded index, so a comic nobody queued is on "+
			"the display", got, want)
	}
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("currentKey() = %q, want e/5.jpg. This is what GET /api/state reports as `key` "+
			"and what BOTH single views render, so it disagreeing with queue.position is the "+
			"same defect one layer further out.", got)
	}
	got := iv.snapshot()
	if got.key != "e/5.jpg" || got.index != 1 {
		t.Fatalf("snapshot = (index %d, key %q), want (1, e/5.jpg) — `key` and `index` must "+
			"describe the queued page, not the position it used to sit at", got.index, got.key)
	}
	// And queue.position still names the same entry, so the two agree.
	wantQueue(t, iv, 2, 1, 0)
}

// TestTheQueueCursorFallsBackToTheIndexWhenItsOwnPageIsGone: the resolver reports
// "nothing to say" rather than inventing an answer, and the pre-existing
// index-based normalisation takes over. Without this arm, a resolver that
// returned (0, true) on a miss would put gallery index 0 on the display and no
// test above would see it.
func TestTheQueueCursorFallsBackToTheIndexWhenItsOwnPageIsGone(t *testing.T) {
	iv := queueTestViewer(t)
	iv.setQueue([]string{"e/5.jpg"})
	iv.advance(1)
	if got := keyNow(t, iv); got != "e/5.jpg" {
		t.Fatalf("setup: currentKey = %q, want e/5.jpg", got)
	}

	// e/5.jpg leaves. currentIndex is still 4, clamped by setImages to 2.
	iv.setImages([]string{"a/1.jpg", "b/2.jpg", "c/3.jpg"})

	if got := keyNow(t, iv); got != "c/3.jpg" {
		t.Fatalf("currentKey = %q, want c/3.jpg — with the queued page gone there is nothing for "+
			"the queue to resolve, so the clamped index answers", got)
	}
}

// TestASkippedEntryAtTheEndOfAScreenIsNotCountedAgain pins the SKIP branch's
// high-water write, which the landing-branch test (F4) cannot reach.
//
// 🔴 Two-up is the only view that reaches it. The tail scan runs off the end of
// the queue without landing, so the last write to queueScanned comes from the
// SKIP branch — and on the next page turn, which starts at tail+1, those same
// entries are walked again. With `queueScanned = i` there they are counted twice;
// with `i + 1` they are not. In a single view a skip is always followed by a
// landing or by a drain, so the value is overwritten either way and the mutant is
// equivalent.
func TestASkippedEntryAtTheEndOfAScreenIsNotCountedAgain(t *testing.T) {
	iv := newControlTestViewer(30, queueGallery()...)
	iv.setViewModeState(ViewLandscapeTwo)
	iv.gotoIndex(0)
	iv.setQueue([]string{"e/5.jpg", "gone/7.jpg", "gone/8.jpg"})

	iv.advance(iv.stepSize()) // lands e/5, tail scan skips both and finds nothing
	wantQueue(t, iv, 3, 1, 2)

	// The next turn starts at tail+1 == 1 and walks the same two entries again.
	iv.advance(iv.stepSize())
	_, _, _, skipped := iv.queueState()
	if skipped != 2 {
		t.Fatalf("skipped = %d after a second page turn walked the same two missing entries, "+
			"want 2. The skip branch is not recording that it looked them up, so every page "+
			"turn re-counts the tail of the queue.", skipped)
	}
}

// ---------------------------------------------------------------------------
// The queue generation and the boot identity (findings F2 and 🟡-5)
// ---------------------------------------------------------------------------

// TestBootIDIsStableAndNonEmpty pins the half of the (boot_id, queue.id) dedupe
// key that lives in this package.
//
// queue.id is a per-process counter, so it IS reused after a restart — a client
// that deduped on it alone would suppress a real "3 pages were no longer in the
// library" notification from a rebooted Pi's third collection. boot_id is what
// makes the pair unique. It must never be empty (a client cannot branch on an
// empty string any more than on an absent field) and must not change while the
// process runs (a value that moved would make every notification look new).
func TestBootIDIsStableAndNonEmpty(t *testing.T) {
	if bootID == "" {
		t.Fatal("bootID is empty — a client has nothing to pair queue.id with, and the id is " +
			"reused after every restart")
	}
	iv := queueTestViewer(t)
	first := gtkViewer{iv: iv}.Snapshot().BootID
	iv.setQueue([]string{"e/5.jpg"})
	iv.advance(1)
	second := gtkViewer{iv: iv}.Snapshot().BootID
	if second != first || first != bootID {
		t.Fatalf("boot id moved during the process: %q then %q (package value %q). It identifies "+
			"the RUN, so a value that changes makes every queue look like a new boot",
			first, second, bootID)
	}
}

// TestNewBootIDDoesNotRepeat is the other half: two runs of the generator must
// differ, or the field cannot distinguish the reboot it exists for. It exercises
// the generator rather than the package variable, because the variable is
// initialised once and a test cannot restart the process.
// 🔴 WITHIN-PROCESS UNIQUENESS IS THE WEAKER HALF, AND ON ITS OWN IT IS NEARLY
// VACUOUS. The property that matters is uniqueness ACROSS REBOOTS, which no unit
// test can observe — and the clock fallback satisfies the loop below perfectly
// while failing the real property, because a Pi has no battery-backed RTC and can
// come up at the same wall clock twice. Measured: a mutant forcing the fallback
// (`if _, err := crand.Read(b[:]); true`) SURVIVED this test when it asserted
// non-repetition alone.
//
// So the SHAPE is asserted as well: 16 lowercase hex characters is what the
// crypto/rand path produces, and the fallback deliberately prefixes "t" so the
// two are distinguishable. That turns "the primary source is the one running"
// into something a test can see.
func TestNewBootIDDoesNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := newBootID()
		if id == "" {
			t.Fatal("newBootID returned an empty string")
		}
		if len(id) != 16 {
			t.Fatalf("newBootID = %q (%d chars), want 16 hex characters. A different shape means "+
				"the clock FALLBACK is answering, and the clock repeats across a reboot on a Pi "+
				"with no RTC — which is exactly the event this id exists to distinguish.",
				id, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("newBootID = %q contains %q, which is not lowercase hex — the "+
					"crypto/rand path is not the one producing it", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("newBootID repeated %q within %d calls — it cannot distinguish one boot "+
				"from the next, which is the only thing it is for", id, i+1)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// The queue generation (finding F2)
// ---------------------------------------------------------------------------

// TestTheQueueGenerationMakesTheSkipCountAttributable is the F2 regression test.
//
// queue.skipped deliberately outlives the queue that produced it, so after a
// drain GET /api/state reports `{length:0, skipped:2}` — and goes on reporting it
// through fifty unrelated page turns. Without an identity beside it, a polling
// client cannot tell "the collection that just finished skipped 2 pages" from
// "some queue an hour ago skipped 2", so it either shows that toast on every poll
// forever or never shows it at all. The generation is what lets it show it once.
func TestTheQueueGenerationMakesTheSkipCountAttributable(t *testing.T) {
	iv := queueTestViewer(t)
	wantQueueID(t, iv, 0) // nothing has ever been queued in this process

	iv.setQueue([]string{"gone/7.jpg", "e/5.jpg", "gone/8.jpg"})
	wantQueueID(t, iv, 1)
	iv.advance(1) // skip gone/7, land e/5
	iv.advance(1) // skip gone/8, drain

	wantQueue(t, iv, 0, 0, 2)
	wantQueueID(t, iv, 1)

	// Fifty unrelated page turns later, the count still stands — and is still
	// attributable to queue 1, which is what makes standing acceptable.
	for i := 0; i < 50; i++ {
		iv.advance(1)
	}
	wantQueue(t, iv, 0, 0, 2)
	wantQueueID(t, iv, 1)

	// A new instruction is a new generation with a clean count.
	iv.setQueue([]string{"a/1.jpg"})
	wantQueueID(t, iv, 2)
	wantQueue(t, iv, 1, 0, 0)
}

// TestTheQueueGenerationIsNotReusedOrRolledBack: it counts POST /api/queue calls,
// not running queues, so ending one does not give its id back.
func TestTheQueueGenerationIsNotReusedOrRolledBack(t *testing.T) {
	iv := queueTestViewer(t)
	for want := 1; want <= 3; want++ {
		iv.setQueue([]string{"e/5.jpg"})
		wantQueueID(t, iv, want)
	}
	iv.advance(1)
	iv.gotoKey("a/1.jpg") // ends the queue
	wantQueue(t, iv, 0, 0, 0)
	wantQueueID(t, iv, 3)
	iv.setQueue([]string{"e/5.jpg"})
	wantQueueID(t, iv, 4)
}
