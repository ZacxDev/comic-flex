package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotk3/gotk3/glib"
)

// These cover the two fields GET /api/state grew for the companion PWA:
//
//	keys                — every object key CURRENTLY ON THE DISPLAY, left to
//	                      right, read from the renderer's own record.
//	seconds_until_next  — whole seconds to the next automatic advance, derived
//	                      from the instant the slide timer was armed.
//
// They live in package main because that is where both facts are produced: the
// render paths write one, startSlideshow writes the other, and snapshot() reads
// both under the single lock the API contract depends on. internal/control's own
// tests can only see the wire shape — it has no gallery and no renderer — so
// pinning the SOURCE of `keys` has to happen here.

// ---------------------------------------------------------------------------
// keys — the renderer's record, not the handler's arithmetic
// ---------------------------------------------------------------------------

// TestKeysComeFromTheRendererNotFromTheIndex is THE guard for this field, and it
// is the one to run a mutant against before believing any of the others.
//
// 🔴 The whole reason `keys` exists is that a consumer cannot compute it. The Pi
// shuffles its gallery per process (config is_random_order), so the PWA's
// ordering is not the Pi's, and "the image after the one at index N" is a guess
// the browser is explicitly forbidden from making. An implementation of snapshot()
// that rebuilt keys from currentIndex and viewMode —
//
//	s.keys = []string{iv.images[s.index]}
//	if iv.viewMode == ViewLandscapeTwo && s.total > 1 {
//	    s.keys = append(s.keys, iv.images[wrapIndex(s.index, 1, s.total)])
//	}
//
// — would satisfy every plausible-looking assertion ("keys is non-empty", "keys
// has two elements in two-up mode", "keys[0] == key") while moving the guessed
// answer from the browser into the Pi and stamping it as truth. So the fixture
// below puts the RENDERER's record and the INDEX's arithmetic deliberately far
// apart: the selection is at the front of the gallery, the frame on the glass is
// from the middle, and the two share no element.
//
// That state is not contrived. It is what every page turn passes through — the
// index moves first and the render completes up to 30 s later, or never, if the
// S3 GET fails — and it is the state in which a consumer most needs the truth.
func TestKeysComeFromTheRendererNotFromTheIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode ViewMode
		// rendered is what the render path recorded when its pixbuf landed.
		rendered []string
		// recomputed is what a snapshot() that derived keys from the index would
		// produce for this fixture. Naming it makes the mutant's answer explicit
		// rather than leaving "not equal to rendered" to cover everything.
		recomputed []string
	}{
		{
			name:       "single view",
			mode:       ViewLandscapeSingle,
			rendered:   []string{"c.jpg"},
			recomputed: []string{"a.jpg"},
		},
		{
			name:       "two-up view",
			mode:       ViewLandscapeTwo,
			rendered:   []string{"c.jpg", "d.jpg"},
			recomputed: []string{"a.jpg", "b.jpg"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iv := newControlTestViewer(30, "a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg")
			iv.setViewModeState(tc.mode)

			// The render that completed put these on the screen.
			iv.noteDisplayed(tc.rendered)
			// A page turn then moved the SELECTION, and no render has completed
			// since: the image load is still in flight, or it failed.
			if !iv.gotoIndex(0) {
				t.Fatal("gotoIndex reported nothing to show on a five-image gallery")
			}

			got := iv.snapshot()

			if reflect.DeepEqual(got.keys, tc.recomputed) {
				t.Fatalf("keys = %v, which is exactly what recomputing from index %d would "+
					"produce — snapshot() is deriving the displayed keys instead of reading "+
					"the renderer's record, which reintroduces the client-side guess this "+
					"field exists to remove", got.keys, got.index)
			}
			if !reflect.DeepEqual(got.keys, tc.rendered) {
				t.Fatalf("keys = %v, want %v — snapshot() is not reporting what the renderer "+
					"recorded", got.keys, tc.rendered)
			}
			// The transient this fixture builds, stated so nobody "fixes" it: the
			// selection has moved ahead of the frame, so key is NOT keys[0] here.
			// key is where the slideshow is; keys is what is lit.
			if got.key != "a.jpg" {
				t.Fatalf("key = %q, want a.jpg — the selection is not being reported", got.key)
			}
		})
	}
}

// TestKeysIsEmptyBeforeAnythingHasBeenRendered pins the boot state, which is the
// one every client meets first and the one no populated fixture exercises.
func TestKeysIsEmptyBeforeAnythingHasBeenRendered(t *testing.T) {
	// A gallery that has been LISTED but not yet rendered: total is non-zero and
	// a key is selected, and still nothing is on the glass.
	iv := newControlTestViewer(30, "a.jpg", "b.jpg")
	got := iv.snapshot()
	if len(got.keys) != 0 {
		t.Fatalf("keys = %v before any render completed, want empty — a client would draw a "+
			"comic that is not on the display", got.keys)
	}
	if got.key != "a.jpg" || got.total != 2 {
		t.Fatalf("snapshot = %+v; the fixture no longer distinguishes 'listed' from 'rendered'", got)
	}

	// And an entirely empty viewer likewise.
	if got := newControlTestViewer(30).snapshot(); len(got.keys) != 0 {
		t.Fatalf("keys = %v on an empty gallery, want empty", got.keys)
	}
}

// TestTheEmptiedGalleryStatesAreReachableAndDistinct pins the two wire states
// that Snapshot's contract describes as MEASURED but that nothing asserted.
//
// 🔴 An unpinned "Measured:" line in a comment is a claim with no guard behind
// it, and this one is load-bearing: the contract tells a PWA author to branch on
// `!Scanning && Total == 0 && len(Keys) > 0` — "the bucket is empty but the
// display still holds the last page". A plausible future tidy-up — clearing
// displayedKeys inside setImages, which looks like obvious hygiene — would make
// that state UNREACHABLE, turn the client's branch into dead code, and leave all
// 384 tests green. Nothing else in the suite reaches Total == 0 with keys
// populated.
//
// It also pins the two states APART, which is the property the partition in the
// contract turns on: "no comics" and "empty bucket, last page still lit" must not
// serialize identically, exactly as scanning-vs-empty must not.
func TestTheEmptiedGalleryStatesAreReachableAndDistinct(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg")
	iv.noteDisplayed([]string{"a.jpg"}) // a render completed and lit a page

	// A rescan returns an empty bucket. setImages must NOT touch what is on the
	// glass — nothing cleared the screen, so the last page is still there.
	iv.setImages(nil)

	lit := iv.snapshot()
	if lit.total != 0 || lit.key != "" {
		t.Fatalf("snapshot = %+v, want total 0 and no key after the gallery emptied", lit)
	}
	if !reflect.DeepEqual(lit.keys, []string{"a.jpg"}) {
		t.Fatalf("keys = %v after the gallery emptied, want [a.jpg] — the last rendered page "+
			"is still on the display, and the contract tells a client to branch on exactly "+
			"this state; if setImages now clears displayedKeys, that branch is dead code",
			lit.keys)
	}

	// The genuine "no comics": nothing listed AND nothing ever rendered.
	dark := newControlTestViewer(30).snapshot()
	if dark.total != 0 || len(dark.keys) != 0 {
		t.Fatalf("snapshot = %+v, want total 0 and no keys", dark)
	}
	if reflect.DeepEqual(lit.keys, dark.keys) {
		t.Fatal("'the bucket is empty but a page is still lit' and 'no comics' produce " +
			"identical keys — a client cannot tell them apart, and one of them has a comic " +
			"on the screen")
	}
}

