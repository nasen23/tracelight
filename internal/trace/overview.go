package trace

import (
	"cmp"
	"slices"
)

// The rank overview's sub-lanes: the overview draws the rows of a kind (a
// trace's CUDA streams, its threads' CUDA API calls or NVTX ranges) into a
// summary lane; work that runs at the same time on several of them would
// draw over each other there. OverviewSlots packs the rows' lanes into a
// few sub-lanes instead: lanes that overlap in time go to different ones,
// lanes that do not (or hardly: under 1% of their busy time) share one,
// the busiest first, so that what runs in parallel shows side by side and
// idle streams take no room. Beyond the kind's cap, a lane goes where it
// overlaps least. It depends on the trace only, not on the view: a lane
// stays in its sub-lane at every zoom.
//
// The lanes of a thread's CUDA API calls and NVTX ranges are nesting
// depths, read top-down: they go as a band (packBands), the thread's
// depths in order in consecutive sub-lanes, and share one with another
// thread's only where they never overlap in time. 0.3.7 packed them lane
// by lane like streams, which put one thread's ranges in among another's
// (and a child above its parent) and drew some over each other.

// LaneRef is a lane of a row (Tracks[Row].Lanes[Lane]).
type LaneRef struct {
	Row  int `json:"r"`
	Lane int `json:"l"`
}

// OverviewCap is the most sub-lanes of a kind's summary lane.
var OverviewCap = map[TrackKind]int{TrackStream: 4, TrackRuntime: 4, TrackNVTX: 6, TrackCPU: 6, TrackPython: 6}

// OverviewDepth is the most lanes (nesting depths) of each CUDA API and
// NVTX row the overview draws.
var OverviewDepth = map[TrackKind]int{TrackRuntime: 2, TrackNVTX: 3, TrackCPU: 3, TrackPython: 3}

// OverviewLanes is how many lanes of a row of kind k, of n lanes, the
// overview draws: every lane of a stream (overlapping work), the outermost
// OverviewDepth of the others.
func OverviewLanes(k TrackKind, n int) int {
	if k == TrackStream {
		return n
	}
	return min(n, OverviewDepth[k], OverviewCap[k])
}

// overviewRange is the lanes [from, to) of tk the overview draws: those
// of OverviewLanes, but for a thread's Python calls, whose outermost depths
// are the script's own frames (a call each, as long as the trace), from
// the first depth with more than one call.
func overviewRange(tk *Track) (from, to int) {
	n := len(tk.Lanes)
	if tk.Kind == TrackPython {
		for from < n-1 && len(tk.Lanes[from].Events) <= 1 {
			from++
		}
	}
	return from, min(n, from+OverviewLanes(tk.Kind, n))
}

// Overview sets each track's Overview: the sub-lane of its summary lane
// each of its lanes draws into, of the lanes up to the last it draws (-1
// for one it does not). Once; it is cheap next to the load (well under 100
// ms on traces of a million events).
func (t *Trace) Overview() {
	t.overview.Do(func() {
		for _, slots := range t.OverviewSlots() {
			for si, slot := range slots {
				for _, r := range slot {
					tk := t.Tracks[r.Row]
					if tk.Overview == nil {
						_, to := overviewRange(tk)
						tk.Overview = make([]int, to)
						for i := range tk.Overview {
							tk.Overview[i] = -1
						}
					}
					tk.Overview[r.Lane] = si
				}
			}
		}
	})
}

// OverviewSlots returns, for each kind of row the trace has, the sub-lanes
// of its summary lane: each the lanes drawn into it, in order.
func (t *Trace) OverviewSlots() map[TrackKind][][]LaneRef {
	out := map[TrackKind][][]LaneRef{}
	for _, k := range TrackKinds {
		var refs []LaneRef
		for ri, tk := range t.Tracks {
			if tk.Kind != k {
				continue
			}
			from, to := overviewRange(tk)
			for li := from; li < to; li++ {
				refs = append(refs, LaneRef{ri, li})
			}
		}
		switch {
		case len(refs) == 0:
		case k == TrackStream:
			out[k] = t.packLanes(refs, OverviewCap[k])
		default:
			out[k] = t.packBands(refs, OverviewCap[k])
		}
	}
	return out
}

