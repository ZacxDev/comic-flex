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

func (f *fakeViewer) Enqueue(fn func()) {
	f.mu.Lock()
	runner := f.enqueueRunner
	if runner == nil {
		f.queued = append(f.queued, fn)
	}
	f.mu.Unlock()
	if runner != nil {
		runner(fn)
	}
}

func (f *fakeViewer) Next()                  { f.record("Next") }
func (f *fakeViewer) Prev()                  { f.record("Prev") }
func (f *fakeViewer) SetPaused(p bool)       { f.record("SetPaused:" + boolStr(p)) }
func (f *fakeViewer) SetViewMode(m ViewMode) { f.record("SetViewMode:" + string(m)) }
func (f *fakeViewer) GotoKey(k string) {
	// Resolve first, then record: indexOf takes the same mutex record does.
	at := f.indexOf(k)
	f.record("GotoKey:" + k + "@" + at)
}
func (f *fakeViewer) GotoIndex(i int)     { f.record("GotoIndex:" + strconv.Itoa(i)) }
func (f *fakeViewer) SetInterval(sec int) { f.record("SetInterval:" + strconv.Itoa(sec)) }
func (f *fakeViewer) Rescan()             { f.record("Rescan") }

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
