// Package trace holds a GPU trace in memory, in a form made for drawing a
// timeline quickly: every row (a CUDA stream, a thread's CUDA API calls, a
// thread's NVTX ranges) is split into lanes of non-overlapping events sorted
// by start, so the visible part of a lane is found by binary search and
// sub-pixel events are merged into density blocks (see lod.go).
//
// The package does not depend on any UI code.
package trace

import (
	"cmp"
	"math"
	"runtime"
	"slices"
	"sync"
)

// Kind is what an event is.
type Kind uint8

const (
	KindKernel Kind = iota + 1
	KindMemcpy
	KindMemset
	KindRuntime
	KindNVTX
	KindNVTXMark
	// KindSync is a CUDA synchronization (CUPTI_ACTIVITY_KIND_SYNCHRONIZATION),
	// shown under the API call that waited.
	KindSync
	// KindCPUOp is an operator a framework ran on a CPU thread (PyTorch's
	// aten:: ops, autograd functions; a Chrome trace's ranges).
	KindCPUOp
	// KindPython is a Python function's call (PyTorch's profiler with
	// with_stack): a thread's calls nest as its stack.
	KindPython

	// LastKind is the last kind.
	LastKind = KindPython
)

func (k Kind) String() string {
	switch k {
	case KindKernel:
		return "Kernel"
	case KindMemcpy:
		return "Memcpy"
	case KindMemset:
		return "Memset"
	case KindRuntime:
		return "CUDA API"
	case KindNVTX:
		return "NVTX range"
	case KindNVTXMark:
		return "NVTX mark"
	case KindSync:
		return "Sync"
	case KindCPUOp:
		return "CPU op"
	case KindPython:
		return "Python"
	}
	return "Event"
}

// Event is one interval on the timeline. Times are nanoseconds, as in the
// nsys export.
type Event struct {
	Start, End int64
	// Name indexes Trace.Names.
	Name int32
	// Corr is the CUDA correlation id linking an API call to the GPU work it
	// launched, 0 if none.
	Corr int32
	// Extra indexes Trace.Kernels for kernels, Trace.MemOps for memcpy and
	// memset, and is -1 otherwise.
	Extra int32
	Kind  Kind
}

// Dur is the event's duration in nanoseconds.
func (e *Event) Dur() int64 { return e.End - e.Start }

// TrackKind tells what a track shows.
type TrackKind uint8

const (
	TrackStream  TrackKind = iota + 1 // kernels, memcpy and memset of one CUDA stream
	TrackRuntime                      // CUDA runtime API calls of one thread
	TrackNVTX                         // NVTX ranges of one thread
	TrackCPU                          // operators of one thread (PyTorch's cpu_op)
	TrackPython                       // Python calls of one thread, nested as its stack
)

// TrackKinds are the kinds of tracks, in their order under a heading.
var TrackKinds = []TrackKind{TrackStream, TrackRuntime, TrackNVTX, TrackCPU, TrackPython}

// threadOrder orders a thread's tracks as its stack, outermost first: its
// ranges (NVTX, annotations), its Python calls, the operators they ran,
// and at the bottom the CUDA API calls those made, the lowest level, just
// above the GPU work they launch.
var threadOrder = map[TrackKind]int64{TrackNVTX: 2, TrackPython: 3, TrackCPU: 4, TrackRuntime: 5}

// threadSuffix names a thread's track of kind k after its thread.
func threadSuffix(k TrackKind) string {
	switch k {
	case TrackRuntime:
		return " · CUDA API"
	case TrackCPU:
		return " · CPU ops"
	case TrackPython:
		return " · Python"
	}
	return " · NVTX"
}

// Lane is a sequence of events that do not overlap, sorted by start (and so
// by end too).
type Lane struct {
	Events []Event
	// prefixDur[i] is the summed duration of Events[:i].
	prefixDur []int64
}

