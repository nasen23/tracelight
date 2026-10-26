package trace

import (
	"cmp"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
)

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o644) }

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// randomLane makes n non-overlapping events with durations spread over
// several orders of magnitude.
func randomLane(rng *rand.Rand, n int) *Lane {
	ev := make([]Event, n)
	t := int64(1000)
	for i := range ev {
		t += rng.Int64N(50)
		d := int64(1) << rng.IntN(14)
		ev[i] = Event{Start: t, End: t + d, Name: int32(i % 7)}
		t += d
	}
	return NewLane(ev)
}

func TestVisible(t *testing.T) {
	l := NewLane([]Event{{Start: 0, End: 10}, {Start: 10, End: 20}, {Start: 30, End: 40}, {Start: 40, End: 40}, {Start: 50, End: 60}})
	cases := []struct {
		t0, t1 int64
		i, j   int
	}{
		{0, 100, 0, 5},
		{5, 15, 0, 2},
		{20, 30, 2, 2}, // the gap: nothing
		{20, 31, 2, 3},
		{40, 45, 3, 4}, // the zero-length event at 40, not the one ending at 40
		{60, 70, 5, 5},
		{-10, 0, 0, 0},
	}
	for _, c := range cases {
		if i, j := l.Visible(c.t0, c.t1); i != c.i || j != c.j {
			t.Errorf("Visible(%d, %d) = %d, %d; want %d, %d", c.t0, c.t1, i, j, c.i, c.j)
		}
	}
}

// LOD must account for every visible event exactly once, keep wide events
// whole, and produce about one block per pixel column at most.
func TestLODCoversEveryEvent(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	l := randomLane(rng, 20000)
	first, last := l.Events[0].Start, l.Events[len(l.Events)-1].End
	for _, width := range []float64{200, 1000, 3000} {
		for trial := range 20 {
			span := (last - first) >> rng.IntN(12)
			t0 := first + rng.Int64N(max(1, last-first-span+1))
			if trial == 0 {
				t0, span = first, last-first
			}
			t1 := t0 + span
			nsPerPx := float64(span) / width
			blocks := l.LOD(t0, t1, nsPerPx, 1, 4, nil)

			i, j := l.Visible(t0, t1)
			count, busy := 0, int64(0)
			next := i
			for _, b := range blocks {
				if b.Index != next {
					t.Fatalf("block starts at event %d, want %d", b.Index, next)
				}
				if !b.Merged && b.Count != 1 {
					t.Fatalf("unmerged block of %d events", b.Count)
				}
				if b.Count == 1 && !b.Merged {
					e := l.Events[b.Index]
					if b.Start != e.Start || b.End != e.End {
						t.Fatalf("single block %v for event %v", b, e)
					}
				}
				if b.Merged {
					for k := b.Index; k < b.Index+b.Count; k++ {
						if float64(l.Events[k].Dur()) >= nsPerPx {
							t.Fatalf("wide event %d (%d ns at %.1f ns/px) merged", k, l.Events[k].Dur(), nsPerPx)
						}
					}
				}
				next += b.Count
				count += b.Count
				busy += b.Busy
			}
			if next != j || count != j-i {
				t.Fatalf("blocks cover %d events, want %d (range %d..%d)", count, j-i, i, j)
			}
			if want := l.BusyBetween(i, j); busy != want {
				t.Fatalf("busy %d, want %d", busy, want)
			}
			// At most a block per column for the merged ones, plus wide events
			// (each at least a pixel), plus slack at the edges.
			if len(blocks) > int(2*width)+4 {
				t.Fatalf("%d blocks for %.0f pixels", len(blocks), width)
			}
		}
	}
}

