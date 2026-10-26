package viewer

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

func countKind(s *App, k rowKind) int {
	n := 0
	for _, r := range s.tl.rows {
		if r.kind == k {
			n++
		}
	}
	return n
}

// More than theme.CompactAuto traces open compact: a strip and summary
// lanes per trace on the shared axis, names in the gutter; C toggles it
// and the choice is kept; the lane filter applies to every trace; events
// in summary lanes hit and select.
func TestCompactView(t *testing.T) {
	dir := jobDir(t, theme.CompactAuto+2)
	s := New(nil)
	s.SetAnimations(false)
	var kept Settings
	s.OnSettings(func(st Settings) { kept = st })
	tt := ui.NewTester(s.View, w, h)
	s.OpenPath(dir)
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	tt.Frame()
	n := theme.CompactAuto + 2
	if !s.compactOn() || countKind(s, rowStrip) != n || countKind(s, rowGroup) != 0 {
		t.Fatalf("compact %v: %d strips, %d groups", s.compactOn(), countKind(s, rowStrip), countKind(s, rowGroup))
	}
	// The fixture's traces have GPU and CUDA API rows; the default lanes
	// are GPU and NVTX: one lane each, as tall as the full view's.
	if got := countKind(s, rowSummary); got != n {
		t.Errorf("%d summary lanes, want %d", got, n)
	}
	for _, r := range s.tl.rows {
		if r.kind == rowSummary && r.h != laneH {
			t.Fatalf("a summary lane %v high, want the full view's %v", r.h, laneH)
		}
	}
	if len(s.tl.ranks) != n || s.tl.ranks[1].label != "rank1" || s.tl.ranks[1].host != "node0" || s.perf.blocks == 0 {
		t.Errorf("ranks %+v, %d blocks", s.tl.ranks, s.perf.blocks)
	}
	for _, rk := range s.tl.ranks {
		if rk.idle < 0 || rk.idle > 1 {
			t.Errorf("%s idle %v", rk.label, rk.idle)
		}
	}
	// Ranks line up: each summary lane's first block starts at its rank's
	// offset on the shared axis (rank r captured r ms later).
	ss := s.src.(*sessionSource)
	s.fit(false)
	tt.Frame()
	for _, r := range s.tl.rows {
		if r.kind != rowSummary || r.key != "gpu" {
			continue
		}
		bl := s.src.lod(r.members[0], 0, -1e9, 1e12, 1, nil)
		p := ss.parts[r.rank]
		if want := p.offset + ss.parts[r.rank].tr.Tracks[r.members[0]-p.first].Lanes[0].Events[0].Start - p.tr.Start; len(bl) == 0 || bl[0].start != want {
			t.Errorf("rank %d's first kernel at %v, want %d", r.rank, bl, want)
		}
	}

	// An event of a summary lane hits, selects, and has a box there.
	var hitOne bool
	for ri, r := range s.tl.rows {
		if r.kind != rowSummary {
			continue
		}
		y := rulerH + r.y - s.tl.scrollY + r.h/2
		for x := float32(s.labelW() + 5); x < s.tl.size.W && !hitOne; x += 3 {
			if h := s.hitAt(x, y); h.ok && h.row == ri {
				s.selectRef(h.ref)
				if _, ok := s.eventBox(h.ref); !ok {
					t.Error("the selected event has no box in its summary lane")
				}
				hitOne = true
			}
		}
		break
	}
	if !hitOne {
		t.Error("nothing hit in a summary lane")
	}

	// C: the full view, kept; C again: compact.
	tt.Click("Timeline")
	tt.Key(0, ui.KeyC)
	tt.Frame()
	if s.compactOn() || kept.Compact != "off" || countKind(s, rowGroup) == 0 {
		t.Errorf("C did not switch to the full view: %v %q", s.compactOn(), kept.Compact)
	}
	tt.Key(0, ui.KeyC)
	tt.Frame()
	if !s.compactOn() || kept.Compact != "on" {
		t.Error("C did not switch back")
	}
	// The lane filter: the GPU lane only, for every trace.
	if !tt.HasText("NVTX") || !tt.HasText("Overview") {
		t.Error("no density control or lane filter in the sidebar")
	}
	s.setLane("api", true)
	tt.Frame()
	if countKind(s, rowSummary) != 2*n || !slices.Equal(kept.CompactLanes, []string{"gpu", "nvtx", "api"}) {
		t.Errorf("with CUDA API: %d lanes, kept %q", countKind(s, rowSummary), kept.CompactLanes)
	}
	s.setLane("api", false)
	s.setLane("gpu", false) // the last lane stays
	tt.Frame()
	// NVTX only: these traces have none; each keeps its strip.
	if countKind(s, rowSummary) != 0 || countKind(s, rowStrip) != n || !slices.Equal(kept.CompactLanes, []string{"nvtx"}) {
		t.Errorf("filtered: %d lanes, %d strips, kept %q", countKind(s, rowSummary), countKind(s, rowStrip), kept.CompactLanes)
	}
	s.setLane("nvtx", false) // the last lane stays
	if !slices.Equal(kept.CompactLanes, []string{"gpu"}) {
		t.Errorf("no lane left: kept %q", kept.CompactLanes)
	}
	s.setLane("nvtx", true)
	// Auto, with two traces: the full view.
	s.SetCompact("auto")
	s.OpenPath(dir + "/rank0.sqlite " + dir + "/rank1.sqlite")
	waitFor(t, tt, "two traces", func() bool { ss, ok := s.src.(*sessionSource); return ok && len(ss.parts) == 2 && s.loading == "" })
	tt.Frame()
	if s.compactOn() {
		t.Error("two traces opened compact")
	}
}