// Track is one row of the timeline, made of one or more lanes (nesting
// depths, or overlapping work).
type Track struct {
	// Group is the heading the track is shown under ("GPU 0", "Process 42").
	Group string
	Name  string
	Kind  TrackKind
	Lanes []*Lane
	// Count is the number of events, Busy their summed duration.
	Count int
	Busy  int64
	// Device is the GPU of a stream's track, -1 for threads.
	Device int32
	// Thread is the thread of a thread's track (its globalTid), 0 for a
	// stream's: a thread's tracks (its CUDA API calls, its operators, its
	// Python calls) go together by it.
	Thread int64
	// Overview is the sub-lane of the rank overview's summary lane that each
	// of its first OverviewLanes lanes draws into (Trace.Overview); nil
	// until set, which draws them all into the first.
	Overview []int
	// sort keys
	groupOrder int64
	order      int64
	k          trackKey
}

// KernelInfo holds a kernel's launch configuration.
type KernelInfo struct {
	Device, Stream              int32
	GridX, GridY, GridZ         int32
	BlockX, BlockY, BlockZ      int32
	Registers                   int32
	StaticShared, DynamicShared int32
	// FullName indexes Trace.Names: the demangled name, where the event's
	// own name is the short one; 0 (the empty string) if unknown.
	FullName int32
}

// MemOpInfo describes a memcpy or memset.
type MemOpInfo struct {
	Device, Stream int32
	Bytes          int64
	// CopyKind is the CUDA memcpy kind for memcpy (1 HtoD, 2 DtoH, 8 DtoD,
	// 10 PtoP, ...), or -1 for memset.
	CopyKind int32
	Value    int32 // memset value
}

// Ref locates an event.
type Ref struct {
	Track, Lane, Index int32
}

// Trace is a whole loaded trace.
type Trace struct {
	Path string
	// Names are the strings events refer to by index.
	Names  []string
	Tracks []*Track
	// Start and End bound every event.
	Start, End int64
	Kernels    []KernelInfo
	MemOps     []MemOpInfo
	// Devices names the GPUs by device id, where the export has them.
	Devices map[int32]string
	// ThreadNames names threads by globalTid, where the export has them.
	ThreadNames map[int64]string
	// ProcessNames names processes by pid, where the export has them.
	ProcessNames map[int64]string
	// SessionStartUTC is when the capture started, in nanoseconds since
	// 1970 (UTC), which event times count from; 0 if the file does not say.
	// Traces of the ranks of one job line up on it.
	SessionStartUTC int64
	// SessionStartMono is when the capture started on the capturing host's
	// monotonic clock (CLOCK_MONOTONIC), in nanoseconds, or 0: the clock
	// event times count from, and that traces of one host share. Reports
	// store it (not the wall clock), as exports do (systemClockNs).
	SessionStartMono int64
	// Host is the name of the machine the trace was captured on, if known.
	Host string
	// Rank is the rank of a distributed job the trace says it is of
	// (PyTorch's distributedInfo, or the processes' names of a file of
	// several ranks merged), or -1.
	Rank int
	// Clock names the clock its times count on, where traces share one
	// without a host to say so: the file several ranks were merged into
	// (their monotonic starts, SessionStartMono, compare); "" otherwise.
	Clock string
	// Tables are the tables found in the file; Missing are the event tables
	// it did not have; Warnings are problems that did not stop the load.
	Tables   []string
	Missing  []string
	Warnings []string
	// Counts of events by kind.
	Counts map[Kind]int
	// Load says where the time of loading went.
	Load LoadStats

	corr      sync.Once
	overview  sync.Once
	pyOnce    sync.Once
	pyTracks  map[int64]int // the Python track of each thread
	thOnce    sync.Once
	thTracks  map[int64][]int // the operator and annotation tracks of each thread
	gpuByCorr []corrRef       // sorted by corr
	apiByCorr []corrRef
}

type corrRef struct {
	corr int32
	ref  Ref
}

// Event returns the event r refers to.
func (t *Trace) Event(r Ref) *Event {
	return &t.Tracks[r.Track].Lanes[r.Lane].Events[r.Index]
}

// Name returns the name of e.
func (t *Trace) Name(e *Event) string {
	if e.Name >= 0 && int(e.Name) < len(t.Names) {
		return t.Names[e.Name]
	}
	return "?"
}

