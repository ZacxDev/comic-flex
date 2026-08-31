package control

import (
	"strconv"
	"sync"
)

// fakeViewer stands in for the GTK-backed viewer. It records every call and,
// crucially, does NOT run enqueued closures until drain() is called — which is
// what lets the 202 tests PROVE the handler did not await the work rather than
// merely assert it.
type fakeViewer struct {
	mu sync.Mutex

	snap Snapshot
	// keys maps an object key to its index, standing in for the gallery.
	keys map[string]int

	// queued holds closures handed to Enqueue and not yet run.
	queued []func()
	// calls records mutation methods, in order, as "Name" or "Name:arg".
	calls []string
	// reads records the R2-class reads made on the handler goroutine.
	reads []string

	// enqueueRunner, when non-nil, replaces the default deferred behaviour.
	// Used by the blocked-GTK-loop test.
	enqueueRunner func(func())

	// capacity, when > 0, makes Enqueue refuse once that many closures are
	// outstanding — standing in for the real adapter's maxQueuedMutations.
	capacity int
	// refused counts the Enqueue calls that were turned away.
	refused int

	// scanCapacity, when > 0, makes Rescan refuse once that many listings are
	// in flight — standing in for the real adapter's maxConcurrentScans. It is
	// a SEPARATE budget from capacity because the two bound different things,
	// and conflating them is what made the round-1 rescan look bounded.
	scanCapacity int
	// scansInFlight counts listings Rescan started and endScan has not ended.
	scansInFlight int
	// scansRefused counts the Rescan calls that were turned away.
	scansRefused int
}

func newFakeViewer(keys ...string) *fakeViewer {
	f := &fakeViewer{keys: map[string]int{}}
	for i, k := range keys {
		f.keys[k] = i
	}
	f.snap = Snapshot{
		Total:         len(keys),
		ViewMode:      string(ViewLandscapeSingle),
		SlideInterval: 30,
	}
	if len(keys) > 0 {
		f.snap.Key = keys[0]
	}
	return f
}

func (f *fakeViewer) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeViewer) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, "Snapshot")
	return f.snap
}

func (f *fakeViewer) Resolve(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, "Resolve:"+key)
	i, ok := f.keys[key]
	return i, ok
}

func (f *fakeViewer) Enqueue(fn func()) bool {
	f.mu.Lock()
	if f.capacity > 0 && len(f.queued) >= f.capacity {
		f.refused++
		f.mu.Unlock()
		return false
	}
	runner := f.enqueueRunner
	if runner == nil {
		f.queued = append(f.queued, fn)
	}
	f.mu.Unlock()
	if runner != nil {
		runner(fn)
	}
	return true
}

// refusedCount reports how many Enqueue calls the fake turned away.
func (f *fakeViewer) refusedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}

func (f *fakeViewer) Next() { f.record("Next") }
func (f *fakeViewer) Prev() { f.record("Prev") }

// SetPaused writes the flag as well as recording the call. The write is what
// lets a toggle test tell an absolute write apart from a flip: without it every
// fake is permanently un-paused and `TogglePaused` would flip the same way
// forever, so a mutant that ignored the current value would survive.
func (f *fakeViewer) SetPaused(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Paused = p
	f.calls = append(f.calls, "SetPaused:"+boolStr(p))
}

// TogglePaused flips the flag and records the value it LANDED ON, under one
// acquisition of the fake's mutex — the same atomicity the real viewer's
// togglePaused provides.
//
// Recording the result rather than the call is what makes the direction of the
// flip visible in callLog, so a test can assert paused -> playing and
// playing -> paused separately instead of seeing an identical "TogglePaused"
// for both.
func (f *fakeViewer) TogglePaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Paused = !f.snap.Paused
	f.calls = append(f.calls, "TogglePaused:"+boolStr(f.snap.Paused))
	return f.snap.Paused
}

// setPaused replaces the flag from outside, standing in for the `p` keypress on
// the Pi — or another queued closure — moving it AFTER a handler answered 202
// and BEFORE the toggle closure runs. It is the fixture for the atomicity test,
// exactly as setKeys is for goto's.
func (f *fakeViewer) setPaused(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Paused = p
}

// pausedFlag reads the fake's paused flag without recording a Snapshot read, so
// a test can check the landed state without polluting readLog — which several
// tests assert is EMPTY.
func (f *fakeViewer) pausedFlag() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap.Paused
}

func (f *fakeViewer) SetViewMode(m ViewMode) { f.record("SetViewMode:" + string(m)) }
func (f *fakeViewer) GotoKey(k string) {
	// Resolve first, then record: indexOf takes the same mutex record does.
	at := f.indexOf(k)
	f.record("GotoKey:" + k + "@" + at)
}
func (f *fakeViewer) GotoIndex(i int)     { f.record("GotoIndex:" + strconv.Itoa(i)) }
func (f *fakeViewer) SetInterval(sec int) { f.record("SetInterval:" + strconv.Itoa(sec)) }

// Rescan stands in for the real adapter: it admits a listing or refuses, and it
// returns SYNCHRONOUSLY without queueing anything. The recorded call goes in
// calls (not reads) only when it was admitted, so a test can tell "started" from
// "refused" without reading the counter.
func (f *fakeViewer) Rescan() bool {
	f.mu.Lock()
	if f.scanCapacity > 0 && f.scansInFlight >= f.scanCapacity {
		f.scansRefused++
		f.mu.Unlock()
		return false
	}
	f.scansInFlight++
	f.calls = append(f.calls, "Rescan")
	f.mu.Unlock()
	return true
}

// endScan releases one listing, standing in for a ListImages returning.
func (f *fakeViewer) endScan() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scansInFlight > 0 {
		f.scansInFlight--
	}
}

// scansRefusedCount reports how many Rescan calls the fake turned away.
func (f *fakeViewer) scansRefusedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scansRefused
}

// indexOf resolves a key the way the real GotoKey does — at the moment the
// closure RUNS, against the gallery as it is then. The tests use this to prove
// resolution is deferred rather than captured in the handler.
func (f *fakeViewer) indexOf(k string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.keys[k]; ok {
		return strconv.Itoa(i)
	}
	return "missing"
}

// pendingCount reports how many closures are waiting to run.
func (f *fakeViewer) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queued)
}

// callLog returns a copy of the mutation calls made so far.
func (f *fakeViewer) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// readLog returns a copy of the read calls made so far.
func (f *fakeViewer) readLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reads))
	copy(out, f.reads)
	return out
}

// drain runs every queued closure, standing in for the GTK main loop turning.
func (f *fakeViewer) drain() {
	f.mu.Lock()
	pending := f.queued
	f.queued = nil
	f.mu.Unlock()
	for _, fn := range pending {
		fn()
	}
}

// setKeys replaces the gallery, standing in for a rescan landing between the
// handler's 404 lookup and the closure running.
func (f *fakeViewer) setKeys(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = map[string]int{}
	for i, k := range keys {
		f.keys[k] = i
	}
	f.snap.Total = len(keys)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
