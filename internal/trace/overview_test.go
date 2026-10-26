package trace

import (
	"slices"
	"testing"
)

// parallelTrace has four streams: a compute stream, a communication stream
// that runs alongside it, a copy stream that runs while neither does, and
// a fourth that overlaps all; and one thread's NVTX ranges, five deep.
func parallelTrace() *Trace {
	b := NewBuilder("parallel.sqlite")
	k := b.Intern("k")
	for i := range int64(100) {
		t := i * 1_000_000
		b.AddKernel(Event{Start: t, End: t + 500_000, Name: k}, KernelInfo{Stream: 1})
		b.AddKernel(Event{Start: t + 100_000, End: t + 450_000, Name: k}, KernelInfo{Stream: 2})
		b.AddKernel(Event{Start: t + 600_000, End: t + 900_000, Name: k}, KernelInfo{Stream: 3})
		b.AddKernel(Event{Start: t, End: t + 990_000, Name: k}, KernelInfo{Stream: 4})
	}
	for d := range int64(5) {
		b.AddNVTX(9, Event{Start: d * 1000, End: 100_000_000 - d*1000, Name: k})
	}
	return b.Finish()
}

func TestOverviewSlots(t *testing.T) {
	tr := parallelTrace()
	tr.Overview()
	slot := map[int]int{} // by stream row's busy rank
	var streams []*Track
	for _, tk := range tr.Tracks {
		switch tk.Kind {
		case TrackStream:
			if len(tk.Overview) != len(tk.Lanes) {
				t.Fatalf("stream %s: overview %v of %d lanes", tk.Name, tk.Overview, len(tk.Lanes))
			}
			streams = append(streams, tk)
		case TrackNVTX:
			// The outermost OverviewDepth lanes, in order.
			if want := []int{0, 1, 2}; !slices.Equal(tk.Overview, want) {
				t.Errorf("NVTX of %d lanes: overview %v, want %v", len(tk.Lanes), tk.Overview, want)
			}
		}
	}
	if len(streams) != 4 {
		t.Fatalf("%d streams", len(streams))
	}
	for i, tk := range streams {
		slot[i] = tk.Overview[0]
	}
	// Stream 4 (the busiest) first; 1 alongside it; 2 overlaps both; 3
	// overlaps only 4, so it shares 1's sub-lane.
	if slot[3] != 0 || slot[0] != 1 || slot[2] != 1 || slot[1] != 2 {
		t.Errorf("sub-lanes by stream %v, want 4→0, 1→1, 3→1, 2→2", slot)
	}
	// Under a cap of 1, everything shares one.
	old := OverviewCap[TrackStream]
	OverviewCap[TrackStream] = 1
	defer func() { OverviewCap[TrackStream] = old }()
	if s := tr.OverviewSlots()[TrackStream]; len(s) != 1 || len(s[0]) != 4 {
		t.Errorf("cap 1: %v", s)
	}
}

func TestOverlap(t *testing.T) {
	a := []Event{{Start: 0, End: 10}, {Start: 20, End: 30}}
	b := []Event{{Start: 5, End: 25}, {Start: 28, End: 40}}
	if got := overlap(a, b); got != 5+5+2 {
		t.Errorf("overlap %d, want 12", got)
	}
	if overlap(a, nil) != 0 {
		t.Error("overlap with nothing")
	}
}

// Nested ranges go as a band a thread, depths in order: two threads whose
// ranges overlap get bands of their own (never one's ranges among the
// other's, as 0.3.7 had them), and a thread that overlaps neither shares
// a sub-lane where it fits; nothing drawn overlaps.
func TestOverviewBands(t *testing.T) {
	b := NewBuilder("bands.sqlite")
	k := b.Intern("r")
	for i := range int64(50) {
		t0 := i * 1_000_000
		// Thread 1: three deep, busy; thread 2: two deep, the first
		// shorter than thread 1's second (so lane by lane they would share).
		b.AddNVTX(1, Event{Start: t0, End: t0 + 900_000, Name: k})
		b.AddNVTX(1, Event{Start: t0 + 10_000, End: t0 + 800_000, Name: k})
		b.AddNVTX(1, Event{Start: t0 + 20_000, End: t0 + 100_000, Name: k})
		b.AddNVTX(2, Event{Start: t0 + 200_000, End: t0 + 300_000, Name: k})
		b.AddNVTX(2, Event{Start: t0 + 210_000, End: t0 + 290_000, Name: k})
	}
	// Thread 3: once, after the others.
	b.AddNVTX(3, Event{Start: 60_000_000, End: 61_000_000, Name: k})
	tr := b.Finish()
	tr.Overview()
	got := map[int64][]int{}
	for _, tk := range tr.Tracks {
		if tk.Kind == TrackNVTX {
			got[int64(len(got))+1] = tk.Overview
		}
	}
	if !slices.Equal(got[1], []int{0, 1, 2}) || !slices.Equal(got[2], []int{3, 4}) || !slices.Equal(got[3], []int{0}) {
		t.Errorf("bands %v, want thread 1 at 0-2, 2 at 3-4, 3 sharing 0", got)
	}
	slots := tr.OverviewSlots()[TrackNVTX]
	for si, s := range slots {
		for i := range s {
			for j := i + 1; j < len(s); j++ {
				if overlap(tr.Tracks[s[i].Row].Lanes[s[i].Lane].Events, tr.Tracks[s[j].Row].Lanes[s[j].Lane].Events) > 0 {
					t.Errorf("sub-lane %d: %v and %v overlap", si, s[i], s[j])
				}
			}
		}
	}
}
