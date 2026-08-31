package main

import (
	"sync"
	"testing"

	"github.com/ZacxDev/comic-flex/internal/control"
)

// These cover the state accessors added for the control API, and the adapter
// that bridges them to internal/control. They live in package main because that
// is where GTK lives; internal/control's own tests need no display.

func newControlTestViewer(interval uint, images ...string) *ImageViewer {
	return &ImageViewer{
		images: images,
		mutex:  &sync.RWMutex{},
		config: &Config{SlideInterval: interval},
	}
}

func TestSetPausedStateIsAbsoluteNotAToggle(t *testing.T) {
	iv := newControlTestViewer(30)

	// The property that matters: POST /api/pause means "be paused", so a repeat
	// must be idempotent. togglePaused would resume on the second call, which
	// is exactly the bug a retrying client would hit.
	iv.setPausedState(true)
	iv.setPausedState(true)
	if !iv.isPaused() {
		t.Fatal("two pauses resumed the slideshow — setPausedState is behaving like a toggle")
	}
	iv.setPausedState(false)
	iv.setPausedState(false)
	if iv.isPaused() {
		t.Fatal("two resumes paused the slideshow")
	}
}

func TestSlideIntervalRoundTrips(t *testing.T) {
	// 37 and 91: neither is the 30 default, neither is a power of two, and
	// neither is a multiple of the other.
	iv := newControlTestViewer(37)
	if got := iv.slideInterval(); got != 37 {
		t.Fatalf("slideInterval = %d, want 37", got)
	}
	iv.setSlideInterval(91)
	if got := iv.slideInterval(); got != 91 {
		t.Fatalf("after set, slideInterval = %d, want 91", got)
	}
	if iv.config.SlideInterval != 91 {
		t.Fatalf("the timer reads config.SlideInterval = %d, want 91 — the accessor wrote "+
			"somewhere the slideshow does not read", iv.config.SlideInterval)
	}
}

func TestIndexOfKey(t *testing.T) {
	iv := newControlTestViewer(30, "a/1.jpg", "b/2.jpg", "c/3.jpg")
	for key, want := range map[string]int{"a/1.jpg": 0, "b/2.jpg": 1, "c/3.jpg": 2} {
		got, ok := iv.indexOfKey(key)
		if !ok || got != want {
			t.Errorf("indexOfKey(%q) = (%d, %v), want (%d, true)", key, got, ok, want)
		}
	}
	if _, ok := iv.indexOfKey("missing.jpg"); ok {
		t.Error("indexOfKey found a key that is not in the gallery")
	}
	// An empty gallery must answer "no", not panic.
	empty := newControlTestViewer(30)
	if _, ok := empty.indexOfKey("a/1.jpg"); ok {
		t.Error("indexOfKey found a key in an empty gallery")
	}
}

func TestGotoKey(t *testing.T) {
	iv := newControlTestViewer(30, "a/1.jpg", "b/2.jpg", "c/3.jpg")
	if !iv.gotoKey("c/3.jpg") {
		t.Fatal("gotoKey did not find c/3.jpg")
	}
	if idx, key, ok := iv.currentKey(); !ok || idx != 2 || key != "c/3.jpg" {
		t.Fatalf("currentKey = (%d, %q, %v), want (2, c/3.jpg, true)", idx, key, ok)
	}
	// A key that has left the gallery is a no-op, and must not move the index.
	if iv.gotoKey("gone.jpg") {
		t.Fatal("gotoKey reported success for a missing key")
	}
	if idx, _, _ := iv.currentKey(); idx != 2 {
		t.Fatalf("a missing key moved the index to %d", idx)
	}
}

// TestGotoIndexClampsAfterTheGalleryShrinks is defect 3's shape: the handler
// bounds-checked against a snapshot, a rescan shortened the gallery, and the
// closure then runs with an index that is now out of range.
func TestGotoIndexClampsAfterTheGalleryShrinks(t *testing.T) {
	iv := newControlTestViewer(30, "a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg")

	// The handler validated index 4 against a five-image gallery...
	iv.setImages([]string{"a.jpg", "b.jpg"})
	// ...and by the time the closure runs there are two.
	if !iv.gotoIndex(4) {
		t.Fatal("gotoIndex reported nothing to show on a non-empty gallery")
	}
	idx, key, ok := iv.currentKey()
	if !ok || idx != 1 || key != "b.jpg" {
		t.Fatalf("currentKey = (%d, %q, %v), want (1, b.jpg, true) — the index was not clamped "+
			"and images[idx] would be out of range", idx, key, ok)
	}

	// Negative goes to the front, not to a negative index.
	if !iv.gotoIndex(-7) {
		t.Fatal("gotoIndex reported nothing to show")
	}
	if idx, _, _ := iv.currentKey(); idx != 0 {
		t.Fatalf("a negative index clamped to %d, want 0", idx)
	}

	// An empty gallery: no panic, and it reports there is nothing to render.
	empty := newControlTestViewer(30)
	if empty.gotoIndex(3) {
		t.Fatal("gotoIndex reported something to show on an empty gallery")
	}
}