// Fully zoomed out on a lane of a million tiny events, LOD does work in
// proportion to the pixels, not the events.
func TestLODScalesWithPixels(t *testing.T) {
	n := 1_000_000
	ev := make([]Event, n)
	for i := range ev {
		ev[i] = Event{Start: int64(i) * 100, End: int64(i)*100 + 60}
	}
	l := NewLane(ev)
	blocks := l.LOD(0, int64(n)*100, float64(n)*100/1000, 1, 1, nil)
	if len(blocks) != 1 {
		t.Errorf("%d blocks for a uniform lane, want 1 merged block", len(blocks))
	}
	if b := blocks[0]; b.Count != n || b.Density() < 0.59 || b.Density() > 0.61 {
		t.Errorf("block %+v density %.2f", b, b.Density())
	}
	// With density levels, uniform density still merges into one block.
	if blocks = l.LOD(0, int64(n)*100, float64(n)*100/1000, 1, 8, nil); len(blocks) != 1 {
		t.Errorf("%d blocks with 8 levels", len(blocks))
	}
}

func BenchmarkLODZoomedOut(b *testing.B) {
	rng := rand.New(rand.NewPCG(3, 4))
	l := randomLane(rng, 1_000_000)
	first, last := l.Events[0].Start, l.Events[len(l.Events)-1].End
	var out []Block
	for b.Loop() {
		out = l.LOD(first, last, float64(last-first)/2000, 1, 4, out[:0])
	}
}

func TestLODColumnsStableWhilePanning(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	l := randomLane(rng, 5000)
	nsPerPx := 5000.0
	a := l.LOD(1_000_000, 3_000_000, nsPerPx, 1, 4, nil)
	b := l.LOD(1_000_000+1234, 3_000_000+1234, nsPerPx, 1, 4, nil)
	// Blocks wholly inside both windows are the same.
	inner := func(bs []Block) map[[2]int64]int {
		m := map[[2]int64]int{}
		for _, x := range bs {
			if x.Start > 1_100_000 && x.End < 2_900_000 {
				m[[2]int64{x.Start, x.End}] = x.Count
			}
		}
		return m
	}
	ma, mb := inner(a), inner(b)
	if len(ma) == 0 {
		t.Fatal("no blocks")
	}
	for k, v := range ma {
		if mb[k] != v {
			t.Fatalf("block %v changed while panning", k)
		}
	}
}

func TestHit(t *testing.T) {
	l := NewLane([]Event{{Start: 10, End: 20}, {Start: 30, End: 31}, {Start: 100, End: 200}})
	cases := []struct {
		t, tol int64
		want   int
	}{
		{15, 0, 0},
		{21, 0, -1},
		{22, 3, 0},
		{29, 3, 1},
		{50, 3, -1},
		{150, 0, 2},
		{5, 5, 0},
		{300, 1000, 2},
	}
	for _, c := range cases {
		if got := l.Hit(c.t, c.tol); got != c.want {
			t.Errorf("Hit(%d, %d) = %d, want %d", c.t, c.tol, got, c.want)
		}
	}
}

func TestPackNesting(t *testing.T) {
	ev := []Event{
		{Start: 0, End: 100, Name: 0},
		{Start: 10, End: 50, Name: 1},
		{Start: 20, End: 30, Name: 2},
		{Start: 60, End: 90, Name: 1},
		{Start: 60, End: 70, Name: 2}, // same start as its parent: longer goes first
		{Start: 200, End: 300, Name: 0},
	}
	lanes := Pack(ev)
	if len(lanes) != 3 {
		t.Fatalf("%d lanes, want 3", len(lanes))
	}
	for d, l := range lanes {
		for _, e := range l.Events {
			if int(e.Name) != d {
				t.Errorf("event %+v at depth %d", e, d)
			}
		}
	}
}