// packLanes packs lanes into at most max sub-lanes (see OverviewSlots).
func (t *Trace) packLanes(refs []LaneRef, max int) [][]LaneRef {
	events := func(r LaneRef) []Event { return t.Tracks[r.Row].Lanes[r.Lane].Events }
	busy := func(r LaneRef) int64 {
		var b int64
		for i := range events(r) {
			b += events(r)[i].Dur()
		}
		return b
	}
	b := make(map[LaneRef]int64, len(refs))
	for _, r := range refs {
		b[r] = busy(r)
	}
	slices.SortStableFunc(refs, func(x, y LaneRef) int {
		return cmp.Or(cmp.Compare(b[y], b[x]), cmp.Compare(x.Row, y.Row), cmp.Compare(x.Lane, y.Lane))
	})
	var slots [][]LaneRef
	for _, r := range refs {
		best, bestOv := -1, int64(0)
		for si, slot := range slots {
			var ov int64
			for _, m := range slot {
				ov += overlap(events(r), events(m))
			}
			if best < 0 || ov < bestOv {
				best, bestOv = si, ov
			}
		}
		fits := best >= 0 && bestOv*100 <= b[r]
		if !fits && len(slots) < max {
			slots = append(slots, []LaneRef{r})
			continue
		}
		slots[best] = append(slots[best], r)
	}
	for _, s := range slots {
		slices.SortFunc(s, func(x, y LaneRef) int { return cmp.Or(cmp.Compare(x.Row, y.Row), cmp.Compare(x.Lane, y.Lane)) })
	}
	return slots
}

// packBands packs the lanes of rows of nested ranges into at most max
// sub-lanes, a row's lanes (refs lists them in order) as a band: lane i of
// a row placed at offset o goes to sub-lane o+i. Bands that share sub-lanes
// line up, depth with depth: a band goes into a group of sub-lanes, at its
// top, where none of its lanes overlaps what is there (and it is no deeper
// than the group, unless the group is the last), else into a new group
// below; the busiest rows first. Past max sub-lanes, it goes where it
// overlaps least.
func (t *Trace) packBands(refs []LaneRef, max int) [][]LaneRef {
	type band struct {
		row   int
		lanes []LaneRef
		busy  int64
	}
	var bands []band
	for _, r := range refs {
		if len(bands) == 0 || bands[len(bands)-1].row != r.Row {
			bands = append(bands, band{row: r.Row, busy: t.Tracks[r.Row].Busy})
		}
		b := &bands[len(bands)-1]
		b.lanes = append(b.lanes, r)
	}
	slices.SortStableFunc(bands, func(x, y band) int { return cmp.Or(cmp.Compare(y.busy, x.busy), cmp.Compare(x.row, y.row)) })
	events := func(r LaneRef) []Event { return t.Tracks[r.Row].Lanes[r.Lane].Events }
	var slots [][]LaneRef
	var groups []int // the first sub-lane of each group
	for _, b := range bands {
		lanes := b.lanes[:min(len(b.lanes), max)]
		cost := func(off int) int64 {
			var ov int64
			for i, r := range lanes {
				if off+i < len(slots) {
					for _, m := range slots[off+i] {
						ov += overlap(events(r), events(m))
					}
				}
			}
			return ov
		}
		best, bestOv := -1, int64(0)
		for gi, off := range groups {
			end := len(slots)
			if gi+1 < len(groups) {
				end = groups[gi+1]
			}
			if off+len(lanes) > end && (gi+1 < len(groups) || off+len(lanes) > max) {
				continue
			}
			if ov := cost(off); ov == 0 {
				best, bestOv = off, 0
				break
			}
		}
		if best < 0 && len(slots)+len(lanes) <= max {
			best = len(slots)
			groups = append(groups, best)
		}
		if best < 0 { // full: where it overlaps least
			for off := 0; off+len(lanes) <= max; off++ {
				if ov := cost(off); best < 0 || ov < bestOv {
					best, bestOv = off, ov
				}
			}
		}
		for i, r := range lanes {
			for best+i >= len(slots) {
				slots = append(slots, nil)
			}
			slots[best+i] = append(slots[best+i], r)
		}
	}
	for _, s := range slots {
		slices.SortFunc(s, func(x, y LaneRef) int { return cmp.Or(cmp.Compare(x.Row, y.Row), cmp.Compare(x.Lane, y.Lane)) })
	}
	return slots
}

// overlap is the time two lanes' events (each sorted and apart) overlap.
func overlap(a, b []Event) int64 {
	var ov int64
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		s, e := max(a[i].Start, b[j].Start), min(a[i].End, b[j].End)
		if e > s {
			ov += e - s
		}
		if a[i].End < b[j].End {
			i++
		} else {
			j++
		}
	}
	return ov
}
