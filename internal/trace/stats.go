package trace

import (
	"slices"
	"sort"
)

// NameStat sums up the events of one name.
type NameStat struct {
	Name            string
	Count           int
	Total, Min, Max int64
	// Share is Total over the summed duration of every event of the kind.
	Share float64
}

// Avg is the mean duration.
func (s *NameStat) Avg() int64 {
	if s.Count == 0 {
		return 0
	}
	return s.Total / int64(s.Count)
}

// TopByTotal sums the events of the kinds given by name and returns the n
// names with the largest total time (all of them if n <= 0), largest first.
//
// It reads only the tracks that can hold those kinds, and sums into a slice
// indexed by name rather than a map: one pass over a million events takes
// a few milliseconds.
func (t *Trace) TopByTotal(n int, kinds ...Kind) []NameStat {
	return t.top(n, 0, 0, false, kinds)
}

// TopInRange is TopByTotal over the time range [from, to) (in the trace's
// times, as Event's): each event counts by its overlap with the range,
// clipped to it, so Total, Min, Max, Avg and Share are of overlap durations;
// Count is the events overlapping the range (an instant one, such as an
// NVTX mark, when it falls in it).
//
// Lanes are sorted with their events apart, so a binary search finds the
// events of each lane in the range and only those are read: a narrow range
// costs little, and the whole trace no more than TopByTotal.
func (t *Trace) TopInRange(n int, from, to int64, kinds ...Kind) []NameStat {
	if to <= from {
		return []NameStat{}
	}
	return t.top(n, from, to, true, kinds)
}

func (t *Trace) top(n int, from, to int64, ranged bool, kinds []Kind) []NameStat {
	var want [kindCount]bool
	var tracks [8]bool
	for _, k := range kinds {
		want[k] = true
		switch k {
		case KindKernel, KindMemcpy, KindMemset:
			tracks[TrackStream] = true
		case KindRuntime, KindSync:
			tracks[TrackRuntime] = true
		case KindNVTX, KindNVTXMark:
			tracks[TrackNVTX] = true
		case KindCPUOp:
			tracks[TrackCPU] = true
		case KindPython:
			tracks[TrackPython] = true
		}
	}
	type acc struct {
		count           int
		total, min, max int64
	}
	sums := make([]acc, len(t.Names))
	var all int64
	for _, tr := range t.Tracks {
		if !tracks[tr.Kind] {
			continue
		}
		for _, l := range tr.Lanes {
			evs := l.Events
			if ranged {
				// Events ending before from, or starting at or after to, are
				// out: the lane's ends and starts are both sorted.
				i := sort.Search(len(evs), func(i int) bool { return evs[i].End >= from })
				j := sort.Search(len(evs), func(j int) bool { return evs[j].Start >= to })
				if i >= j {
					continue
				}
				evs = evs[i:j]
			}
			for i := range evs {
				e := &evs[i]
				if !want[e.Kind] {
					continue
				}
				d := e.Dur()
				if ranged {
					s, en := max(e.Start, from), min(e.End, to)
					switch {
					case en > s:
						d = en - s
					case e.Start == e.End && e.Start >= from:
						d = 0 // an instant in the range
					default:
						continue // ends just at from
					}
				}
				all += d
				a := &sums[e.Name]
				if a.count == 0 || d < a.min {
					a.min = d
				}
				a.max = max(a.max, d)
				a.count++
				a.total += d
			}
		}
	}
	// The top n by total time (ties by name), without making a stat for
	// each of what can be hundreds of thousands of names.
	better := func(x, y int) bool { // whether name x ranks before name y
		if sums[x].total != sums[y].total {
			return sums[x].total > sums[y].total
		}
		return t.Names[x] < t.Names[y]
	}
	var top []int
	if n > 0 {
		top = make([]int, 0, n)
	}
	for name := range sums {
		if sums[name].count == 0 {
			continue
		}
		switch {
		case n <= 0 || len(top) < n:
			top = append(top, name)
			if n > 0 && len(top) == n {
				// Keep the worst at the top of a heap ordered worst-first.
				heapify(top, better)
			}
		case better(name, top[0]):
			top[0] = name
			siftDown(top, 0, better)
		}
	}
	slices.SortFunc(top, func(x, y int) int {
		if better(x, y) {
			return -1
		}
		if better(y, x) {
			return 1
		}
		return 0
	})
	out := make([]NameStat, len(top))
	for i, name := range top {
		a := sums[name]
		out[i] = NameStat{Name: t.Names[name], Count: a.count, Total: a.total, Min: a.min, Max: a.max}
		if all > 0 {
			out[i].Share = float64(a.total) / float64(all)
		}
	}
	return out
}

// heapify orders h as a heap whose root is the worst element by better.
func heapify(h []int, better func(x, y int) bool) {
	for i := len(h)/2 - 1; i >= 0; i-- {
		siftDown(h, i, better)
	}
}

func siftDown(h []int, i int, better func(x, y int) bool) {
	for {
		worst, l, r := i, 2*i+1, 2*i+2
		if l < len(h) && better(h[worst], h[l]) {
			worst = l
		}
		if r < len(h) && better(h[worst], h[r]) {
			worst = r
		}
		if worst == i {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}
