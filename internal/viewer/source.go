package viewer

import (
	"math"
	"slices"
	"sort"

	"tracelight/internal/trace"
)

// A source is where the timeline's events come from: a trace in memory
// (localSource), a server's tiles (remoteSource), or several of either on
// one time axis (sessionSource). The renderer draws, hit-tests and shows
// details through it alone, so one renderer serves them all; the trace the
// App holds then only lays out the rows (for a remote trace, a skeleton of
// rows with no events).
//
// Times are nanoseconds relative to the start of what is shown.
type source interface {
	// frame tells the source the time and zoom about to be painted, before
	// the lanes are asked for: a remote one picks its tiles for it.
	frame(t0, t1, scale float64)
	// lod appends what to draw of lane li of track ti over [t0, t1) at
	// scale nanoseconds a pixel.
	lod(ti, li int, t0, t1, scale float64, out []vblock) []vblock
	// hit returns the event under time t of a lane, or the nearest within
	// tol nanoseconds: as a block whose Count says how many events share
	// those pixels (and then Start, End and Busy are theirs).
	hit(ti, li int, t, tol, scale float64) (vblock, bool)
	// event returns an event's details; false while they load.
	event(r trace.Ref) (*eventInfo, bool)
	// top returns the 100 names of a kind with the most time, over [from,
	// to) when ranged; false while they load.
	top(k trace.Kind, ranged bool, from, to int64) ([]trace.NameStat, bool)
	// memBytes is the size of a memcpy or memset, where the source has it
	// at hand (for tooltips).
	memBytes(r trace.Ref) (int64, bool)
	// loading is the number of requests in flight.
	loading() int
}

// vblock is something to draw in a lane: an event, or events merged.
type vblock struct {
	start, end int64
	name       string
	kind       trace.Kind
	// count is the number of events; index the (first) event's in the lane.
	count, index int
	busy         int64
	merged       bool
}

func (b *vblock) density() float64 {
	if b.end <= b.start {
		return 1
	}
	return min(float64(b.busy)/float64(b.end-b.start), 1)
}

// eventInfo is an event's details.
type eventInfo struct {
	ref          trace.Ref
	name, full   string
	kind         trace.Kind
	start, end   int64
	group, track string
	corr         int32
	kernel       *trace.KernelInfo
	memop        *trace.MemOpInfo
	// linked is the event the correlation id links it to, if any.
	linked *eventInfo
	// stack is the Python stack it ran under, the outermost frame first
	// (trace.PythonStack); python says the trace has Python calls at all.
	stack  []trace.StackFrame
	python bool
	// ops are the operators and annotations it ran under on its thread,
	// the outermost first (trace.OperatorStack).
	ops []trace.StackFrame
}

// ---------------------------------------------------------------- local

// localSource draws a trace in memory, with its per-pixel LOD.
type localSource struct {
	tr     *trace.Trace
	blocks []trace.Block
	tops   map[topKey][]trace.NameStat
}

type topKey struct {
	kind     trace.Kind
	ranged   bool
	from, to int64
}

func newLocalSource(tr *trace.Trace) *localSource {
	tr.Overview()
	return &localSource{tr: tr}
}

func (l *localSource) frame(t0, t1, scale float64) {}
func (l *localSource) loading() int                { return 0 }

func (l *localSource) lod(ti, li int, t0, t1, scale float64, out []vblock) []vblock {
	tr := l.tr
	lane := tr.Tracks[ti].Lanes[li]
	a := tr.Start + int64(math.Floor(t0))
	b := tr.Start + int64(math.Ceil(t1)) + 1
	l.blocks = lane.LOD(a, b, scale, 1, 5, l.blocks[:0])
	for i := range l.blocks {
		bl := &l.blocks[i]
		e := &lane.Events[bl.Index]
		out = append(out, vblock{start: bl.Start - tr.Start, end: bl.End - tr.Start, name: tr.Name(e), kind: e.Kind,
			count: bl.Count, index: bl.Index, busy: bl.Busy, merged: bl.Merged})
	}
	return out
}

func (l *localSource) hit(ti, li int, t, tol, scale float64) (vblock, bool) {
	tr := l.tr
	lane := tr.Tracks[ti].Lanes[li]
	at := tr.Start + int64(math.Round(t))
	idx := lane.Hit(at, int64(math.Ceil(tol)))
	if idx < 0 {
		return vblock{}, false
	}
	e := &lane.Events[idx]
	b := vblock{start: e.Start - tr.Start, end: e.End - tr.Start, name: tr.Name(e), kind: e.Kind, count: 1, index: idx, busy: e.Dur()}
	if float64(e.Dur()) < scale {
		// Sub-pixel: say how many share these pixels.
		i, j := lane.Visible(at-int64(1.5*scale), at+int64(1.5*scale)+1)
		if j-i > 1 {
			b.count, b.busy = j-i, lane.BusyBetween(i, j)
			b.start, b.end = lane.Events[i].Start-tr.Start, lane.Events[j-1].End-tr.Start
			b.merged = true
		}
	}
	return b, true
}