// Correlated returns the event linked to the one at r by its correlation
// id: the GPU work an API call launched, or the API call that launched the
// GPU work.
func (t *Trace) Correlated(r Ref) (Ref, bool) {
	e := t.Event(r)
	if e.Corr == 0 {
		return Ref{}, false
	}
	var idx []corrRef
	t.corr.Do(t.indexCorrelations)
	switch e.Kind {
	case KindRuntime:
		idx = t.gpuByCorr
	case KindKernel, KindMemcpy, KindMemset:
		idx = t.apiByCorr
	default:
		return Ref{}, false
	}
	i, ok := slices.BinarySearchFunc(idx, e.Corr, func(c corrRef, v int32) int { return cmp.Compare(c.corr, v) })
	if !ok {
		return Ref{}, false
	}
	return idx[i].ref, true
}

// indexCorrelations sorts the API calls and the GPU work by correlation id.
func (t *Trace) indexCorrelations() {
	var gpu, api []corrRef
	for ti, tr := range t.Tracks {
		for li, l := range tr.Lanes {
			for i := range l.Events {
				ev := &l.Events[i]
				if ev.Corr == 0 {
					continue
				}
				c := corrRef{ev.Corr, Ref{int32(ti), int32(li), int32(i)}}
				switch ev.Kind {
				case KindRuntime:
					api = append(api, c)
				case KindKernel, KindMemcpy, KindMemset:
					gpu = append(gpu, c)
				}
			}
		}
	}
	byCorr := func(a, b corrRef) int { return cmp.Compare(a.corr, b.corr) }
	slices.SortFunc(gpu, byCorr)
	slices.SortFunc(api, byCorr)
	t.gpuByCorr, t.apiByCorr = gpu, api
}

// Duration is the time the trace covers.
func (t *Trace) Duration() int64 { return t.End - t.Start }

// EventCount is the number of events of every kind.
func (t *Trace) EventCount() int {
	n := 0
	for _, tr := range t.Tracks {
		n += tr.Count
	}
	return n
}

// Builder collects events into tracks and then sorts and packs them.
//
// Loading reads tables in parallel, each into a Builder of its own (a
// shard), which merge into one before Finish.
type Builder struct {
	t      *Trace
	events int
	counts [kindCount]int
	tracks map[trackKey]*building
	// order lists the track keys as they were first seen, so that shards
	// merge their events in the order they were read.
	order []trackKey
	names map[string]int32
	// ids holds the StringIds id of each name whose string is not read yet
	// (see nameID), or -1; byID maps small ids back to their name index
	// plus one, byIDMap the others.
	ids     []int64
	byID    []int32
	byIDMap map[int64]int32
	// last is the track of the previous event, which the next one is
	// usually on too.
	lastKey trackKey
	last    *building
	// syncs wait for their thread, known from the API call of the same
	// correlation id once every table is read.
	syncs []Event
	// capHint sizes the first track made.
	capHint int
}

const kindCount = int(LastKind) + 1

type trackKey struct {
	kind TrackKind
	a, b int64
}

type building struct {
	track  *Track
	events []Event
}

// NewBuilder starts a trace.
func NewBuilder(path string) *Builder {
	b := &Builder{
		t: &Trace{
			Path:         path,
			Devices:      map[int32]string{},
			ThreadNames:  map[int64]string{},
			ProcessNames: map[int64]string{},
			Rank:         -1,
			Counts:       map[Kind]int{},
		},
		tracks: map[trackKey]*building{},
		names:  map[string]int32{},
	}
	// Names[0] is the empty string, so that a zero name index means none
	// (as in KernelInfo.FullName).
	b.Intern("")
	return b
}

// Trace is the trace being built, for its metadata fields.
func (b *Builder) Trace() *Trace { return b.t }

// Intern returns the index of name in Names, adding it if new.
func (b *Builder) Intern(name string) int32 {
	if i, ok := b.names[name]; ok {
		return i
	}
	return b.newName(name, -1)
}

