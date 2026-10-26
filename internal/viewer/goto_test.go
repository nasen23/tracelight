package viewer

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// inView reports whether the event at ref, from start to end, is drawn
// inside the timeline's visible area.
func inView(s *App, ref trace.Ref, start, end int64) bool {
	b, ok := s.boxAt(ref, start, end)
	tl := &s.tl
	return ok && b.Y >= tl.size.Y+rulerH && b.Y+b.H <= tl.size.Y+tl.size.H && b.X+b.W >= tl.size.X+s.labelW() && b.X <= tl.size.X+tl.size.W
}

// A kernel's "Go to launch" sits up in the details, where it shows; it
// selects the CUDA API call and shows it, its row scrolled into view, in
// the full view and in the rank overview (its lane turned on); "Go to
// kernel" goes back. A flow line whose other end is out of view ends in a
// chip at the view's edge, which goes there.
func TestGoToLinked(t *testing.T) {
	dir := jobDir(t, 6)
	for _, mode := range []string{"off", "on"} {
		s := New(nil)
		s.SetAnimations(false)
		s.SetCompact(mode)
		tt := ui.NewTester(s.View, 1600, 1000)
		s.OpenLocal(dir)
		waitFor(t, tt, "the session", func() bool { ss := s.session(); return ss != nil && len(ss.parts) == 6 && s.loading == "" })
		tt.Frame()
		tried := 0
		for ti, tk := range s.tr.Tracks {
			if tk.Kind != trace.TrackStream || len(tk.Lanes) == 0 || tried >= 8 {
				continue
			}
			l := tk.Lanes[0]
			for i := 0; i < len(l.Events) && tried < 8; i += max(1, len(l.Events)/2) {
				s.tl.scrollY = 0
				s.fit(false)
				s.Select(trace.Ref{Track: int32(ti), Lane: 0, Index: int32(i)})
				tt.Frame()
				in, _ := s.selectedInfo()
				if in.linked == nil {
					continue
				}
				tried++
				if r, ok := tt.Find("Go to launch"); !ok || r.Y+r.H > 1000 || r.X+r.W > 1600 {
					t.Fatalf("%s: Go to launch is not in the window: %+v", mode, r)
				}
				if err := tt.Click("Go to launch"); err != nil {
					t.Fatal(err)
				}
				tt.Frame()
				to, _ := s.selectedInfo()
				if to.kind != trace.KindRuntime || !inView(s, to.ref, to.start, to.end) {
					t.Errorf("%s: after Go to launch, %v selected, in view: %v", mode, to.kind, inView(s, to.ref, to.start, to.end))
				}
				if !tt.HasText("Go to kernel") {
					t.Errorf("%s: the launch does not offer Go to kernel", mode)
				}
			}
		}
		if tried == 0 {
			t.Fatalf("%s: no kernel with its launch", mode)
		}
		// The chip: zoomed into the launch, the kernel is out of view.
		in, _ := s.selectedInfo()
		s.zoomTo(float64(in.start), float64(in.end), false)
		tt.Frame()
		c := s.tl.linkChip
		if c.W == 0 {
			t.Fatalf("%s: no chip where the flow line leaves the view", mode)
		}
		tt.ClickAt(c.X+c.W/2, c.Y+c.H/2)
		tt.Frame()
		if k, _ := s.selectedInfo(); k.kind == trace.KindRuntime || !inView(s, k.ref, k.start, k.end) {
			t.Errorf("%s: the chip went to %v", mode, k.kind)
		}
	}
}

// The label gutter's edge drags its width (kept in the settings); a
// double click there sets it back.
func TestGutterResize(t *testing.T) {
	s, tt := openSession(t, jobDir(t, 2), 2)
	var kept Settings
	s.OnSettings(func(v Settings) { kept = v })
	x0, y := s.tl.size.X+s.labelW(), s.tl.size.Y+s.tl.size.H/2
	tt.Press(x0-1, y)
	tt.Move(s.tl.size.X+400, y)
	tt.Release(s.tl.size.X+400, y)
	tt.Frame()
	if s.labelW() != 400 || kept.LabelW != 400 {
		t.Fatalf("dragged to 400: gutter %v, kept %v", s.labelW(), kept.LabelW)
	}
	if h := s.hitAt(380, y-s.tl.size.Y); !h.gutter && !h.group && h.row == 0 {
		t.Error("380 is not in the gutter")
	}
	tt.Press(s.tl.size.X+399, y)
	tt.Move(s.tl.size.X+5000, y)
	tt.Release(s.tl.size.X+5000, y)
	if w := s.labelW(); w != 640 {
		t.Errorf("dragged far: %v", w)
	}
	// A double click on the edge: the default again.
	tt.ClickAt(s.tl.size.X+639, y)
	tt.ClickAt(s.tl.size.X+639, y)
	tt.Frame()
	if w := s.labelW(); w != theme.LabelW || kept.LabelW != 0 {
		t.Errorf("double-clicked: gutter %v, kept %v", w, kept.LabelW)
	}
}