func (l *localSource) info(r trace.Ref) *eventInfo {
	tr := l.tr
	e := tr.Event(r)
	tk := tr.Tracks[r.Track]
	in := &eventInfo{ref: r, name: tr.Name(e), kind: e.Kind, start: e.Start - tr.Start, end: e.End - tr.Start,
		group: tk.Group, track: tk.Name, corr: e.Corr}
	switch e.Kind {
	case trace.KindKernel:
		k := tr.Kernels[e.Extra]
		in.kernel = &k
		in.full = tr.Names[k.FullName]
	case trace.KindMemcpy, trace.KindMemset:
		m := tr.MemOps[e.Extra]
		in.memop = &m
	}
	return in
}

func (l *localSource) event(r trace.Ref) (*eventInfo, bool) {
	in := l.info(r)
	if other, ok := l.tr.Correlated(r); ok {
		in.linked = l.info(other)
	}
	if l.tr.Counts[trace.KindPython] > 0 {
		in.python = true
		in.stack = l.tr.PythonStack(r)
	}
	if l.tr.Counts[trace.KindCPUOp] > 0 {
		in.ops = l.tr.OperatorStack(r)
	}
	return in, true
}

func (l *localSource) top(k trace.Kind, ranged bool, from, to int64) ([]trace.NameStat, bool) {
	key := topKey{k, ranged, from, to}
	if st, ok := l.tops[key]; ok {
		return st, true
	}
	if l.tops == nil || len(l.tops) > 32 {
		l.tops = map[topKey][]trace.NameStat{}
	}
	var st []trace.NameStat
	if ranged {
		st = l.tr.TopInRange(100, l.tr.Start+from, l.tr.Start+to, k)
	} else {
		st = l.tr.TopByTotal(100, k)
	}
	l.tops[key] = st
	return st, true
}

func (l *localSource) memBytes(r trace.Ref) (int64, bool) {
	e := l.tr.Event(r)
	if e.Kind != trace.KindMemcpy && e.Kind != trace.KindMemset {
		return 0, false
	}
	return l.tr.MemOps[e.Extra].Bytes, true
}

// ---------------------------------------------------------------- session

// part is a trace of a session.
type part struct {
	src source
	// tr is the part's trace (or skeleton); its tracks are the session's
	// from first on.
	tr    *trace.Trace
	first int
	// label names it in the session's group headings; key in links
	// (traceBase of its path: rank3).
	label, key string
	// utc and mono are its capture's start on the wall clock and on its
	// host's monotonic clock (0 if unknown); start its first event's time
	// from the capture's start (alignTraces).
	utc, mono, start int64
	host             string
	// coll are the ends of its collective kernels by operation, in ns from
	// its first event (trace.Collectives), for aligning on them.
	coll map[string][]float64
	// align is where alignSession put it, and on what.
	align alignOut
	basis string
	// offset is where its time 0 is on the session's axis.
	offset int64
}

// sessionSource shows several traces on one time axis, as the web client
// does: each trace's rows under headings of its name, shifted so that they
// line up on the wall clock of their captures (or start together).
type sessionSource struct {
	parts []*part
	// trackPart is the part of each of the session's tracks.
	trackPart []int
	// focus is the part the summaries are of.
	focus int
	// mode is how traces are aligned (alignSession): "collectives" (the
	// default, ""), "abs" (their clocks) or "start".
	mode string
	// visible marks the parts with rows on screen (App.prepareStream); nil
	// for all.
	visible []bool
	// off marks the parts left out (traces.go); nil for none.
	off []bool
}

// newSession makes a session of parts, and the skeleton the timeline lays
// out: the parts' rows in order, under "label · group" headings.
func newSession(parts []*part, path string) (*sessionSource, *trace.Trace) {
	ss := &sessionSource{parts: parts}
	sk := &trace.Trace{Path: path, Names: []string{""}, Devices: map[int32]string{}, Counts: map[trace.Kind]int{}}
	for pi, p := range parts {
		p.first = len(sk.Tracks)
		for _, tk := range p.tr.Tracks {
			c := *tk
			c.Group = p.label + " · " + tk.Group
			sk.Tracks = append(sk.Tracks, &c)
			ss.trackPart = append(ss.trackPart, pi)
		}
		for k, n := range p.tr.Counts {
			sk.Counts[k] += n
		}
		// Each once: the ranks of one file, or of one job, say the same.
		for _, m := range p.tr.Missing {
			if !slices.Contains(sk.Missing, m) {
				sk.Missing = append(sk.Missing, m)
			}
		}
		for _, w := range p.tr.Warnings {
			if !slices.Contains(sk.Warnings, w) {
				sk.Warnings = append(sk.Warnings, w)
			}
		}
	}
	ss.align(sk)
	return ss, sk
}