// internBytes is Intern for bytes it does not keep unless they are new.
func (b *Builder) internBytes(name []byte) int32 {
	if i, ok := b.names[string(name)]; ok { // no allocation for the lookup
		return i
	}
	return b.newName(string(name), -1)
}

func (b *Builder) newName(name string, id int64) int32 {
	i := int32(len(b.t.Names))
	b.t.Names = append(b.t.Names, name)
	b.ids = append(b.ids, id)
	if id < 0 {
		b.names[name] = i
	}
	return i
}

// nameID returns the index of a name given by its StringIds id, whose
// string is filled in when shards merge (resolveNames): only the strings
// events use are read, not the whole table.
func (b *Builder) nameID(id int64) int32 {
	if id >= 0 && id < 1<<24 {
		if id < int64(len(b.byID)) {
			if v := b.byID[id]; v != 0 {
				return v - 1
			}
		} else {
			b.byID = append(b.byID, make([]int32, int(id)+1-len(b.byID))...)
		}
		i := b.newName("", id)
		b.byID[id] = i + 1
		return i
	}
	if b.byIDMap == nil {
		b.byIDMap = map[int64]int32{}
	}
	if i, ok := b.byIDMap[id]; ok {
		return i
	}
	i := b.newName("", id)
	b.byIDMap[id] = i
	return i
}

// AddKernel adds a kernel to the track of its device and stream.
func (b *Builder) AddKernel(e Event, k KernelInfo) {
	e.Kind = KindKernel
	e.Extra = int32(len(b.t.Kernels))
	b.t.Kernels = append(b.t.Kernels, k)
	b.add(b.track(trackKey{TrackStream, int64(k.Device), int64(k.Stream)}), e)
}

// AddMemOp adds a memcpy or memset (e.Kind) to the track of its stream.
func (b *Builder) AddMemOp(e Event, m MemOpInfo) {
	e.Extra = int32(len(b.t.MemOps))
	b.t.MemOps = append(b.t.MemOps, m)
	b.add(b.track(trackKey{TrackStream, int64(m.Device), int64(m.Stream)}), e)
}

// AddRuntime adds a CUDA API call of the thread globalTid.
func (b *Builder) AddRuntime(globalTid int64, e Event) {
	e.Kind, e.Extra = KindRuntime, -1
	b.add(b.track(trackKey{TrackRuntime, globalTid, 0}), e)
}

// AddNVTX adds an NVTX range (or a mark, e.Kind KindNVTXMark) of the thread
// globalTid.
func (b *Builder) AddNVTX(globalTid int64, e Event) {
	if e.Kind == 0 {
		e.Kind = KindNVTX
	}
	e.Extra, e.Corr = -1, 0
	b.add(b.track(trackKey{TrackNVTX, globalTid, 0}), e)
}

// AddCPUOp adds an operator a framework ran on the thread globalTid.
func (b *Builder) AddCPUOp(globalTid int64, e Event) {
	e.Kind, e.Extra = KindCPUOp, -1
	b.add(b.track(trackKey{TrackCPU, globalTid, 0}), e)
}

// AddPython adds a Python call of the thread globalTid.
func (b *Builder) AddPython(globalTid int64, e Event) {
	e.Kind, e.Extra, e.Corr = KindPython, -1, 0
	b.add(b.track(trackKey{TrackPython, globalTid, 0}), e)
}

// AddSync adds a CUDA synchronization. It goes under the API call of the
// same correlation id, on that call's thread, when the trace is finished;
// it is dropped if there is none.
func (b *Builder) AddSync(e Event) {
	e.Kind, e.Extra = KindSync, -1
	if e.End < e.Start {
		e.End = e.Start
	}
	b.syncs = append(b.syncs, e)
}

func (b *Builder) add(bt *building, e Event) {
	if e.End < e.Start {
		e.End = e.Start
	}
	bt.events = append(bt.events, e)
	b.counts[e.Kind]++
	b.events++
}