// TestAPausedRescanLeavesKeyAndKeysDisagreeingIndefinitely pins the other
// measured claim: the divergence window between Key and Keys is UNBOUNDED, not
// "up to a 30 s image load" as an earlier version of the contract said.
//
// 🔴 Two independent gates conspire, and the comment is only true because BOTH
// hold: startSlideshow re-renders only `if !iv.isPaused()`, and onScanComplete
// re-renders only when `currentIndex == 0`. So a rescan that lands while paused
// at a non-zero index leaves a key on the wire that is no longer in the gallery
// at all, until an operator does something.
//
// 🔴 SCOPE — THIS TEST PINS ONE OF THE TWO GATES, NOT BOTH. An earlier version
// of this docstring said "if either gate changes, this test fails", and that was
// measured FALSE: replacing startSlideshow's `if !iv.isPaused()` with `if true`
// LEFT the whole suite at 386 PASS / 0 FAIL — past tense, because that mutant is
// killed now and a reader re-running it today sees 386 PASS / 1 FAIL. The PASS
// count is unchanged between the two, so only the FAIL distinguishes them; state
// historical measurements in the past tense, as the rest of this repo does.
// This test never runs
// startSlideshow — it checks the paused FLAG, which is not the branch that reads
// it. The pause gate lives inside a glib.TimeoutAdd closure and cannot be reached
// headlessly at all, so it is pinned structurally instead, by
// TestTheSlideshowTimerStillHonoursThePauseFlag below. Whichever way it is
// pinned, do not restate the wider claim here.
//
// What THIS test pins is the onScanComplete gate, behaviourally.
func TestAPausedRescanLeavesKeyAndKeysDisagreeingIndefinitely(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg")
	iv.setPausedState(true)
	if !iv.gotoIndex(1) {
		t.Fatal("gotoIndex failed")
	}
	iv.noteDisplayed([]string{"b.jpg"}) // the render that is on the glass

	// A rescan replaces the gallery wholesale; b.jpg is gone from it.
	iv.setImages([]string{"x.jpg", "y.jpg"})

	got := iv.snapshot()
	if got.total != 2 || got.index != 1 || got.key != "y.jpg" {
		t.Fatalf("snapshot = %+v, want total 2, index 1, key y.jpg", got)
	}
	if !reflect.DeepEqual(got.keys, []string{"b.jpg"}) {
		t.Fatalf("keys = %v, want [b.jpg] — the frame on the glass is the pre-rescan one",
			got.keys)
	}

	// The state the contract warns a consumer about: keys names an object that is
	// not in the gallery any more, so presigning it can 404.
	if _, ok := iv.indexOfKey(got.keys[0]); ok {
		t.Fatal("the fixture no longer exercises the case where keys names an object that " +
			"has left the gallery")
	}

	// And nothing corrects it: the two gates the contract names are both shut.
	iv.onScanComplete(func() {
		t.Fatal("onScanComplete re-rendered at a non-zero index — the contract's claim that " +
			"the divergence is unbounded rests on it NOT doing that")
	})
	if !iv.isPaused() {
		t.Fatal("the viewer is no longer paused; the slide timer would paper over the " +
			"divergence and this test would stop pinning what it claims")
	}
}

// TestTheSlideshowTimerStillHonoursThePauseFlag pins the gate that
// TestAPausedRescanLeavesKeyAndKeysDisagreeingIndefinitely cannot reach.
//
// 🔴 It exists because a mutant proved the gap rather than because a comment
// looked thin: replacing startSlideshow's `if !iv.isPaused()` with `if true` was
// measured to leave the ENTIRE suite at 386 PASS / 0 FAIL. Two separate claims
// rest on that gate — Snapshot.SecondsUntilNext's "0 when paused", and the
// contract's "the divergence between Key and Keys is UNBOUNDED", which is only
// true because a paused slideshow never re-renders. Delete the gate and a paused
// Pi resumes turning pages: the countdown keeps reporting 0 while pages actually
// turn, the divergence becomes bounded by slide_interval, and the operator's
// pause button stops working. None of that was observable.
//
// It is STRUCTURAL because the gate is genuinely unreachable from a test: it
// lives inside the closure handed to glib.TimeoutAdd, which only a running GTK
// main loop executes. Same precedent as TestTheSlideTimerHasExactlyOneArmingSite.
//
// 🔴 IT REQUIRES THE `!`, AND COUNTING THE CALL WAS NOT ENOUGH. The first version
// of this guard counted isPaused() call sites, and its own disclosure claimed
// that killed "deleting or inverting the condition". The inversion half was
// measured FALSE: `if iv.isPaused()` — a slideshow that advances ONLY while
// paused — is still exactly one call in startSlideshow, so it produced a
// byte-identical 387 PASS / 0 FAIL. The guard now matches the negated expression
// `!iv.isPaused()`, which is the thing the code must actually contain. A count
// of DECLARATIONS is not a count of what they cover, one level down.
//
// 🔴 What it still does NOT pin, so nobody reads it for more than it is: it
// asserts the EXPRESSION exists in this function, not that the branch it guards
// is the one that advances the slideshow. `x := !iv.isPaused(); if true { … }`
// walks it, and so does keeping the condition while swapping the if/else bodies.
// Those are deliberate walks rather than plausible refactors; the three
// mutations that plausibly happen — deleting the condition, inverting it, and
// hoisting the read out of the closure so the flag is sampled once at startup —
// are all killed.
func TestTheSlideshowTimerStillHonoursThePauseFlag(t *testing.T) {
	negated := negatedIsPausedSites(t, "startSlideshow")
	for _, line := range negated {
		t.Logf("  main.go:%d  !iv.isPaused() inside startSlideshow()", line)
	}
	if len(negated) != 1 {
		t.Fatalf("startSlideshow contains %d `!iv.isPaused()` guard(s), want exactly 1 — the "+
			"slide timer must advance ONLY when the slideshow is not paused. Deleting that "+
			"condition, inverting it, or hoisting the read out of the timer closure all land "+
			"here: each makes the pause button inert, makes seconds_until_next's \"0 when "+
			"paused\" a lie while pages turn, and bounds the Key/Keys divergence the contract "+
			"documents as unbounded.", len(negated))
	}
}

// negatedIsPausedSites reports the lines inside the named function that hold a
// `!<something>.isPaused()` expression.
//
// 🔴 It matches the UnaryExpr, not the CallExpr, and that is the whole point —
// see the guard above. It also deliberately reports every match rather than
// stopping at the first, so the ledger fails when the set GROWS as well as when
// it shrinks: a second pause check in the timer is a second place for the rule to
// drift, which is the defect this repo consolidates against.
func negatedIsPausedSites(t *testing.T, fn string) []int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	var lines []int
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.FuncDecl)
		if !ok || decl.Name.Name != fn {
			return true
		}
		found = true
		ast.Inspect(decl, func(inner ast.Node) bool {
			u, ok := inner.(*ast.UnaryExpr)
			if !ok || u.Op != token.NOT {
				return true
			}
			call, ok := u.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "isPaused" {
				lines = append(lines, fset.Position(u.Pos()).Line)
			}
			return true
		})
		return false
	})
	// Instrument control: a finder that cannot locate the function at all would
	// report zero matches and read as a real failure of the code under test.
	if !found {
		t.Fatalf("no func %s in main.go — this guard is inspecting nothing", fn)
	}
	return lines
}

// TestKeysReflectTheSettledStateAfterARender is the other half of the guard
// above: once the render HAS completed, key is keys[0] and the two agree. Without
// it, an implementation that simply never updated keys would pass the
// not-recomputed test forever.
func TestKeysReflectTheSettledStateAfterARender(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg", "c.jpg")
	if !iv.gotoKey("b.jpg") {
		t.Fatal("gotoKey failed")
	}
	// What updateSingleImage records once its pixbuf is on the widget.
	idx, key, ok := iv.currentKey()
	if !ok {
		t.Fatal("currentKey reported an empty gallery")
	}
	iv.noteDisplayed([]string{key})

	got := iv.snapshot()
	if len(got.keys) != 1 || got.keys[0] != got.key {
		t.Fatalf("keys = %v, key = %q — in a settled state key must be the leading displayed "+
			"key, which is the additive contract every existing consumer relies on",
			got.keys, got.key)
	}
	if got.index != idx {
		t.Fatalf("index = %d, want %d", got.index, idx)
	}
}

// TestDisplayedPairReportsBothPositionsAndCollapsesTheOneImageGallery covers the
// length rule for the two-up view, at the seam the render path actually calls.
//
// The odd case is not hypothetical: pairKeys WRAPS (right = left + 1 mod n), so a
// one-image gallery loads images[0] into both halves and the screen shows one
// comic twice. Reporting it as two keys would tell the PWA two different comics
// are up. The test is on the INDEX rather than the key string, because two
// distinct positions holding an identical key means the bucket has a duplicated
// object — and those really are two comics on the glass.
func TestDisplayedPairReportsBothPositionsAndCollapsesTheOneImageGallery(t *testing.T) {
	for _, tc := range []struct {
		name              string
		leftIdx, rightIdx int
		left, right       string
		want              []string
	}{
		{"two distinct positions", 2, 3, "c.jpg", "d.jpg", []string{"c.jpg", "d.jpg"}},
		{"wrapped onto the front", 4, 0, "e.jpg", "a.jpg", []string{"e.jpg", "a.jpg"}},
		{"one-image gallery", 0, 0, "a.jpg", "a.jpg", []string{"a.jpg"}},
		{"duplicate key, two positions", 1, 2, "dup.jpg", "dup.jpg",
			[]string{"dup.jpg", "dup.jpg"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := displayedPair(tc.leftIdx, tc.left, tc.rightIdx, tc.right)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("displayedPair(%d,%q,%d,%q) = %v, want %v",
					tc.leftIdx, tc.left, tc.rightIdx, tc.right, got, tc.want)
			}
		})
	}
}

