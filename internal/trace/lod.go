package trace

import (
	"math"
	"sort"
)

// Block is something to draw for a lane: one event wide enough to show on
// its own, or a run of events too small to tell apart, merged.
type Block struct {
	// Start and End are the time the block covers.
	Start, End int64
	// Index is the event's index in the lane when Count is 1 and Merged is
	// false; for a merged block it is the first event's index.
	Index int
	// Count is the number of events in the block, Busy their summed
	// duration.
	Count int
	Busy  int64
	// Merged marks a density block.
	Merged bool
}

// Density is the fraction of the block's time that its events cover.
func (b *Block) Density() float64 {
	if b.End <= b.Start {
		return 1
	}
	d := float64(b.Busy) / float64(b.End-b.Start)
	return min(d, 1)
}

// Len is the number of events in the lane.
func (l *Lane) Len() int { return len(l.Events) }

// FirstEnding returns the index of the first event that ends after t. Since
// a lane's events do not overlap, ends are sorted as starts are.
func (l *Lane) FirstEnding(t int64) int {
	return sort.Search(len(l.Events), func(i int) bool { return l.Events[i].End > t })
}

// FirstStarting returns the index of the first event at or after index from
// that starts at or after t.
func (l *Lane) FirstStarting(from int, t int64) int {
	ev := l.Events[from:]
	return from + sort.Search(len(ev), func(i int) bool { return ev[i].Start >= t })
}

// Visible returns the range [i, j) of events that overlap [t0, t1).
func (l *Lane) Visible(t0, t1 int64) (i, j int) {
	i = l.FirstEnding(t0)
	j = l.FirstStarting(i, t1)
	// A zero-length event at t0 ends at t0; keep it.
	for i > 0 && l.Events[i-1].End == t0 && l.Events[i-1].Start == t0 {
		i--
	}
	return i, j
}

// BusyBetween returns the summed duration of events [i, j).
func (l *Lane) BusyBetween(i, j int) int64 { return l.prefixDur[j] - l.prefixDur[i] }

// LOD returns what to draw of the lane between t0 and t1 at nsPerPx
// nanoseconds a pixel, appended to out.
//
// Events at least minPx pixels wide come as blocks of their own. Smaller
// ones are merged by pixel column: for each column the pass binary searches
// to the first event starting after it, and takes the summed duration of
// those it skipped from a prefix sum, so the work is about
// O((columns + wide events) · log n) whatever the number of events. Adjacent
// columns merge into one block while their density stays in the same of
// `levels` steps (0 or 1 merges regardless of density).
//
// Columns are on a grid anchored at the lane's first event, not at t0, so
// that panning does not make merged blocks flicker (and so that large
// absolute timestamps do not lose precision).
func (l *Lane) LOD(t0, t1 int64, nsPerPx, minPx float64, levels int, out []Block) []Block {
	n := len(l.Events)
	if n == 0 || t1 <= t0 || nsPerPx <= 0 {
		return out
	}
	minDur := minPx * nsPerPx
	i, end := l.Visible(t0, t1)
	origin := l.Events[0].Start
	var cur Block // the open merged block
	open := false
	curCol := int64(math.MinInt64)
	level := func(b *Block) int {
		if levels <= 1 {
			return 0
		}
		return int(b.Density()*float64(levels-1) + 0.5)
	}
	flush := func() {
		if open {
			out = append(out, cur)
			open = false
		}
	}
	for i < end {
		e := &l.Events[i]
		if float64(e.Dur()) >= minDur {
			flush()
			out = append(out, Block{Start: e.Start, End: e.End, Index: i, Count: 1, Busy: e.Dur()})
			i++
			continue
		}
		col := int64(math.Floor(float64(e.Start-origin) / nsPerPx))
		colEnd := origin + int64(math.Ceil(float64(col+1)*nsPerPx))
		k := l.FirstStarting(i, colEnd)
		if k > end {
			k = end
		}
		// Only the last event starting in the column can be wide: the ones
		// before it end before it starts.
		last := k
		if k-1 > i && float64(l.Events[k-1].Dur()) >= minDur {
			last = k - 1
		}
		b := Block{
			Start:  e.Start,
			End:    l.Events[last-1].End,
			Index:  i,
			Count:  last - i,
			Busy:   l.BusyBetween(i, last),
			Merged: true,
		}
		if b.Count == 1 {
			b.Merged = false
		}
		switch {
		case open && col <= curCol+1 && level(&cur) == level(&b):
			cur.End = max(cur.End, b.End)
			cur.Count += b.Count
			cur.Busy += b.Busy
			cur.Merged = true
		default:
			flush()
			cur, open = b, true
		}
		curCol = col
		i = last
	}
	flush()
	return out
}

// Hit returns the index of the event under time t, or failing that the
// nearest one within tol nanoseconds, or -1.
func (l *Lane) Hit(t int64, tol int64) int {
	n := len(l.Events)
	if n == 0 {
		return -1
	}
	i := l.FirstEnding(t)
	if i < n && l.Events[i].Start <= t {
		return i
	}
	best, bestD := -1, tol+1
	if i < n {
		if d := l.Events[i].Start - t; d <= tol {
			best, bestD = i, d
		}
	}
	if i > 0 {
		if d := t - l.Events[i-1].End; d <= tol && d < bestD {
			best = i - 1
		}
	}
	return best
}