// align sets each part's offset (alignTraces, as the browser does) and the
// skeleton's span.
func (ss *sessionSource) align(sk *trace.Trace) {
	// Wall clocks relative to the earliest, exactly (int64), as the
	// browser does with BigInt; +1 so that 0 says "none".
	var ref int64
	for _, p := range ss.parts {
		if p.utc > 0 && (ref == 0 || p.utc < ref) {
			ref = p.utc
		}
	}
	in := make([]alignIn, len(ss.parts))
	for i, p := range ss.parts {
		in[i] = alignIn{Mono: float64(p.mono), Host: p.host, Clock: p.tr.Clock, Start: float64(p.start), Coll: p.coll}
		if p.utc > 0 {
			in[i].UTC = float64(p.utc-ref) + 1
		}
	}
	var end int64
	for i, a := range alignSession(in, ss.alignMode()) {
		p := ss.parts[i]
		p.offset, p.basis, p.align = int64(math.Round(a.Offset)), a.Basis, a
		end = max(end, p.offset+p.tr.Duration())
	}
	sk.Start, sk.End = 0, end
}

// alignMode is how the traces are aligned (alignSession's mode).
func (ss *sessionSource) alignMode() string {
	if ss.mode == "" {
		return "collectives"
	}
	return ss.mode
}

// collOf converts a trace's collectives for alignSession.
func collOf(c map[string][]int64) map[string][]float64 {
	if len(c) == 0 {
		return nil
	}
	out := make(map[string][]float64, len(c))
	for op, v := range c {
		f := make([]float64, len(v))
		for i, x := range v {
			f[i] = float64(x)
		}
		out[op] = f
	}
	return out
}

func (ss *sessionSource) at(ti int) (*part, int) {
	p := ss.parts[ss.trackPart[ti]]
	return p, ti - p.first
}

func (ss *sessionSource) frame(t0, t1, scale float64) {
	for i, p := range ss.parts {
		o := float64(p.offset)
		if ss.visible != nil && !ss.visible[i] {
			p.src.frame(0, -1, scale) // nothing shows: nothing to fetch
			continue
		}
		p.src.frame(t0-o, t1-o, scale)
	}
}

func (ss *sessionSource) lod(ti, li int, t0, t1, scale float64, out []vblock) []vblock {
	p, pt := ss.at(ti)
	n := len(out)
	o := float64(p.offset)
	out = p.src.lod(pt, li, t0-o, t1-o, scale, out)
	for i := n; i < len(out); i++ {
		out[i].start += p.offset
		out[i].end += p.offset
	}
	return out
}

func (ss *sessionSource) hit(ti, li int, t, tol, scale float64) (vblock, bool) {
	p, pt := ss.at(ti)
	b, ok := p.src.hit(pt, li, t-float64(p.offset), tol, scale)
	b.start += p.offset
	b.end += p.offset
	return b, ok
}

func (ss *sessionSource) shift(in *eventInfo, p *part) *eventInfo {
	if in == nil {
		return nil
	}
	c := *in
	c.ref.Track += int32(p.first)
	shiftFrames := func(fs []trace.StackFrame) []trace.StackFrame {
		if fs == nil {
			return nil
		}
		out := make([]trace.StackFrame, len(fs))
		for i, f := range fs {
			f.Ref.Track += int32(p.first)
			out[i] = f
		}
		return out
	}
	c.stack, c.ops = shiftFrames(in.stack), shiftFrames(in.ops)
	c.start += p.offset
	c.end += p.offset
	c.group = p.label + " · " + c.group
	c.linked = ss.shift(in.linked, p)
	return &c
}

func (ss *sessionSource) event(r trace.Ref) (*eventInfo, bool) {
	p, pt := ss.at(int(r.Track))
	ss.focus = ss.trackPart[r.Track]
	in, ok := p.src.event(trace.Ref{Track: int32(pt), Lane: r.Lane, Index: r.Index})
	if !ok {
		return nil, false
	}
	return ss.shift(in, p), true
}

func (ss *sessionSource) top(k trace.Kind, ranged bool, from, to int64) ([]trace.NameStat, bool) {
	p := ss.parts[ss.focus]
	return p.src.top(k, ranged, from-p.offset, to-p.offset)
}

func (ss *sessionSource) memBytes(r trace.Ref) (int64, bool) {
	p, pt := ss.at(int(r.Track))
	return p.src.memBytes(trace.Ref{Track: int32(pt), Lane: r.Lane, Index: r.Index})
}

func (ss *sessionSource) loading() int {
	n := 0
	for _, p := range ss.parts {
		n += p.src.loading()
	}
	return n
}

// searchBlocks returns the first of blocks (sorted by start, apart) that
// ends at or after t.
func searchBlocks(blocks []vblock, t int64) int {
	return sort.Search(len(blocks), func(i int) bool { return blocks[i].end >= t })
}