// sortEvents orders as a stable comparison sort by (start, longer first).
func TestSortEvents(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for _, n := range []int{0, 1, 2, 10, 1000, 100_000} {
		ev := make([]Event, n)
		for i := range ev {
			s := rng.Int64N(1 << uint(1+rng.IntN(40)))
			ev[i] = Event{Start: s, End: s + rng.Int64N(1<<uint(1+rng.IntN(30))), Name: int32(i)}
			if i > 0 && rng.IntN(5) == 0 {
				ev[i].Start, ev[i].End = ev[i-1].Start, ev[i-1].End // ties
			}
		}
		want := slices.Clone(ev)
		slices.SortStableFunc(want, func(x, y Event) int {
			return cmp.Or(cmp.Compare(x.Start, y.Start), cmp.Compare(y.End, x.End))
		})
		if got := sortEvents(ev); !slices.Equal(got, want) {
			t.Fatalf("n=%d: sortEvents differs from a stable sort", n)
		}
	}
}

// TopByTotal's heap selects what sorting everything would.
func TestTopByTotalMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	b := NewBuilder("x")
	names := make([]int32, 300)
	for i := range names {
		names[i] = b.Intern(string(rune('A'+i%26)) + itoa(int64(i)))
	}
	for i := range 20000 {
		s := int64(i) * 1000
		b.AddKernel(Event{Start: s, End: s + rng.Int64N(900)/100*100, Name: names[rng.IntN(len(names))]}, KernelInfo{Stream: int32(i % 3)})
	}
	tr := b.Finish()
	all := tr.TopByTotal(0, KindKernel)
	for _, n := range []int{1, 5, 50, 299, 300, 1000} {
		top := tr.TopByTotal(n, KindKernel)
		if want := all[:min(n, len(all))]; !slices.Equal(top, want) {
			t.Fatalf("top %d differs from the full ranking", n)
		}
	}
}

// TopInRange sums what clipping every event to the range by brute force
// would: overlap durations, counting the events overlapping the range.
func TestTopInRangeMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	b := NewBuilder("x")
	names := make([]int32, 40)
	for i := range names {
		names[i] = b.Intern("k" + itoa(int64(i)))
	}
	var evs []Event
	for i := range 5000 {
		s := int64(i) * 1000
		e := Event{Start: s, End: s + rng.Int64N(10)*100, Name: names[rng.IntN(len(names))]} // some instants
		b.AddKernel(e, KernelInfo{Stream: int32(i % 4)})
		evs = append(evs, e)
	}
	tr := b.Finish()
	type acc struct {
		count           int
		total, min, max int64
	}
	for _, r := range [][2]int64{{0, 5_000_000}, {1234, 98765}, {2000, 2001}, {150, 450}, {700_000, 700_000}, {3000, 3900}, {-50, 10}} {
		got := tr.TopInRange(0, r[0], r[1], KindKernel)
		want := map[string]acc{}
		var all int64
		for _, e := range evs {
			s, en := max(e.Start, r[0]), min(e.End, r[1])
			in := en > s || (e.Start == e.End && e.Start >= r[0] && e.Start < r[1])
			if !in || r[1] <= r[0] {
				continue
			}
			d := max(en-s, 0)
			a := want[tr.Names[e.Name]]
			if a.count == 0 || d < a.min {
				a.min = d
			}
			a.max = max(a.max, d)
			a.count++
			a.total += d
			want[tr.Names[e.Name]] = a
			all += d
		}
		if len(got) != len(want) {
			t.Fatalf("range %v: %d names, want %d", r, len(got), len(want))
		}
		for _, g := range got {
			w := want[g.Name]
			if g.Count != w.count || g.Total != w.total || g.Min != w.min || g.Max != w.max {
				t.Fatalf("range %v, %s: got %+v, want %+v", r, g.Name, g, w)
			}
			if all > 0 && math.Abs(g.Share-float64(w.total)/float64(all)) > 1e-12 {
				t.Fatalf("range %v, %s: share %v", r, g.Name, g.Share)
			}
		}
	}
	// The whole trace as a range is TopByTotal.
	if !slices.Equal(tr.TopInRange(10, math.MinInt64/2, math.MaxInt64/2, KindKernel), tr.TopByTotal(10, KindKernel)) {
		t.Error("a range covering the trace differs from TopByTotal")
	}
}