// TestTheTwoUpRenderReportsThePairPairKeysGaveIt joins displayedPair back to the
// accessor the renderer reads, so the ORDER is pinned end to end rather than only
// inside the helper. A helper that returned {right, left} passes the table above
// if the table's expectations were derived from it; this one derives them from
// pairKeys, which is where left and right are defined.
func TestTheTwoUpRenderReportsThePairPairKeysGaveIt(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg", "c.jpg")
	iv.setViewModeState(ViewLandscapeTwo)
	if !iv.gotoIndex(1) {
		t.Fatal("gotoIndex failed")
	}

	leftIdx, left, rightIdx, right, ok := iv.pairKeys()
	if !ok {
		t.Fatal("pairKeys reported an empty gallery")
	}
	if left != "b.jpg" || right != "c.jpg" {
		t.Fatalf("pairKeys = (%q, %q); the fixture no longer exercises a real pair", left, right)
	}
	iv.noteDisplayed(displayedPair(leftIdx, left, rightIdx, right))

	got := iv.snapshot()
	if !reflect.DeepEqual(got.keys, []string{"b.jpg", "c.jpg"}) {
		t.Fatalf("keys = %v, want [b.jpg c.jpg] — left to right, in the order the render "+
			"composited them", got.keys)
	}
	if got.key != got.keys[0] {
		t.Fatalf("key = %q but keys[0] = %q", got.key, got.keys[0])
	}
}

// TestNoteDisplayedDoesNotAliasTheCallersSlice: the render path builds its slice
// and hands it over under a different lock acquisition than the one that stores
// it. Retaining the caller's backing array would let a later render mutate state
// the HTTP goroutine is reading — a data race -race only catches when the timing
// lands. Copying on the way in and on the way out closes it deterministically.
func TestNoteDisplayedDoesNotAliasTheCallersSlice(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg")

	caller := []string{"a.jpg", "b.jpg"}
	iv.noteDisplayed(caller)
	caller[0] = "clobbered.jpg"

	got := iv.snapshot()
	if !reflect.DeepEqual(got.keys, []string{"a.jpg", "b.jpg"}) {
		t.Fatalf("keys = %v after the caller mutated its own slice — noteDisplayed retained "+
			"the caller's array", got.keys)
	}

	// And the other direction: a consumer of the snapshot must not be able to
	// reach back into viewer state.
	got.keys[0] = "clobbered.jpg"
	if again := iv.snapshot(); again.keys[0] != "a.jpg" {
		t.Fatalf("keys = %v after a caller mutated a snapshot — snapshot handed out the "+
			"viewer's own array", again.keys)
	}
}

// TestBothRenderPathsRecordWhatTheyDisplayedAfterTheWidgetTakesIt pins WHERE and
// WHEN the recording happens. It says nothing about WHAT is recorded — that is
// TestEachRenderPathRecordsTheKeysItActuallyLoaded, and the two are only adequate
// together.
//
// 🔴 SCOPE, narrowed to match the body. The name reads like full coverage of the
// recording, and an audit measured that it is not: with this guard green, both
// `noteDisplayed([]string{"WRONG-KEY-NOT-ON-SCREEN.jpg"})` and a two-up path
// recording only its left half passed the ENTIRE suite. It checks the SET of
// calling functions and the LINE ORDER against SetFromPixbuf; it never looks at
// the argument.
//
// What it does pin earns its place, because neither render path can be
// unit-tested at all — both end in gtk.Image.SetFromPixbuf, which needs a display
// — so the ORDERING rule has no behavioural guard available. The rule is the one
// noteLayoutBox lives by: record only after the pixbuf is on the widget. Recorded
// BEFORE the load, `keys` would name a frame that never appeared, and a failed
// 30 s S3 GET would leave that claim standing while the previous comic is still
// lit — silently, forever.
//
// ⚠ And it is walkable by SHAPE rather than by ordering: a noteDisplayed wrapped
// in a never-true condition, placed textually after SetFromPixbuf, survives. That
// is inherent to a line-number guard. Said here so it is not read as more.
func TestBothRenderPathsRecordWhatTheyDisplayedAfterTheWidgetTakesIt(t *testing.T) {
	renders := []string{"updateSingleImage", "updateTwoImages"}

	setPixbuf := map[string]int{}
	for _, s := range packageCallSites(t, "SetFromPixbuf") {
		setPixbuf[s.Func] = s.Line
	}
	noted := map[string]int{}
	for _, s := range packageCallSites(t, "noteDisplayed") {
		if _, dup := noted[s.Func]; dup {
			t.Fatalf("%s calls noteDisplayed more than once; the last call wins and the "+
				"earlier one is a claim about a frame that was replaced", s.Func)
		}
		noted[s.Func] = s.Line
	}

	if len(noted) != len(renders) {
		t.Fatalf("noteDisplayed is called from %d function(s) %v, want exactly the %d render "+
			"paths %v. Anything else that puts a pixbuf on the widget must record what it "+
			"displayed, and anything that records without displaying is lying about the glass.",
			len(noted), noted, len(renders), renders)
	}
	for _, fn := range renders {
		note, ok := noted[fn]
		if !ok {
			t.Errorf("%s does not call noteDisplayed(); GET /api/state would go on reporting "+
				"the previous frame's keys after this path renders", fn)
			continue
		}
		set, ok := setPixbuf[fn]
		if !ok {
			t.Errorf("%s does not call SetFromPixbuf(); this guard is inspecting the wrong "+
				"function", fn)
			continue
		}
		if note < set {
			t.Errorf("%s records the displayed keys at line %d, BEFORE SetFromPixbuf at line "+
				"%d — a render that bails out between them claims a frame that never "+
				"appeared", fn, note, set)
		}
	}
}

// noteDisplayedArg is one `iv.noteDisplayed(...)` call site with its argument
// rendered back to source text.
type noteDisplayedArg struct {
	File string
	Func string
	Line int
	Arg  string
}

// exprSource prints an expression back to source text, in full.
//
// 🔴 NOT types.ExprString, and this is a measured correction rather than a
// preference. types.ExprString ELIDES the elements of a composite literal: it
// renders both `[]string{imageKey}` and `[]string{"WRONG-KEY-NOT-ON-SCREEN.jpg"}`
// as the identical string `[]string{…}`. A ledger built on it compares equal for
// the correct code and for the exact mutant it exists to catch — a guard that
// reads as coverage and provides none. Caught before it shipped only because
// TestTheNoteDisplayedArgumentFinderCanSeeAWrongArgument checks the finder
// against a wrong argument rather than trusting it.
//
// go/printer emits the whole expression. Whitespace is normalised so that
// gofmt's line-breaking decisions are not part of the contract.
func exprSource(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "<unprintable: " + err.Error() + ">"
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// noteDisplayedCallSites walks every non-test file of package main and reports
// each noteDisplayed call together with the EXPRESSION it passes.
//
// packageCallSites deliberately does not carry arguments — it answers "who calls
// this", which is the question the ledgers above it ask. This one answers "with
// what", and that turned out to be the question that mattered.
func noteDisplayedCallSites(t *testing.T) []noteDisplayedArg {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var out []noteDisplayedArg
	scanned := 0
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", n, err)
		}
		scanned++
		var currentFunc string
		ast.Inspect(f, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.FuncDecl:
				currentFunc = x.Name.Name
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "noteDisplayed" {
					return true
				}
				arg := "<no argument>"
				if len(x.Args) == 1 {
					arg = exprSource(fset, x.Args[0])
				}
				out = append(out, noteDisplayedArg{
					File: n, Func: currentFunc, Arg: arg,
					Line: fset.Position(sel.Sel.Pos()).Line,
				})
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files at all — this guard is inspecting nothing")
	}
	return out
}