func TestSnapshotIsOneConsistentRead(t *testing.T) {
	iv := newControlTestViewer(37, "a/1.jpg", "b/2.jpg", "c/3.jpg")
	iv.setViewModeState(ViewLandscapeTwo)
	iv.setPausedState(true)
	if !iv.tryBeginScan() {
		t.Fatal("tryBeginScan refused on an idle viewer")
	}
	if !iv.gotoKey("b/2.jpg") {
		t.Fatal("gotoKey failed")
	}

	got := iv.snapshot()
	want := viewerSnapshot{
		total:         3,
		index:         1,
		key:           "b/2.jpg",
		viewMode:      ViewLandscapeTwo,
		paused:        true,
		slideInterval: 37,
		scanning:      true,
	}
	if got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

// TestSnapshotOfAnUnscannedGalleryIsNotAnEmptyOne pins the distinction the API
// contract turns on.
func TestSnapshotOfAnUnscannedGalleryIsNotAnEmptyOne(t *testing.T) {
	iv := newControlTestViewer(30)

	if !iv.tryBeginScan() {
		t.Fatal("tryBeginScan refused on an idle viewer")
	}
	before := iv.snapshot()
	if before.total != 0 || !before.scanning {
		t.Fatalf("mid-scan snapshot = %+v, want total 0 and scanning true", before)
	}

	iv.endScan()
	after := iv.snapshot()
	if after.total != 0 || after.scanning {
		t.Fatalf("post-scan snapshot = %+v, want total 0 and scanning false", after)
	}
	if before == after {
		t.Fatal("an un-scanned gallery and a scanned-but-empty one produce identical snapshots — " +
			"a client cannot tell 'indexing…' from 'no comics'")
	}
}

// TestSnapshotNormalisesAnOutOfRangeIndex pins that snapshot() and currentKey()
// answer the SAME thing — a relationship, not one side of it.
//
// currentIndex is normally kept in range by the accessors, so a snapshot that
// read it raw would agree with currentKey on every path the other tests take.
// This one builds the state those accessors would never produce (a struct
// literal, as an interrupted rescan or a future writer could leave it) and
// checks the two still agree. Without it, dropping the wrapIndex call in
// snapshot() is invisible.
func TestSnapshotNormalisesAnOutOfRangeIndex(t *testing.T) {
	iv := &ImageViewer{
		images:       []string{"a.jpg", "b.jpg", "c.jpg"},
		currentIndex: 7, // out of range, and 7 % 3 == 1 rather than 0
		mutex:        &sync.RWMutex{},
		config:       &Config{SlideInterval: 30},
	}

	wantIdx, wantKey, ok := iv.currentKey()
	if !ok {
		t.Fatal("currentKey reported an empty gallery")
	}
	if wantIdx != 1 || wantKey != "b.jpg" {
		t.Fatalf("currentKey = (%d, %q); the fixture no longer exercises normalisation", wantIdx, wantKey)
	}

	got := iv.snapshot()
	if got.index != wantIdx || got.key != wantKey {
		t.Fatalf("snapshot = (index %d, key %q) but currentKey = (index %d, key %q) — "+
			"GET /api/state would report a position the renderer does not agree with",
			got.index, got.key, wantIdx, wantKey)
	}
}

// TestSnapshotOfAnEmptyGalleryHasNoKey guards against images[0] on an empty
// slice inside snapshot().
func TestSnapshotOfAnEmptyGalleryHasNoKey(t *testing.T) {
	iv := newControlTestViewer(30)
	got := iv.snapshot()
	if got.key != "" || got.index != 0 || got.total != 0 {
		t.Fatalf("empty snapshot = %+v, want zero total/index and an empty key", got)
	}
}

// ---------------------------------------------------------------------------
// The adapter
// ---------------------------------------------------------------------------

func TestViewModeMappingRoundTrips(t *testing.T) {
	// Every internal mode maps to a distinct wire value and back. A mutant that
	// collapsed two modes onto one wire value fails here, and a mutant that
	// crossed two of them fails the round trip.
	seen := map[control.ViewMode]bool{}
	for _, mode := range []ViewMode{ViewLandscapeSingle, ViewPortraitSingle, ViewLandscapeTwo} {
		wire := toControlViewMode(mode)
		if seen[wire] {
			t.Fatalf("two internal modes map to the same wire value %q", wire)
		}
		seen[wire] = true

		if _, ok := control.ParseViewMode(string(wire)); !ok {
			t.Fatalf("toControlViewMode produced %q, which control.ParseViewMode rejects", wire)
		}
		if back := fromControlViewMode(wire); back != mode {
			t.Fatalf("round trip %v -> %q -> %v", mode, wire, back)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("mapped %d distinct wire values, want 3", len(seen))
	}
}

// TestWireViewModeValuesMatchTheConfigFile: config.yaml's view_mode accepts the
// same three strings, and parseViewMode is what reads it. Keeping them equal is
// what stops POST /api/viewmode and the config file disagreeing.
func TestWireViewModeValuesMatchTheConfigFile(t *testing.T) {
	for _, wire := range []control.ViewMode{
		control.ViewLandscapeSingle,
		control.ViewPortraitSingle,
		control.ViewLandscapeTwo,
	} {
		if got, want := parseViewMode(string(wire)), fromControlViewMode(wire); got != want {
			t.Errorf("config parseViewMode(%q) = %v but the API maps it to %v", wire, got, want)
		}
	}
}

func TestAdapterSnapshotMapsEveryField(t *testing.T) {
	iv := newControlTestViewer(37, "a/1.jpg", "b/2.jpg", "c/3.jpg")
	iv.setViewModeState(ViewPortraitSingle)
	iv.setPausedState(true)
	if !iv.tryBeginScan() {
		t.Fatal("tryBeginScan refused on an idle viewer")
	}
	if !iv.gotoKey("c/3.jpg") {
		t.Fatal("gotoKey failed")
	}

	got := gtkViewer{iv: iv}.Snapshot()
	want := control.Snapshot{
		Total:         3,
		Index:         2,
		Key:           "c/3.jpg",
		ViewMode:      "portrait_single",
		Paused:        true,
		SlideInterval: 37,
		Scanning:      true,
	}
	if got != want {
		t.Fatalf("adapter Snapshot = %+v, want %+v", got, want)
	}
}

func TestAdapterResolveMatchesTheGallery(t *testing.T) {
	iv := newControlTestViewer(30, "a/1.jpg", "b/2.jpg")
	g := gtkViewer{iv: iv}
	if i, ok := g.Resolve("b/2.jpg"); !ok || i != 1 {
		t.Fatalf("Resolve(b/2.jpg) = (%d, %v), want (1, true)", i, ok)
	}
	if _, ok := g.Resolve("missing.jpg"); ok {
		t.Error("Resolve found a key that is not in the gallery")
	}
}

// TestAdapterSetIntervalRejectsNegative: the handler bounds-checks 1..3600, but
// the adapter converts to uint, where a negative would wrap to an enormous
// interval rather than erroring.
func TestAdapterSetIntervalRejectsNegative(t *testing.T) {
	iv := newControlTestViewer(30)
	g := gtkViewer{iv: iv}
	g.SetInterval(-1)
	if got := iv.slideInterval(); got != 30 {
		t.Fatalf("a negative interval wrapped to %d; the slideshow would never advance again", got)
	}
	g.SetInterval(45)
	if got := iv.slideInterval(); got != 45 {
		t.Fatalf("slideInterval = %d, want 45", got)
	}
}

// TestControlServerRefusesToStartWithoutAToken pins the fail-closed decision at
// the seam main() actually uses, not just inside internal/control.
//
// 🔴 This is only HALF the property, and on its own it is walkable: inserting
// `if true { return nil }` at the top of startControlAPI makes the entire
// control API inert and this test still passes, because "returned nil" is
// exactly what it asserts. The other half is
// TestStartControlAPIServesOnItsAddress, which must stay next to it.
func TestControlServerRefusesToStartWithoutAToken(t *testing.T) {
	t.Setenv(control.TokenEnvVar, "")
	iv := newControlTestViewer(30)
	if srv := startControlAPI(iv, "127.0.0.1:0"); srv != nil {
		t.Fatal("startControlAPI returned a server with no token set — a control port would " +
			"bind on a host with passwordless sudo")
	}

	t.Setenv(control.TokenEnvVar, "short")
	if srv := startControlAPI(iv, "127.0.0.1:0"); srv != nil {
		t.Fatal("startControlAPI returned a server for a 5-byte token")
	}
}
