package main

import (
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
// at all, until an operator does something. If either gate changes, this test
// fails and the contract paragraph has to be rewritten — which is the point.
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

// TestLoweringTheIntervalFreezesTheCountdownUntilTheTimerCatchesUp pins a KNOWN
// WRONG-LOOKING behaviour so that it is an asserted property rather than a
// surprise, and so that anyone who fixes it has to come here and say so.
//
// 🔴 SetInterval deliberately does not restart the running timer — "it takes
// effect on the next tick" — so after lowering the interval the armed source is
// still the OLD duration away, while the contract clamps the report to the NEW
// interval. The result is a countdown that sits still. Found by audit; measured
// here rather than described.
//
// There is no honest small answer available inside the [0, slide_interval] bound:
// the true remaining is the large number the bound forbids reporting. The real
// fix is for SetInterval to re-arm, which changes an existing endpoint's
// documented semantics and is a separate decision — see the contract note on
// Snapshot.SecondsUntilNext. This test exists so that decision is made
// deliberately.
func TestLoweringTheIntervalFreezesTheCountdownUntilTheTimerCatchesUp(t *testing.T) {
	iv := newControlTestViewer(600, "a.jpg")
	armed := time.Now()
	iv.swapTimeout(glib.SourceHandle(3), armed.Add(600*time.Second))

	// POST /api/interval lands. 11 is neither a factor nor a multiple of 600.
	iv.setSlideInterval(11)

	for _, tc := range []struct {
		elapsed time.Duration
		want    int
		note    string
	}{
		{0, 11, "clamped down from 600 the instant the interval changed"},
		{5 * time.Minute, 11, "still frozen: the armed source is 5 minutes from firing"},
		{589 * time.Second, 11, "frozen right up to the point the truth drops below 11"},
		{594 * time.Second, 6, "and only now does it start to move"},
		{600 * time.Second, 0, "the armed source is finally due"},
	} {
		got := iv.snapshotAt(armed.Add(tc.elapsed)).secondsUntilNext
		if got != tc.want {
			t.Errorf("%v after arming: seconds_until_next = %d, want %d (%s)",
				tc.elapsed, got, tc.want, tc.note)
		}
		if got > int(iv.slideInterval()) {
			t.Errorf("%v after arming: seconds_until_next = %d exceeds slide_interval %d — "+
				"the clamp that causes the freeze is also the contract, and it broke",
				tc.elapsed, got, iv.slideInterval())
		}
	}

	if previous := iv.swapTimeout(0, time.Time{}); previous != 3 {
		t.Fatalf("swapTimeout returned %d, want 3", previous)
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