// TestEachRenderPathRecordsTheKeysItActuallyLoaded closes the seam that
// TestBothRenderPathsRecordWhatTheyDisplayedAfterTheWidgetTakesIt leaves open.
//
// 🔴 SCOPE FIRST, because the NAME overclaims and a later round measured that it
// does. This pins the ARGUMENT EXPRESSION each render path records. It does NOT
// bind what was LOADED to what is RECORDED, and two mutants prove the gap:
// making updateTwoImages load leftKey for BOTH halves while still recording
// displayedPair(...), and reassigning imageKey immediately before the record,
// each leave the argument text identical and each SURVIVE this guard and the
// whole suite. Both are unreachable through the AST — they are about values at
// run time, and neither render path can be run without a display. Read the name
// as "records the keys it NAMED", not "verified against the pixels".
//
// 🔴 It is still the guard for the field's central failure, and it is here
// because an adversarial audit found the suite blind to it. Two mutants passed
// all 381 tests with 0 FAIL:
//
//	updateSingleImage: iv.noteDisplayed([]string{"WRONG-KEY-NOT-ON-SCREEN.jpg"})
//	updateTwoImages:   iv.noteDisplayed([]string{leftKey})   // drops the right half
//
// The second is the PR's own motivating case — the two-up view reporting one key
// is exactly the state `keys` exists to end — and every guard walked past it.
// Each side was tested in isolation: displayedPair has its own table, and
// TestTheTwoUpRenderReportsThePairPairKeysGaveIt hand-assembles the call. Nothing
// bound the CALL SITE to the helper, so a render path that bypassed displayedPair
// entirely was invisible. Isolation-seam, exactly.
//
// Neither render path can be executed without a display (both end in
// gtk.Image.SetFromPixbuf), so the argument cannot be observed behaviourally at
// all. What can be pinned is the expression, WHOLE and normalised rather than by
// substring — a partial match ("mentions leftKey") is satisfied by the mutant
// that passes only leftKey.
//
// The cost is real and accepted: renaming a local in either render path fails
// this test. That is the price of a machine-readable claim about a value no test
// can otherwise reach, and the fix is one line in the ledger.
func TestEachRenderPathRecordsTheKeysItActuallyLoaded(t *testing.T) {
	want := map[string]string{
		// The key currentKey() resolved and LoadImage was called with — not a
		// re-read of the index, which a queued page turn may already have moved.
		"updateSingleImage": "[]string{imageKey}",
		// Both halves, from the one pairKeys read the two loads used, through the
		// helper that collapses a one-image gallery. A mutant that passes
		// []string{leftKey} — or that inlines the pair and skips the collapse —
		// fails here.
		"updateTwoImages": "displayedPair(idx, leftKey, rightIdx, rightKey)",
	}

	sites := noteDisplayedCallSites(t)
	for _, s := range sites {
		t.Logf("  %s:%d  %s() <- %s", s.File, s.Line, s.Func, s.Arg)
	}
	if len(sites) != len(want) {
		t.Fatalf("noteDisplayed is called from %d site(s), want %d.\nAnything that puts a "+
			"pixbuf on the widget must record the keys IT loaded; anything that records "+
			"without displaying is lying about the glass.\ngot: %+v", len(sites), len(want), sites)
	}
	for _, s := range sites {
		w, ok := want[s.Func]
		if !ok {
			t.Errorf("%s:%d records displayed keys from %s(), which is not a render path",
				s.File, s.Line, s.Func)
			continue
		}
		if s.Arg != w {
			t.Errorf("%s:%d %s() records `%s`, want exactly `%s` — GET /api/state would report "+
				"keys that are not the ones this render put on the screen, which is the whole "+
				"defect the field exists to close",
				s.File, s.Line, s.Func, s.Arg, w)
		}
	}
}