// PidTid splits an nsys globalTid (or globalPid) into the process and
// thread ids.
func PidTid(globalTid int64) (pid, tid int64) {
	return (globalTid >> 24) & 0xFFFFFF, globalTid & 0xFFFFFF
}

// track returns the track of key, making it if new.
func (b *Builder) track(key trackKey) *building {
	if b.last != nil && key == b.lastKey {
		return b.last
	}
	bt, ok := b.tracks[key]
	if !ok {
		bt = &building{track: newTrack(key)}
		if b.capHint > 0 {
			bt.events = make([]Event, 0, b.capHint)
			b.capHint = 0 // only the first: the others are usually small
		}
		b.tracks[key] = bt
		b.order = append(b.order, key)
	}
	b.lastKey, b.last = key, bt
	return bt
}

func newTrack(key trackKey) *Track {
	if key.kind == TrackStream {
		return &Track{
			k:          key,
			Kind:       TrackStream,
			Group:      "GPU " + itoa(key.a),
			Name:       "Stream " + itoa(key.b),
			Device:     int32(key.a),
			groupOrder: key.a,
			order:      key.b,
		}
	}
	pid, tid := PidTid(key.a)
	name := "Thread " + itoa(tid) + threadSuffix(key.kind)
	return &Track{
		k:          key,
		Kind:       key.kind,
		Thread:     key.a,
		Group:      "Process " + itoa(pid),
		Name:       name,
		Device:     -1,
		groupOrder: 1<<40 + pid,
		order:      tid*8 + threadOrder[key.kind],
	}
}

// merge moves the events of shards into b, in order. Their names must be
// resolved (resolveNames) to strings first.
func (b *Builder) merge(shards []*Builder) {
	// How many events each track gets, to allocate them once.
	total := map[trackKey]int{}
	for _, sh := range shards {
		for key, bt := range sh.tracks {
			total[key] += len(bt.events)
		}
	}
	names := len(b.t.Names)
	for _, sh := range shards {
		names += len(sh.t.Names)
	}
	if len(b.names) < names {
		// Grown once rather than doubling through hundreds of thousands
		// of names (NVTX texts that embed a sequence number are each new).
		grown := make(map[string]int32, names)
		for k, v := range b.names {
			grown[k] = v
		}
		b.names = grown
		b.t.Names = slices.Grow(b.t.Names, names-len(b.t.Names))
		b.ids = slices.Grow(b.ids, names-len(b.ids))
	}
	remaps := make([][]int32, len(shards))
	for si, sh := range shards {
		remap := make([]int32, len(sh.t.Names))
		for i, n := range sh.t.Names {
			remap[i] = b.Intern(n)
		}
		remaps[si] = remap
	}
	// Rename and renumber in parallel, each shard its own events.
	kBase, mBase := make([]int32, len(shards)), make([]int32, len(shards))
	nk, nm := int32(len(b.t.Kernels)), int32(len(b.t.MemOps))
	for si, sh := range shards {
		kBase[si], mBase[si] = nk, nm
		nk += int32(len(sh.t.Kernels))
		nm += int32(len(sh.t.MemOps))
	}
	var wg sync.WaitGroup
	for si, sh := range shards {
		wg.Go(func() {
			remap := remaps[si]
			for _, bt := range sh.tracks {
				for i := range bt.events {
					e := &bt.events[i]
					e.Name = remap[e.Name]
					switch e.Kind {
					case KindKernel:
						e.Extra += kBase[si]
					case KindMemcpy, KindMemset:
						e.Extra += mBase[si]
					}
				}
			}
			for i := range sh.syncs {
				sh.syncs[i].Name = remap[sh.syncs[i].Name]
			}
			for i := range sh.t.Kernels {
				sh.t.Kernels[i].FullName = remap[sh.t.Kernels[i].FullName]
			}
		})
	}
	wg.Wait()
	for _, sh := range shards {
		b.t.Kernels = append(b.t.Kernels, sh.t.Kernels...)
		b.t.MemOps = append(b.t.MemOps, sh.t.MemOps...)
		for _, key := range sh.order {
			src := sh.tracks[key]
			dst := b.track(key)
			if dst.events == nil && len(src.events) == total[key] {
				dst.events = src.events // the only shard with this track: no copy
			} else {
				if dst.events == nil {
					dst.events = make([]Event, 0, total[key])
				}
				dst.events = append(dst.events, src.events...)
			}
			src.events = nil
		}
		for k, n := range sh.counts {
			b.counts[k] += n
		}
		b.events += sh.events
		b.syncs = append(b.syncs, sh.syncs...)
		sh.t.Kernels, sh.t.MemOps, sh.syncs = nil, nil, nil
	}
}

