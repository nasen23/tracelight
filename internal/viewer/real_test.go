package viewer

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/trace"
)

var (
	realOnce  sync.Once
	realTrace *trace.Trace
	realErr   error
)

// loadReal loads the real trace (an nsys SQLite export of an actual run of
// your own, named by TRACELIGHT_TRACE; none is in the repository), or
// skips.
func loadReal(t *testing.T) *trace.Trace {
	t.Helper()
	path := os.Getenv("TRACELIGHT_TRACE")
	if _, err := os.Stat(path); path == "" || err != nil {
		t.Skip("no real trace (set TRACELIGHT_TRACE to an nsys SQLite export)")
	}
	realOnce.Do(func() { realTrace, realErr = trace.LoadSQLite(path) })
	if realErr != nil {
		t.Fatal(realErr)
	}
	return realTrace
}

func countRows(s *App, kind rowKind, idle bool) int {
	n := 0
	for _, r := range s.tl.rows {
		if r.kind == kind && r.idle == idle {
			n++
		}
	}
	return n
}

// GPUs that did nothing are hidden at first, behind a row that says how
// many there are; it, and the sidebar's switch, show them.
func TestRealTraceIdleGPUsHidden(t *testing.T) {
	tr := loadReal(t)
	idle := tr.IdleDevices()
	if len(idle) == 0 {
		t.Skip("the trace has no idle GPUs")
	}
	s := New(tr)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	if n := countRows(s, rowGroup, true); n != 0 {
		t.Fatalf("%d idle GPUs shown at first", n)
	}
	if countRows(s, rowIdleToggle, false) != 1 {
		t.Fatal("no row to show the idle GPUs")
	}
	label := fmt.Sprintf("Show %d idle GPUs", len(idle))
	if len(idle) == 1 {
		label = "Show 1 idle GPU"
	}
	if !tt.HasText(label) {
		t.Errorf("the sidebar does not offer to %q", label)
	}
	// The toggle row on the timeline.
	var y float32
	for _, r := range s.tl.rows {
		if r.kind == rowIdleToggle {
			y = s.tl.size.Y + rulerH + r.y - s.tl.scrollY + r.h/2
		}
	}
	tt.ClickAt(s.tl.size.X+100, y)
	if n := countRows(s, rowGroup, true); n != len(idle) {
		t.Fatalf("%d idle GPUs shown after the toggle, want %d", n, len(idle))
	}
	if countRows(s, rowIdle, false) != len(idle) {
		t.Error("idle GPUs show no placeholder row")
	}
	// The sidebar's switch hides them again.
	s.setShowIdle(false)
	tt.Frame()
	if n := countRows(s, rowGroup, true); n != 0 {
		t.Fatalf("%d idle GPUs shown after hiding them", n)
	}
}

// A zoom from the whole trace down to microseconds and back, pointing at
// and clicking the timeline at each level, renders and selects.
func TestRealTraceZoomScript(t *testing.T) {
	tr := loadReal(t)
	s := New(tr)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, 1600, 1000)
	tt.Frame()
	r := s.tl.size
	// The first stream's first lane.
	x, y := r.X+s.labelW()+600, float32(-1)
	for _, row := range s.tl.rows {
		if row.kind == rowTrack && y < 0 {
			y = r.Y + rulerH + row.y + trackPad + laneH/2
		}
	}
	selected := 0
	for span := float64(tr.Duration()); span > 2000; span /= 4 {
		s.ZoomTo(float64(tr.Duration())/2-span/2, float64(tr.Duration())/2+span/2)
		tt.Move(x, y)
		tt.ClickAt(x, y)
		if s.selected != nil {
			selected++
		}
	}
	if selected == 0 {
		t.Error("no click selected anything on the first stream at any zoom")
	}
	img := tt.Image()
	if img.Bounds().Dx() != 1600 {
		t.Errorf("image %v", img.Bounds())
	}
}

func TestPerfOverlay(t *testing.T) {
	s, tt := newTester(t)
	tt.Key(0, ui.KeyP)
	if !s.showPerf {
		t.Fatal("P did not show the overlay")
	}
	tt.Frame()
	if _, p50, _, _ := s.perf.stats(); p50 <= 0 || s.perf.blocks == 0 {
		t.Errorf("the overlay has no times (p50 %v) or blocks (%d)", p50, s.perf.blocks)
	}
}