// TestTheNoteDisplayedArgumentFinderCanSeeAWrongArgument is the instrument
// control for the guard above: an extractor that silently matched nothing, or
// that returned the same string for every call, would make that ledger vacuously
// clean. Both failure modes are checked against source built to defeat them — a
// literal argument, a bare identifier, and a call with no argument at all.
func TestTheNoteDisplayedArgumentFinderCanSeeAWrongArgument(t *testing.T) {
	const src = `package main
func a() { iv.noteDisplayed([]string{"WRONG-KEY-NOT-ON-SCREEN.jpg"}) }
func b() { iv.noteDisplayed([]string{leftKey}) }
func c() { iv.noteDisplayed() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the control source: %v", err)
	}
	var got []string
	var currentFunc string
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.FuncDecl:
			currentFunc = x.Name.Name
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "noteDisplayed" {
				arg := "<no argument>"
				if len(x.Args) == 1 {
					arg = exprSource(fset, x.Args[0])
				}
				got = append(got, currentFunc+":"+arg)
			}
		}
		return true
	})
	want := []string{
		`a:[]string{"WRONG-KEY-NOT-ON-SCREEN.jpg"}`,
		"b:[]string{leftKey}",
		"c:<no argument>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the argument finder reported %#v, want %#v — the ledger above cannot see "+
			"the mutants it exists to catch", got, want)
	}
}

// ---------------------------------------------------------------------------
// seconds_until_next — one clock, the timer's
// ---------------------------------------------------------------------------

// TestCountdownFromRoundsUpAndClamps drives the arithmetic directly, at both
// boundaries and in the middle.
func TestCountdownFromRoundsUpAndClamps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		interval  uint
		want      int
	}{
		{"freshly armed", 45 * time.Second, 45, 45},
		{"mid-flight", 26*time.Second + 300*time.Millisecond, 45, 27},
		{"rounds up rather than down", 400 * time.Millisecond, 45, 1},
		{"exactly due", 0, 45, 0},
		{"overdue is never negative", -90 * time.Second, 45, 0},
		{"a lowered interval clamps the ceiling", 600 * time.Second, 11, 11},
		{"a zero interval pins it at zero", 600 * time.Second, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countdownFrom(tc.remaining, tc.interval); got != tc.want {
				t.Fatalf("countdownFrom(%v, %d) = %d, want %d", tc.remaining, tc.interval, got, tc.want)
			}
		})
	}
}

// TestSecondsUntilNextCountsDownAcrossSuccessiveReads is the behaviour a polling
// client depends on: the number must strictly DECREASE between polls of an armed,
// playing slideshow, and land on 0 rather than going negative.
func TestSecondsUntilNextCountsDownAcrossSuccessiveReads(t *testing.T) {
	// 45, not the 30 default: a mutant that reported the constant 30 would be
	// invisible against a 30 s fixture.
	iv := newControlTestViewer(45, "a.jpg")
	armed := time.Now()
	iv.swapTimeout(glib.SourceHandle(7), armed.Add(45*time.Second))

	var previous int
	for i, tc := range []struct {
		elapsed time.Duration
		want    int
	}{
		{0, 45},
		{1 * time.Second, 44},
		{22*time.Second + 500*time.Millisecond, 23},
		{44*time.Second + 100*time.Millisecond, 1},
		{45 * time.Second, 0},
		{5 * time.Minute, 0}, // the main loop fired late; still not negative
	} {
		got := iv.snapshotAt(armed.Add(tc.elapsed)).secondsUntilNext
		if got != tc.want {
			t.Fatalf("%v after arming: seconds_until_next = %d, want %d", tc.elapsed, got, tc.want)
		}
		if got < 0 {
			t.Fatalf("seconds_until_next = %d — negative", got)
		}
		if got > int(iv.slideInterval()) {
			t.Fatalf("seconds_until_next = %d, above slide_interval %d", got, iv.slideInterval())
		}
		// Strictly decreasing while there is anything left to count; the two rows
		// past the deadline are both 0, which is the floor and not a stall.
		if i > 0 && previous > 0 && got >= previous {
			t.Fatalf("seconds_until_next went %d -> %d as time advanced; it is not counting down",
				previous, got)
		}
		previous = got
	}
}

// TestSecondsUntilNextIsZeroWhenPausedAndResumesWhenPlaying pins both directions
// of the paused rule from ONE armed timer.
//
// One direction alone is walkable: `return 0` always passes the paused half, and
// ignoring paused entirely passes the playing half. Reading the same viewer twice
// with only the flag moved is what separates them.
func TestSecondsUntilNextIsZeroWhenPausedAndResumesWhenPlaying(t *testing.T) {
	iv := newControlTestViewer(45, "a.jpg")
	armed := time.Now()
	iv.swapTimeout(glib.SourceHandle(9), armed.Add(45*time.Second))
	at := armed.Add(20 * time.Second)

	if got := iv.snapshotAt(at).secondsUntilNext; got != 25 {
		t.Fatalf("playing: seconds_until_next = %d, want 25", got)
	}

	iv.setPausedState(true)
	got := iv.snapshotAt(at)
	if !got.paused {
		t.Fatal("the fixture did not pause")
	}
	if got.secondsUntilNext != 0 {
		t.Fatalf("paused: seconds_until_next = %d, want 0 — the timer still ticks while paused "+
			"but it advances nothing, so a countdown would be counting down to a page turn "+
			"that will not happen", got.secondsUntilNext)
	}

	iv.setPausedState(false)
	if got := iv.snapshotAt(at).secondsUntilNext; got != 25 {
		t.Fatalf("resumed: seconds_until_next = %d, want 25", got)
	}
}

// TestSecondsUntilNextIsZeroWithNoArmedTimer covers the other 0: nothing is
// scheduled at all. It is a real state — the window between retiring the fired
// source and arming its replacement, and every viewer built before
// startSlideshow runs.
func TestSecondsUntilNextIsZeroWithNoArmedTimer(t *testing.T) {
	iv := newControlTestViewer(45, "a.jpg")
	if got := iv.snapshot().secondsUntilNext; got != 0 {
		t.Fatalf("seconds_until_next = %d with no timer armed, want 0", got)
	}

	// Arm it, then retire it the way startTimer does.
	armed := time.Now()
	iv.swapTimeout(glib.SourceHandle(5), armed.Add(45*time.Second))
	if got := iv.snapshotAt(armed).secondsUntilNext; got != 45 {
		t.Fatalf("seconds_until_next = %d while armed, want 45; this test is not observing "+
			"the field move", got)
	}
	if previous := iv.swapTimeout(0, time.Time{}); previous != 5 {
		t.Fatalf("swapTimeout returned handle %d, want 5", previous)
	}
	if got := iv.snapshotAt(armed).secondsUntilNext; got != 0 {
		t.Fatalf("seconds_until_next = %d after the timer was retired, want 0 — the countdown "+
			"is running against a source that no longer exists", got)
	}
}

// TestADeadlineWithoutAnArmedSourceCountsDownToNothing pins WHICH field means
// "an advance is scheduled".
//
// swapTimeout writes the handle and the deadline together, so on every path the
// accessors take, "no handle" and "no deadline" coincide and dropping the handle
// check from snapshotAt changes no observable behaviour — the same shape as
// TestSnapshotNormalisesAnOutOfRangeIndex, and handled the same way: build the
// state the accessors would never produce and check the rule still holds.
//
// It is not decoration. The hazard this whole field is written against is a
// SECOND clock, and a second clock arrives exactly as a deadline that no GLib
// source backs — a "predict the next advance" convenience, a deadline restored
// from config, a half-finished refactor. The armed GLib source is the thing that
// actually turns the page, so it is the thing the countdown is allowed to
// describe.
func TestADeadlineWithoutAnArmedSourceCountsDownToNothing(t *testing.T) {
	now := time.Now()
	iv := &ImageViewer{
		images:        []string{"a.jpg"},
		mutex:         &sync.RWMutex{},
		config:        &Config{SlideInterval: 45},
		nextAdvanceAt: now.Add(30 * time.Second), // a deadline...
		timeoutID:     0,                         // ...with no source behind it
	}
	if got := iv.snapshotAt(now).secondsUntilNext; got != 0 {
		t.Fatalf("seconds_until_next = %d for a deadline with no armed GLib source, want 0 — "+
			"the countdown is describing an advance that nothing will perform", got)
	}

	// Positive control: the same deadline WITH a source does count down, so the
	// assertion above is about the handle and not about a field that is inert.
	iv.timeoutID = 4
	if got := iv.snapshotAt(now).secondsUntilNext; got != 30 {
		t.Fatalf("seconds_until_next = %d once a source is armed, want 30; this test cannot "+
			"see the handle check move", got)
	}
}

// TestStartSlideshowArmsTheCountdownFromTheTimersOwnInterval is the seam test:
// the countdown and the GLib timeout must be armed from ONE interval read at ONE
// instant, not from two.
//
// glib.TimeoutAdd registers a source without gtk.Init or a running main loop, so
// this drives the REAL startSlideshow rather than a stand-in. 53 is neither the
// 30 default nor a power of two, so neither constant can stand in for it.
func TestStartSlideshowArmsTheCountdownFromTheTimersOwnInterval(t *testing.T) {
	iv := newControlTestViewer(53, "a.jpg")

	iv.startSlideshow()
	armedBy := time.Now()
	t.Cleanup(func() {
		// Leave no callback registered in the shared default main context.
		if previous := iv.swapTimeout(0, time.Time{}); previous != 0 {
			glib.SourceRemove(previous)
		}
	})

	if got := iv.snapshotAt(armedBy).secondsUntilNext; got != 53 {
		t.Fatalf("immediately after startSlideshow: seconds_until_next = %d, want 53 — the "+
			"countdown was not armed from the same interval as the GLib timeout", got)
	}
	if got := iv.snapshotAt(armedBy.Add(20 * time.Second)).secondsUntilNext; got != 33 {
		t.Fatalf("20 s later: seconds_until_next = %d, want 33", got)
	}
	if got := iv.snapshotAt(armedBy.Add(53 * time.Second)).secondsUntilNext; got != 0 {
		t.Fatalf("at the deadline: seconds_until_next = %d, want 0", got)
	}
}

// TestTheSlideTimerHasExactlyOneArmingSite is an asserted ledger, and it is the
// deterministic form of "two clocks that can disagree is a defect".
//
// The countdown is honest only because the deadline is written by the SAME call
// that stores the GLib handle, from the SAME interval read, at the ONE place that
// arms a timeout. A second glib.TimeoutAdd anywhere in package main — a
// "restart the timer after a page turn" convenience, say — would arm a source
// whose deadline nothing recorded, and GET /api/state would count down to an
// instant that is not when the slide changes. Nothing else in the suite can see
// that, because the second timer would work perfectly.
func TestTheSlideTimerHasExactlyOneArmingSite(t *testing.T) {
	adds := packageCallSites(t, "TimeoutAdd")
	for _, s := range adds {
		t.Logf("  %s:%d  %s() on %s", s.File, s.Line, s.Func, s.Recv)
	}
	if len(adds) != 1 || adds[0].File != "main.go" || adds[0].Func != "startSlideshow" {
		t.Fatalf("glib.TimeoutAdd is called from %+v, want exactly one site in "+
			"main.go startSlideshow. Every armed timeout must record its deadline through "+
			"swapTimeout in the same statement, or seconds_until_next counts down to the "+
			"wrong instant.", adds)
	}

	swaps := packageCallSites(t, "swapTimeout")
	inStart := 0
	for _, s := range swaps {
		if s.File == "main.go" && s.Func == "startSlideshow" {
			inStart++
			continue
		}
		t.Errorf("swapTimeout is called from %s:%d in %s(); the handle and its deadline are "+
			"one atomic write and startSlideshow is the only writer", s.File, s.Line, s.Func)
	}
	// Two: one to retire the previous source, one to arm the next.
	if inStart != 2 {
		t.Errorf("startSlideshow calls swapTimeout %d time(s), want 2 (retire, then arm)", inStart)
	}

	// 🔴 ONE interval read, and it took an audit to notice nothing pinned it.
	// startSlideshow's own comment claims reading the interval twice "would let
	// POST /api/interval land between them and produce a countdown for a duration
	// no timer was ever armed for", and
	// TestStartSlideshowArmsTheCountdownFromTheTimersOwnInterval's docstring says
	// it pins ONE read at ONE instant — but its body cannot tell one read from
	// two, because a test that never moves the interval mid-call gets the same
	// answer either way. Splitting the local back into two iv.slideInterval()
	// calls SURVIVED the whole suite. A docstring claiming a relationship while
	// the body checks one side of it is worse than no guard: it stops the next
	// person looking.
	reads := 0
	for _, s := range packageCallSites(t, "slideInterval") {
		if s.File == "main.go" && s.Func == "startSlideshow" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("startSlideshow reads slideInterval() %d time(s), want exactly 1 — the GLib "+
			"timeout and the deadline it is counted down from must come from the SAME read, "+
			"or POST /api/interval landing between them arms one duration and reports another",
			reads)
	}
}

// ---------------------------------------------------------------------------
// POST /api/interval re-arms the timer
// ---------------------------------------------------------------------------

// armedTimer reads the handle/deadline PAIR under one lock acquisition, which is
// the only way to observe that the two agree. Reading them separately would let
// a writer land between the reads and could report a pair the viewer was never
// in — which is the exact defect swapTimeout exists to prevent, reintroduced in
// the instrument that checks for it.
func armedTimer(iv *ImageViewer) (glib.SourceHandle, time.Time) {
	iv.mutex.RLock()
	defer iv.mutex.RUnlock()
	return iv.timeoutID, iv.nextAdvanceAt
}

// retireArmedTimer leaves no callback registered in the shared default main
// context, which every test that drives the real startSlideshow must do.
//
// 🔴 It is not tidiness, and the reason it is not yet a bug is worth writing
// down. These tests arm REAL GLib timeouts on the default main context, some of
// them seconds away from firing, and a number of tests in lifecycle_test.go
// ITERATE that same context. Nothing in package main calls t.Parallel, so a
// test's sources are always retired before the next test runs and no other test's
// Iteration can ever dispatch them.
//
// 🔴 NO COUNT IS GIVEN HERE, deliberately, and that is the second correction to
// this paragraph. It said "four tests", was corrected to "six", and six was ALSO
// wrong — the real figure was seven, because ctx.Iteration is reached both
// directly and through drainMainContext AND through waitScanCount, which calls
// drainMainContext in turn. A count of call SITES is not a count of tests, the
// version of this sentence that said so out loud still undercounted by stopping
// one helper level short, and any number written here rots the next time a helper
// gains a caller. What matters is the relationship — some tests iterate the
// shared default main context, transitively — so that is what is written.
//
// Add t.Parallel anywhere in this package and the invariant stops holding: an
// expired slide timeout would be dispatched inside an unrelated test, and the
// result is a PANIC, not a slow test — measured, not assumed. newControlTestViewer
// leaves the store field nil, so the closure reaches iv.store.LoadImage and
// nil-dereferences, aborting the whole test binary from inside a test that never
// armed anything.
//
// 🔴 Two earlier versions of that sentence were wrong, in opposite directions.
// One said "a real S3 GET", which would send the next maintainer hunting a
// network problem in the wrong test. The other hedged it to "usually", reasoning
// that the one PAUSED fixture would re-arm silently instead — the mechanism is
// real (the closure gates the render on !isPaused) but it does not apply here,
// because that test RESUMES before it returns and its surviving source therefore
// expires unpaused like every other. It is all of them, not most of them.
func retireArmedTimer(t *testing.T, iv *ImageViewer) {
	t.Helper()
	if previous := iv.swapTimeout(0, time.Time{}); previous != 0 {
		glib.SourceRemove(previous)
	}
}

// sourceIsArmed asks GLIB — not this program's own bookkeeping — whether a
// source id is still registered on the default main context.
//
// That distinction is the whole point of using it. Every other assertion in this
// file reads iv.timeoutID, which is what the code BELIEVES; an implementation
// that arms a replacement and forgets to remove the old source updates that
// belief perfectly while leaving two live timeouts behind it. This reads the
// registry the timeouts actually fire from.
func sourceIsArmed(h glib.SourceHandle) bool {
	if h == 0 {
		return false
	}
	return glib.MainContextDefault().FindSourceById(h) != nil
}

// TestLoweringTheIntervalReArmsTheTimerImmediately is the motivating case, and
// it REPLACES TestLoweringTheIntervalFreezesTheCountdownUntilTheTimerCatchesUp.
//
// 🔴 WHY IT CHANGED. The old test pinned the opposite behaviour, deliberately:
// SetInterval only stored the value ("it takes effect on the next tick"), so a
// source armed for 600 s was still 600 s from firing after the interval dropped
// to 11, and the [0, slide_interval] clamp reported a stationary 11 for the next
// nine and a half minutes. Its own docstring said it existed "so that decision is
// made deliberately" and that "the real fix is for SetInterval to re-arm". That
// decision has now been made — the operator's call — and the fix is in
// gtkViewer.SetInterval, so the property that test asserted is no longer true and
// asserting it would pin the defect back in. It is updated rather than deleted so
// the freeze stays on the record as something that was measured, and so the
// replacement is visibly the same scenario with the answer changed.
//
// It drives the ADAPTER, not the raw accessor. setSlideInterval on its own still
// only stores — the re-arm is its caller's job, because the GLib calls must not
// happen under the viewer's lock — so a test that called it directly would still
// observe the freeze and would be testing a path POST /api/interval never takes.
//
// glib.TimeoutAdd registers a source without gtk.Init or a running main loop, so
// this drives the REAL startSlideshow and the REAL re-arm.
func TestLoweringTheIntervalReArmsTheTimerImmediately(t *testing.T) {
	// 600 and 11: 11 is neither a factor nor a multiple of 600, and neither is
	// the 30 default, so no constant in the implementation can stand in for
	// either one.
	iv := newControlTestViewer(600, "a.jpg")
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })

	oldHandle, oldDeadline := armedTimer(iv)
	if oldHandle == 0 {
		t.Fatal("startSlideshow armed nothing — this test cannot observe a re-arm")
	}

	// The pre-change symptom, so the fix is measured against the failure and not
	// merely against an expectation: at this instant the countdown is 600.
	if got := iv.snapshotAt(oldDeadline.Add(-600 * time.Second)).secondsUntilNext; got != 600 {
		t.Fatalf("armed at 600 s the countdown reads %d, want 600 — the fixture is not what "+
			"this test claims", got)
	}

	// POST /api/interval lands, through the closure the handler enqueues.
	before := time.Now()
	gtkViewer{iv: iv}.SetInterval(11)
	after := time.Now()

	newHandle, newDeadline := armedTimer(iv)
	if newHandle == 0 {
		t.Fatal("no timer is armed after the interval change — the slideshow has stopped")
	}
	if newHandle == oldHandle {
		t.Fatalf("the armed handle is still %d after the interval changed from 600 s to 11 s: "+
			"the pending timeout was never replaced, so the display waits out the remaining "+
			"~10 minutes and the countdown sits frozen on 11", oldHandle)
	}

	// The deadline is one NEW interval out, and it was written by the same call
	// that stored the handle above — the pair, read under one lock.
	if newDeadline.Before(before.Add(11*time.Second)) || newDeadline.After(after.Add(11*time.Second)) {
		t.Fatalf("the new deadline is %v; want within [%v, %v] — one 11 s interval from the "+
			"instant of the re-arm. It is %v from the old 600 s deadline.",
			newDeadline, before.Add(11*time.Second), after.Add(11*time.Second),
			newDeadline.Sub(oldDeadline))
	}
	if !newDeadline.Before(oldDeadline) {
		t.Fatalf("the deadline did not move IN: %v -> %v. Lowering the interval must bring the "+
			"next advance closer, not leave it where it was.", oldDeadline, newDeadline)
	}

	// And the countdown a polling client reads starts from the NEW interval and
	// DECREASES. Measured from `after`, so every bound below is exact for any
	// re-arm that took under a second: at `after` the remaining is in
	// (11 s - δ, 11 s], at after+5 s it is in (5 s, 6 s], and at after+11 s it is
	// already past due.
	var previous int
	for i, tc := range []struct {
		elapsed time.Duration
		want    int
		note    string
	}{
		{0, 11, "the countdown restarts from the NEW interval"},
		{5 * time.Second, 6, "and it moves — it did not for ~9 minutes before this fix"},
		{10 * time.Second, 1, "still moving"},
		{11 * time.Second, 0, "the re-armed source is due"},
		{5 * time.Minute, 0, "the main loop fired late; still not negative"},
	} {
		got := iv.snapshotAt(after.Add(tc.elapsed)).secondsUntilNext
		if got != tc.want {
			t.Errorf("%v after the re-arm: seconds_until_next = %d, want %d (%s); the re-arm "+
				"took %v", tc.elapsed, got, tc.want, tc.note, after.Sub(before))
		}
		if got > int(iv.slideInterval()) {
			t.Errorf("%v after the re-arm: seconds_until_next = %d exceeds slide_interval %d — "+
				"the [0, slide_interval] clamp is part of the wire contract and it broke",
				tc.elapsed, got, iv.slideInterval())
		}
		if i > 0 && previous > 0 && got >= previous {
			t.Errorf("seconds_until_next went %d -> %d as time advanced: it is not counting down, "+
				"which is the freeze this change exists to remove", previous, got)
		}
		previous = got
	}
}

// TestRaisingTheIntervalReArmsFromTheNewInterval is the other direction, and it
// is not symmetric with lowering: raising it moves the next advance OUT, and the
// countdown has to be able to report a number ABOVE the old interval.
//
// A "clamp harder" non-fix — leave the timer alone and report min(remaining,
// interval) — passes the lowering case and fails here, because the honest answer
// is 53 and the old timer can only ever produce something ≤ 7.
func TestRaisingTheIntervalReArmsFromTheNewInterval(t *testing.T) {
	iv := newControlTestViewer(7, "a.jpg")
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })

	oldHandle, oldDeadline := armedTimer(iv)

	before := time.Now()
	gtkViewer{iv: iv}.SetInterval(53) // neither 7, nor the 30 default, nor a power of two
	after := time.Now()

	newHandle, newDeadline := armedTimer(iv)
	if newHandle == oldHandle {
		t.Fatalf("the armed handle is still %d after the interval changed from 7 s to 53 s: "+
			"the display will keep turning the page every 7 seconds", oldHandle)
	}
	if !newDeadline.After(oldDeadline) {
		t.Fatalf("the deadline did not move OUT: %v -> %v", oldDeadline, newDeadline)
	}
	if newDeadline.Before(before.Add(53*time.Second)) || newDeadline.After(after.Add(53*time.Second)) {
		t.Fatalf("the new deadline is %v, want one 53 s interval from the re-arm "+
			"(within [%v, %v])", newDeadline, before.Add(53*time.Second), after.Add(53*time.Second))
	}
	if got := iv.snapshotAt(after).secondsUntilNext; got != 53 {
		t.Fatalf("seconds_until_next = %d immediately after raising the interval to 53, want 53 — "+
			"a countdown that cannot exceed the OLD interval is a timer that was never re-armed",
			got)
	}
	if got := iv.snapshotAt(after.Add(30 * time.Second)).secondsUntilNext; got != 23 {
		t.Fatalf("30 s after the re-arm: seconds_until_next = %d, want 23", got)
	}
}

// TestReArmingRetiresTheOldGLibSourceSoOnlyOneTimerIsArmed is the leak guard,
// and it is the one assertion in this section that a "the deadline moved"
// implementation cannot satisfy.
//
// 🔴 THE MUTANT IT EXISTS FOR: arm the replacement, do not remove the old source
// — i.e. drop `glib.SourceRemove(previous)` from startSlideshow's startTimer.
// Every other test here still passes against that: iv.timeoutID holds the new
// handle, nextAdvanceAt holds the new deadline, the countdown counts down from
// the new interval. The bookkeeping is perfect and there are TWO live timeouts,
// each of which advances the display and each of which re-arms its own successor
// on every tick. On the Pi that is a slideshow that skips pages, at a rate that
// doubles again on every interval change.
//
// So this asks GLib, not the struct: the old source id must no longer resolve on
// the default main context, and the new one must.
//
// PAIRED CONTROL, both directions, from the same probe: the old handle is
// asserted PRESENT before the re-arm (so a probe wired to nothing cannot pass by
// always answering "gone") and the new handle is asserted PRESENT after (so a
// probe that answers "gone" for everything cannot pass either).
//
// Scope, stated: this proves the OLD source is retired and the NEW one is armed.
// It does not enumerate the whole main context, so it cannot see a third source
// armed by some unrelated code path — TestTheSlideTimerHasExactlyOneArmingSite
// is what closes that, by pinning the number of glib.TimeoutAdd call sites in
// the package at one.
func TestReArmingRetiresTheOldGLibSourceSoOnlyOneTimerIsArmed(t *testing.T) {
	iv := newControlTestViewer(600, "a.jpg")
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })

	oldHandle, _ := armedTimer(iv)
	if oldHandle == 0 {
		t.Fatal("startSlideshow armed nothing")
	}
	// Positive control: the probe CAN see a live source. Without this, the
	// assertion below is indistinguishable from a probe that never finds anything.
	if !sourceIsArmed(oldHandle) {
		t.Fatalf("GLib does not know about source %d, which startSlideshow just armed. The probe "+
			"is not observing the registry the timeouts fire from, so its verdict below would be "+
			"worthless.", oldHandle)
	}

	gtkViewer{iv: iv}.SetInterval(11)

	newHandle, _ := armedTimer(iv)
	if newHandle == 0 {
		t.Fatal("no timer armed after the re-arm")
	}
	if newHandle == oldHandle {
		t.Fatalf("the handle did not change (%d): nothing was re-armed", oldHandle)
	}
	// The other half of the control: the probe answers YES for a source that is
	// genuinely armed, so a NO below is about the old source and not about the
	// probe.
	if !sourceIsArmed(newHandle) {
		t.Fatalf("GLib does not know about the newly armed source %d — the re-arm recorded a "+
			"handle for a timeout that is not registered", newHandle)
	}
	if sourceIsArmed(oldHandle) {
		t.Fatalf("the 600 s source %d is STILL armed on the default main context after the "+
			"interval changed to 11 s, alongside the new source %d. TWO live slide timeouts: the "+
			"display advances twice per period and each tick re-arms its own successor, so the "+
			"leak compounds. The re-arm must RETIRE the pending source "+
			"(glib.SourceRemove of the handle swapTimeout returns), not just arm another one.",
			oldHandle, newHandle)
	}
}

// TestSettingTheIntervalItIsAlreadyOnDoesNotResetTheCountdown pins the starvation
// guard.
//
// Re-arming resets the countdown. An unconditional re-arm therefore means a
// client that re-POSTs the interval it already has — a settings page that
// submits its whole form, a PWA that pushes its state on every poll — pushes the
// next advance out on every request, and the display never turns the page again.
// The endpoint would report 202 the whole time and "did nothing" would be the
// symptom.
//
// setSlideInterval reports whether the value MOVED and SetInterval re-arms only
// then. Mutants killed here: `changed = true` unconditionally, and dropping the
// `if !changed { return }` in the adapter.
func TestSettingTheIntervalItIsAlreadyOnDoesNotResetTheCountdown(t *testing.T) {
	iv := newControlTestViewer(23, "a.jpg")
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })

	handle, deadline := armedTimer(iv)

	for i := 0; i < 3; i++ {
		gtkViewer{iv: iv}.SetInterval(23)
	}

	gotHandle, gotDeadline := armedTimer(iv)
	if gotHandle != handle || !gotDeadline.Equal(deadline) {
		t.Fatalf("three no-op interval writes moved the armed timer from (handle %d, deadline %v) "+
			"to (handle %d, deadline %v). A client that re-sends its current interval on every "+
			"poll would push the next advance out forever and the slideshow would never advance.",
			handle, deadline, gotHandle, gotDeadline)
	}

	// Positive control on the same viewer and the same probe: a REAL change does
	// move it. Without this the assertion above passes for an implementation that
	// never re-arms at all — which is the defect this whole change removes.
	gtkViewer{iv: iv}.SetInterval(24)
	movedHandle, movedDeadline := armedTimer(iv)
	if movedHandle == handle || !movedDeadline.After(deadline) {
		t.Fatalf("a genuine change from 23 s to 24 s did not re-arm: handle %d -> %d, deadline "+
			"%v -> %v. This test cannot tell a no-op from a re-arm, so its assertion above is "+
			"vacuous.", handle, movedHandle, deadline, movedDeadline)
	}
	if sourceIsArmed(handle) {
		t.Fatalf("source %d survived the real change to 24 s", handle)
	}
}

// TestTheIntervalChangeReArmsWhilePaused pins the paused DECISION.
//
// The slide timer runs while paused — startSlideshow re-arms on every tick and
// only the ADVANCE is gated on !isPaused — so there is a pending source to
// retire in this state exactly as in any other, and the choice is to re-arm it.
//
// The alternative (skip the re-arm while paused, on the ground that nothing is
// counting down) leaves the OLD duration armed under the NEW interval, so a
// resume serves out the remainder of an interval the operator already replaced:
// the same stale-timer defect, made invisible until the moment someone presses
// play. Nothing is user-visible at the instant of the change, because
// seconds_until_next is 0 while paused by contract; what the re-arm buys is that
// the resume is honest.
func TestTheIntervalChangeReArmsWhilePaused(t *testing.T) {
	iv := newControlTestViewer(600, "a.jpg")
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })
	iv.setPausedState(true)

	oldHandle, oldDeadline := armedTimer(iv)
	if oldHandle == 0 {
		t.Fatal("no timer armed while paused — the fixture contradicts the premise of this test, " +
			"which is that the slide timer keeps running and only the advance is gated")
	}

	before := time.Now()
	gtkViewer{iv: iv}.SetInterval(11)
	after := time.Now()

	newHandle, newDeadline := armedTimer(iv)
	if newHandle == oldHandle {
		t.Fatalf("the interval change did not re-arm while paused (handle still %d): a resume "+
			"would then serve out the rest of the OLD 600 s interval before the first advance",
			oldHandle)
	}
	if sourceIsArmed(oldHandle) {
		t.Fatalf("the old source %d is still armed: pausing must not exempt the re-arm from "+
			"retiring the pending timeout", oldHandle)
	}
	if newDeadline.Before(before.Add(11*time.Second)) || newDeadline.After(after.Add(11*time.Second)) {
		t.Fatalf("the new deadline is %v, want one 11 s interval from the re-arm", newDeadline)
	}
	if !newDeadline.Before(oldDeadline) {
		t.Fatalf("the deadline did not move in while paused: %v -> %v", oldDeadline, newDeadline)
	}

	// The contract while paused is unchanged: no countdown is running.
	if got := iv.snapshotAt(after).secondsUntilNext; got != 0 {
		t.Fatalf("paused: seconds_until_next = %d, want 0 — re-arming must not start a countdown "+
			"to a page turn that will not happen", got)
	}
	// And the resume finds the NEW interval, not the remains of the old one.
	iv.setPausedState(false)
	if got := iv.snapshotAt(after).secondsUntilNext; got != 11 {
		t.Fatalf("resumed: seconds_until_next = %d, want 11 — the timer armed while paused is "+
			"the one the resume counts down", got)
	}
}

// TestSetIntervalBeforeTheSlideshowStartsStoresWithoutArming covers the state in
// which there is no arming closure to run.
//
// It is reachable only during boot — main() calls startSlideshow before it starts
// the control API, so no request can land in the window — and the answer is that
// the value is STORED and nothing is armed, because the first arm will read it.
// Arming here instead would be the second arming site the whole design forbids.
//
// It is an invariant guard, not regression coverage: no bug of this shape has
// happened. It is here because a nil func value is a panic and the panic would
// be on the GTK main loop.
func TestSetIntervalBeforeTheSlideshowStartsStoresWithoutArming(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg")

	gtkViewer{iv: iv}.SetInterval(11) // must not panic

	if got := iv.slideInterval(); got != 11 {
		t.Fatalf("slide_interval = %d after SetInterval(11) with no slideshow running, want 11", got)
	}
	if handle, deadline := armedTimer(iv); handle != 0 || !deadline.IsZero() {
		t.Fatalf("SetInterval armed a timer (handle %d, deadline %v) before startSlideshow ran; "+
			"arming from anywhere but startSlideshow's own closure is the second arming site "+
			"seconds_until_next's honesty depends on there not being", handle, deadline)
	}
	if iv.rearmSlideTimer() {
		t.Fatal("rearmSlideTimer reported that it re-armed with no arming closure registered")
	}

	// Positive control: once the slideshow starts, the stored value is what gets
	// armed — so the "stores without arming" answer above is correct rather than
	// merely quiet.
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })
	armedAt := time.Now()
	if got := iv.snapshotAt(armedAt).secondsUntilNext; got != 11 {
		t.Fatalf("seconds_until_next = %d once the slideshow starts, want 11 — the interval set "+
			"before it started was not the one it armed", got)
	}
}

// TestSetIntervalRefusesAZeroRatherThanArmingAZeroDelayTimeout guards the belt
// on the direct caller, and it is a REGRESSION guard for a hazard the re-arm
// promoted, not an invariant guard.
//
// countdownFrom's comment in state.go names the damage: "glib.TimeoutAdd(0, …) in
// startSlideshow spinning the main loop through S3 GETs is the real one". Before
// this change a 0 that reached SetInterval was merely stored, and did its damage
// at the next tick. Now it would be armed IMMEDIATELY — a zero-delay timeout
// re-arming itself from its own callback, i.e. a wedged main loop and a display
// that stops responding to anything, including the control API that would let you
// fix it.
//
// POST /api/interval bounds to 1..3600 (pinned in internal/control), so this is
// not reachable over the network. That is exactly why the guard belongs here: the
// handler's bound is a second component's property, and this adapter is called
// directly by tests and by anything added later.
func TestSetIntervalRefusesAZeroRatherThanArmingAZeroDelayTimeout(t *testing.T) {
	for _, seconds := range []int{0, -1} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			iv := newControlTestViewer(30, "a.jpg")
			iv.startSlideshow()
			t.Cleanup(func() { retireArmedTimer(t, iv) })
			handle, deadline := armedTimer(iv)

			gtkViewer{iv: iv}.SetInterval(seconds)

			if got := iv.slideInterval(); got != 30 {
				t.Fatalf("SetInterval(%d) stored slide_interval = %d; it must refuse, not store. "+
					"A 0 here arms glib.TimeoutAdd(0, …), which spins the GTK main loop through "+
					"S3 GETs and wedges the display.", seconds, got)
			}
			gotHandle, gotDeadline := armedTimer(iv)
			if gotHandle != handle || !gotDeadline.Equal(deadline) {
				t.Fatalf("SetInterval(%d) re-armed the timer: (handle %d, deadline %v) -> "+
					"(handle %d, deadline %v)", seconds, handle, deadline, gotHandle, gotDeadline)
			}
			// And GLib agrees, not just this program's bookkeeping. The two probes
			// answer different questions and this file has both for a reason: a
			// refusal that somehow armed and retired a source would leave the
			// struct fields identical and the registry changed.
			if !sourceIsArmed(handle) {
				t.Fatalf("SetInterval(%d) left source %d unregistered on the main context, "+
					"even though the bookkeeping still names it: the refusal path touched "+
					"GLib when it should have done nothing at all", seconds, handle)
			}
		})
	}

	// Positive control, same viewer shape and same probe: a LEGAL value does get
	// through and does re-arm. Without it, `if true { return }` at the top of
	// SetInterval — the whole endpoint inert — passes both rows above.
	iv := newControlTestViewer(30, "a.jpg")
	iv.startSlideshow()
	t.Cleanup(func() { retireArmedTimer(t, iv) })
	handle, _ := armedTimer(iv)
	gtkViewer{iv: iv}.SetInterval(1) // the smallest value the handler accepts
	if got := iv.slideInterval(); got != 1 {
		t.Fatalf("SetInterval(1) stored %d, want 1 — this test cannot tell a refusal from an "+
			"implementation that refuses everything", got)
	}
	if gotHandle, _ := armedTimer(iv); gotHandle == handle {
		t.Fatal("SetInterval(1) did not re-arm; the refusals above are vacuous")
	}
}

// TestTheReArmHasOneRegistrationAndOneCaller is the ledger for the new seam, and
// it is the companion to TestTheSlideTimerHasExactlyOneArmingSite.
//
// That test pins the number of glib.TimeoutAdd sites at one. This one pins the
// two ends of the indirection that reaches it: exactly one place publishes the
// arming closure (startSlideshow, so the closure that gets re-run is the one that
// does retire-then-arm from a single interval read), and exactly one place runs
// it (gtkViewer.SetInterval, which is an R1 write and therefore on the GTK main
// loop).
//
// 🔴 The CALLER half is a thread-affinity guard wearing a ledger. A second call
// to rearmSlideTimer from gtkViewer.Rescan — the one write that runs on the HTTP
// handler goroutine — would call glib.SourceRemove and glib.TimeoutAdd off the
// GTK main loop. TestNothingOffTheGTKThreadCanReachAWidget cannot see it: its
// graph is about gtk widget calls, glib is not gtk, and it says in its own
// docstring that a func value in a STRUCT FIELD is a blind spot it does not
// close. This is the guard for that.
func TestTheReArmHasOneRegistrationAndOneCaller(t *testing.T) {
	registrations := packageCallSites(t, "setArmTimer")
	for _, s := range registrations {
		t.Logf("  %s:%d  setArmTimer() in %s()", s.File, s.Line, s.Func)
	}
	if len(registrations) != 1 || registrations[0].File != "main.go" ||
		registrations[0].Func != "startSlideshow" {
		t.Errorf("setArmTimer is called from %+v, want exactly one site in main.go "+
			"startSlideshow. The closure it publishes is what POST /api/interval re-runs, and it "+
			"is the only one that retires the pending source and arms its replacement from a "+
			"single interval read through a single swapTimeout.", registrations)
	}

	callers := packageCallSites(t, "rearmSlideTimer")
	for _, s := range callers {
		t.Logf("  %s:%d  rearmSlideTimer() in %s()", s.File, s.Line, s.Func)
	}
	if len(callers) != 1 || callers[0].File != "control_adapter.go" ||
		callers[0].Func != "SetInterval" {
		t.Errorf("rearmSlideTimer is called from %+v, want exactly one site in "+
			"control_adapter.go SetInterval. It calls glib.SourceRemove and glib.TimeoutAdd, so "+
			"every caller must be an R1 write running inside an Enqueue closure on the GTK main "+
			"loop — gtkViewer.Rescan in particular is NOT, and calling it from there would arm "+
			"GLib sources from an HTTP handler goroutine.", callers)
	}
}

// TestSnapshotReadsKeysAndTheCountdownUnderTheSameLock is a -race detection, not
// a structural check. Both new fields are read on the HTTP handler goroutine
// while the GTK thread writes them, so an unlocked read of either — or a
// retained slice — is a genuine race.
func TestSnapshotReadsKeysAndTheCountdownUnderTheSameLock(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg", "c.jpg")
	iv.setViewModeState(ViewLandscapeTwo)
	// Prime the record so the reader below is never racing the writer's FIRST
	// write: "empty" is a legitimate state and asserting against it would make
	// this a scheduling test rather than a race detection.
	iv.noteDisplayed([]string{"a.jpg", "b.jpg"})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the GTK thread: rendering and re-arming the timer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			iv.noteDisplayed(displayedPair(i%3, "a.jpg", (i+1)%3, "b.jpg"))
			iv.swapTimeout(glib.SourceHandle(i%7+1), time.Now().Add(time.Duration(i%30)*time.Second))
			iv.setPausedState(i%2 == 0)
		}
	}()

	for i := 0; i < 4000; i++ { // the HTTP goroutine
		s := iv.snapshot()
		if len(s.keys) == 0 || len(s.keys) > 2 {
			t.Errorf("keys = %v, want one or two entries", s.keys)
			break
		}
		if s.secondsUntilNext < 0 || s.secondsUntilNext > int(s.slideInterval) {
			t.Errorf("seconds_until_next = %d, outside [0, %d]", s.secondsUntilNext, s.slideInterval)
			break
		}
	}
	close(stop)
	wg.Wait()
	// No cleanup: the handles above are bare integers, not real GLib sources —
	// nothing was registered on the main context, so there is nothing to remove.
	// An earlier version ended with a swapTimeout(0, …) that read as cleanup and
	// cleaned nothing up.
}