// placeSyncs puts the synchronizations on the CUDA API tracks of the calls
// that waited for them.
func (b *Builder) placeSyncs() {
	if len(b.syncs) == 0 {
		return
	}
	// Correlation ids are small and dense: index a slice by them when
	// they are, and fall back to a map.
	maxCorr := int32(0)
	for _, e := range b.syncs {
		maxCorr = max(maxCorr, e.Corr)
	}
	var bySlice []*building
	byMap := map[int32]*building{}
	if maxCorr < 1<<26 {
		bySlice = make([]*building, maxCorr+1)
	}
	for key, bt := range b.tracks {
		if key.kind != TrackRuntime {
			continue
		}
		for i := range bt.events {
			c := bt.events[i].Corr
			switch {
			case c <= 0:
			case bySlice != nil:
				if c <= maxCorr {
					bySlice[c] = bt
				}
			default:
				byMap[c] = bt
			}
		}
	}
	dropped := 0
	for _, e := range b.syncs {
		var bt *building
		if bySlice != nil {
			if e.Corr > 0 {
				bt = bySlice[e.Corr]
			}
		} else {
			bt = byMap[e.Corr]
		}
		if bt == nil {
			dropped++
			continue
		}
		b.add(bt, e)
	}
	if dropped > 0 {
		b.t.Warnings = append(b.t.Warnings, itoa(int64(dropped))+" synchronization events without an API call were left out")
	}
	b.syncs = nil
}

// Finish sorts and packs the events and returns the trace.
func (b *Builder) Finish() *Trace {
	b.placeSyncs()
	t := b.t
	for k, n := range b.counts {
		if n > 0 {
			t.Counts[Kind(k)] = n
		}
	}
	// Name groups and tracks after their devices and threads, now that the
	// metadata tables are read.
	for key, bt := range b.tracks {
		tr := bt.track
		switch key.kind {
		case TrackStream:
			if n, ok := t.Devices[int32(key.a)]; ok && n != "" {
				tr.Group += " · " + n
			}
		default:
			pid, _ := PidTid(key.a)
			if n := t.ProcessNames[pid]; n != "" {
				tr.Group += " · " + n
			}
			if n, ok := t.ThreadNames[key.a]; ok && n != "" {
				_, tid := PidTid(key.a)
				tr.Name = n + " (" + itoa(tid) + ")" + threadSuffix(key.kind)
			}
		}
		if len(bt.events) > 0 {
			t.Tracks = append(t.Tracks, tr)
		}
	}
	// Pack the tracks in parallel, the largest first.
	slices.SortFunc(t.Tracks, func(x, y *Track) int {
		return cmp.Compare(len(b.eventsOf(y)), len(b.eventsOf(x)))
	})
	jobs := make(chan *Track)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), max(len(t.Tracks), 1)) {
		wg.Go(func() {
			for tr := range jobs {
				ev := b.eventsOf(tr)
				tr.Lanes = Pack(ev)
				tr.Count = len(ev)
				for _, l := range tr.Lanes {
					tr.Busy += l.prefixDur[len(l.Events)]
				}
			}
		})
	}
	for _, tr := range t.Tracks {
		jobs <- tr
	}
	close(jobs)
	wg.Wait()
	b.tracks = nil
	slices.SortFunc(t.Tracks, func(x, y *Track) int {
		return cmp.Or(cmp.Compare(x.groupOrder, y.groupOrder), cmp.Compare(x.order, y.order), cmp.Compare(x.Name, y.Name))
	})

	first := true
	for _, tr := range t.Tracks {
		for _, l := range tr.Lanes {
			if len(l.Events) == 0 {
				continue
			}
			s, e := l.Events[0].Start, l.Events[len(l.Events)-1].End
			if first || s < t.Start {
				t.Start = s
			}
			if first || e > t.End {
				t.End = e
			}
			first = false
		}
	}
	// Index correlation ids in the background: only a selection needs it.
	go t.corr.Do(t.indexCorrelations)
	return t
}