// parallelTrace has three streams, two of them running side by side and a
// third in their gaps, NVTX ranges five deep and another thread's beside
// them.
func parallelTrace() *trace.Trace {
	b := trace.NewBuilder("parallel.sqlite")
	k, c, m := b.Intern("gemm"), b.Intern("allreduce"), b.Intern("copy")
	for i := range int64(200) {
		t := 1_000_000 + i*1_000_000
		b.AddKernel(trace.Event{Start: t, End: t + 500_000, Name: k}, trace.KernelInfo{Stream: 7})
		b.AddKernel(trace.Event{Start: t + 100_000, End: t + 450_000, Name: c}, trace.KernelInfo{Stream: 13})
		b.AddKernel(trace.Event{Start: t + 600_000, End: t + 900_000, Name: m}, trace.KernelInfo{Stream: 14})
	}
	for d := range int64(5) {
		b.AddNVTX(1<<56|7<<24|8, trace.Event{Start: 1_000_000 + d*1000, End: 201_000_000 - d*1000, Name: k})
	}
	// A second thread's range, alongside: a band of its own.
	b.AddNVTX(1<<56|7<<24|9, trace.Event{Start: 50_000_000, End: 150_000_000, Name: m})
	return b.Finish()
}

// The overview stacks what runs in parallel: kernels of streams that
// overlap in sub-lanes of their own, those that do not sharing one, NVTX
// ranges by depth (up to trace.OverviewDepth); a hit in a sub-lane is of the
// lanes drawn there, and its box is there. Overlaid: one lane a kind, kept.
func TestOverviewStacksParallelWork(t *testing.T) {
	s := New(parallelTrace())
	s.SetAnimations(false)
	var kept Settings
	s.OnSettings(func(st Settings) { kept = st })
	s.SetCompact("on")
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	subs := map[string]int{}
	for _, r := range s.tl.rows {
		if r.kind == rowSummary {
			subs[r.key]++
		}
	}
	if subs["gpu"] != 2 || subs["nvtx"] != trace.OverviewDepth[trace.TrackNVTX]+1 {
		t.Fatalf("sub-lanes %v, want gpu 2, nvtx %d", subs, trace.OverviewDepth[trace.TrackNVTX]+1)
	}
	// The second thread's band starts below the first's, marked.
	var tops []int
	for _, r := range s.tl.rows {
		if r.top {
			tops = append(tops, r.sub)
		}
	}
	if !slices.Equal(tops, []int{trace.OverviewDepth[trace.TrackNVTX]}) {
		t.Errorf("bands start at sub-lanes %v, want [%d]", tops, trace.OverviewDepth[trace.TrackNVTX])
	}
	if !tt.HasText("Stacked") || !tt.HasText("Parallel work") {
		t.Error("no Stacked/Overlaid control")
	}
	// Every gpu sub-lane hits, and only lanes drawn there.
	for ri, r := range s.tl.rows {
		if r.kind != rowSummary {
			continue
		}
		y := rulerH + r.y - s.tl.scrollY + r.h/2
		var got bool
		for x := float32(s.labelW() + 5); x < s.tl.size.W && !got; x += 2 {
			h := s.hitAt(x, y)
			if !h.ok || h.row != ri {
				continue
			}
			got = true
			if sl := s.slotOf(int(h.ref.Track), int(h.ref.Lane)); sl != r.slot {
				t.Errorf("row %d (%s/%d): hit a lane of sub-lane %d", ri, r.key, r.sub, sl)
			}
			s.selectRef(h.ref)
			if box, ok := s.eventBox(h.ref); !ok || box.Y != s.tl.size.Y+rulerH+r.y-s.tl.scrollY+1 {
				t.Errorf("row %d: the selected event's box %v not in its sub-lane", ri, box)
			}
		}
		if !got {
			t.Errorf("nothing hit in %s sub-lane %d", r.key, r.sub)
		}
	}
	s.setOverlaid(true)
	tt.Frame()
	if n := countKind(s, rowSummary); n != 2 || !kept.CompactOverlaid {
		t.Errorf("overlaid: %d summary rows, kept %v", n, kept.CompactOverlaid)
	}
}