// shift moves every event added so far by d.
func (b *Builder) shift(d int64) {
	for _, bt := range b.tracks {
		for i := range bt.events {
			bt.events[i].Start += d
			bt.events[i].End += d
		}
	}
	for i := range b.syncs {
		b.syncs[i].Start += d
		b.syncs[i].End += d
	}
}

func (b *Builder) eventsOf(tr *Track) []Event {
	return b.tracks[tr.k].events
}

// Pack sorts events by start (longer first on ties, so enclosing ranges come
// before what they enclose) and places each in the first lane where it does
// not overlap the lane's last event. For properly nested ranges, such as
// NVTX push/pop, the lane is the nesting depth. Events usually come sorted
// from the export, so the sort is skipped when it would change nothing, and
// a track that needs only one lane keeps its slice as that lane.
func Pack(events []Event) []*Lane {
	less := func(x, y Event) int {
		return cmp.Or(cmp.Compare(x.Start, y.Start), cmp.Compare(y.End, x.End))
	}
	// order is the sorted order of events, nil if they are sorted already.
	var order []int32
	if !slices.IsSortedFunc(events, less) {
		order = sortOrder(events)
	}
	at := func(i int) *Event {
		if order != nil {
			return &events[order[i]]
		}
		return &events[i]
	}
	// First pass: the lane of each event, in sorted order.
	lane := make([]uint16, len(events))
	var ends []int64
	var counts []int
	for i := range events {
		e := at(i)
		l := -1
		for j := range ends {
			if ends[j] <= e.Start {
				l = j
				break
			}
		}
		if l < 0 {
			if len(ends) == 1<<16-1 {
				l = len(ends) - 1 // pathological overlap: let the last lane overlap
			} else {
				ends = append(ends, math.MinInt64) // free for any event, at any time (negative ones too)
				counts = append(counts, 0)
				l = len(ends) - 1
			}
		}
		ends[l] = max(ends[l], e.End)
		counts[l]++
		lane[i] = uint16(l)
	}
	if len(events) == 0 {
		return nil
	}
	if len(ends) == 1 && order == nil {
		return []*Lane{NewLane(events)} // one lane, already sorted: no copy
	}
	// Second pass: copy each event into its lane, allocated once.
	lanes := make([]*Lane, len(ends))
	for j := range lanes {
		lanes[j] = &Lane{Events: make([]Event, 0, counts[j])}
	}
	for i := range events {
		l := lanes[lane[i]]
		l.Events = append(l.Events, *at(i))
	}
	for _, l := range lanes {
		l.finish()
	}
	return lanes
}

// NewLane makes a lane of events that must already be sorted and must not
// overlap; for tests and callers that pack events themselves.
func NewLane(events []Event) *Lane {
	l := &Lane{Events: events}
	l.finish()
	return l
}

func (l *Lane) finish() {
	l.Events = slices.Clip(l.Events)
	l.prefixDur = make([]int64, len(l.Events)+1)
	for i := range l.Events {
		l.prefixDur[i+1] = l.prefixDur[i] + l.Events[i].Dur()
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// IdleDevices returns the GPUs the export lists (TARGET_INFO_GPU) that ran
// no kernel, memcpy or memset in the trace, by id.
func (t *Trace) IdleDevices() []int32 {
	active := map[int32]bool{}
	for _, tr := range t.Tracks {
		if tr.Kind == TrackStream {
			active[tr.Device] = true
		}
	}
	var idle []int32
	for id := range t.Devices {
		if !active[id] {
			idle = append(idle, id)
		}
	}
	slices.Sort(idle)
	return idle
}
