package viewer

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/trace"
)

const w, h = 1400, 900

// testTrace has a GPU stream of kernels launched from one thread, and that
// thread's nested NVTX ranges.
func testTrace() *trace.Trace {
	b := trace.NewBuilder("test.sqlite")
	gtid := int64(1<<56 | 7<<24 | 8)
	gemm, relu := b.Intern("gemm"), b.Intern("relu")
	launch := b.Intern("cudaLaunchKernel")
	step, fwd := b.Intern("step"), b.Intern("forward")
	t := int64(1_000_000)
	for i := range 2000 {
		name := gemm
		if i%3 == 0 {
			name = relu
		}
		corr := int32(i + 1)
		b.AddRuntime(gtid, trace.Event{Start: t, End: t + 4_000, Name: launch, Corr: corr})
		b.AddKernel(trace.Event{Start: t + 10_000, End: t + 10_000 + 30_000, Name: name, Corr: corr}, trace.KernelInfo{Stream: 7, GridX: 4, BlockX: 128})
		t += 50_000
	}
	for s := int64(0); s < 10; s++ {
		start := 1_000_000 + s*10_000_000
		b.AddNVTX(gtid, trace.Event{Start: start, End: start + 9_000_000, Name: step})
		b.AddNVTX(gtid, trace.Event{Start: start + 100_000, End: start + 4_000_000, Name: fwd})
	}
	return b.Finish()
}

func newTester(t *testing.T) (*App, *ui.Tester) {
	t.Helper()
	s := New(testTrace())
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	if s.tl.size.W == 0 {
		t.Fatal("the timeline was not painted")
	}
	return s, tt
}

func TestFitsWholeTraceAtFirst(t *testing.T) {
	s, _ := newTester(t)
	span := s.tl.cur.scale * s.dataW()
	if d := float64(s.tr.Duration()); span < d || span > d*1.1 {
		t.Errorf("view spans %.0f ns, trace %.0f", span, d)
	}
}

func TestCtrlWheelZoomsAroundPointer(t *testing.T) {
	s, tt := newTester(t)
	r := s.tl.size
	x, y := r.X+s.labelW()+400, r.Y+rulerH+40
	before := s.timeAtX(x - r.X)
	scale := s.tl.cur.scale
	tt.HoldModifiers(ui.Ctrl)
	tt.Scroll(x, y, 0, -50) // a notch up zooms in
	tt.HoldModifiers(0)
	if s.tl.cur.scale >= scale {
		t.Fatalf("scale %.1f after zooming in, was %.1f", s.tl.cur.scale, scale)
	}
	if want := scale / zoomPerNotch; math.Abs(s.tl.cur.scale-want) > want*1e-6 {
		t.Errorf("scale %.3f, want %.3f", s.tl.cur.scale, want)
	}
	if after := s.timeAtX(x - r.X); math.Abs(after-before) > s.tl.cur.scale {
		t.Errorf("the time under the pointer moved from %.0f to %.0f", before, after)
	}
}

func TestSmoothZoomSettles(t *testing.T) {
	s := New(testTrace())
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	r := s.tl.size
	scale := s.tl.cur.scale
	tt.HoldModifiers(ui.Ctrl)
	tt.Scroll(r.X+s.labelW()+300, r.Y+100, 0, -150)
	tt.HoldModifiers(0)
	target := s.tl.tgt.scale
	if !s.tl.animating || s.tl.cur.scale == target {
		t.Fatalf("zoom did not animate: cur %.2f target %.2f", s.tl.cur.scale, target)
	}
	tt.Advance(time.Second)
	if s.tl.animating || s.tl.cur.scale != target || target >= scale {
		t.Errorf("after a second: animating %v, scale %.2f, target %.2f", s.tl.animating, s.tl.cur.scale, target)
	}
	if tt.FramePending() {
		t.Error("the timeline keeps asking for frames once settled")
	}
}

func TestDragPans(t *testing.T) {
	s, tt := newTester(t)
	s.ZoomTo(0, 10_000_000)
	tt.Frame()
	r := s.tl.size
	start := s.tl.cur.start
	x, y := r.X+s.labelW()+500, r.Y+rulerH+200
	tt.Press(x, y)
	tt.Move(x-100, y)
	tt.Move(x-200, y)
	tt.Release(x-200, y)
	if got, want := s.tl.cur.start-start, 200*s.tl.cur.scale; math.Abs(got-want) > s.tl.cur.scale {
		t.Errorf("dragging 200px left moved the view by %.0f ns, want %.0f", got, want)
	}
	if s.selected != nil {
		t.Error("a drag selected an event")
	}
}

func TestClickSelectsAndShowsDetails(t *testing.T) {
	s, tt := newTester(t)
	s.ZoomTo(0, 2_000_000)
	tt.Frame()
	// The first kernel.
	var ref trace.Ref
	found := false
	for i, tk := range s.tr.Tracks {
		if tk.Kind == trace.TrackStream {
			ref, found = trace.Ref{Track: int32(i)}, true
		}
	}
	if !found {
		t.Fatal("no stream track")
	}
	x, y, ok := s.EventCenter(ref)
	if !ok {
		t.Fatal("the first kernel is not in view")
	}
	tt.ClickAt(x, y)
	if s.selected == nil || *s.selected != ref {
		t.Fatalf("selected %v, want %v", s.selected, ref)
	}
	for _, want := range []string{"Grid", "4 × 0 × 0", "Launched by", "cudaLaunchKernel"} {
		if !tt.HasText(want) {
			t.Errorf("details do not show %q: %q", want, tt.Texts())
		}
	}
	if tt.HasText("Demangled name") {
		t.Error("details show a demangled name the kernel does not have")
	}
	// Escape clears it.
	tt.Key(0, ui.KeyEscape)
	if s.selected != nil {
		t.Error("Escape did not clear the selection")
	}
}

func TestKeysZoomAndFit(t *testing.T) {
	s, tt := newTester(t)
	scale := s.tl.cur.scale
	tt.Key(0, ui.KeyEqual)
	if s.tl.cur.scale >= scale {
		t.Errorf("= did not zoom in")
	}
	tt.KeyDown(ui.KeyD)
	tt.Advance(200 * time.Millisecond)
	tt.KeyUp(ui.KeyD)
	tt.Key(0, ui.Key0)
	if math.Abs(s.tl.cur.scale-scale) > scale*1e-9 {
		t.Errorf("0 did not fit the trace again: %.3f vs %.3f", s.tl.cur.scale, scale)
	}
}

func TestCollapseGroup(t *testing.T) {
	s, tt := newTester(t)
	rows := len(s.tl.rows)
	r := s.tl.size
	tt.ClickAt(r.X+100, r.Y+rulerH+groupH/2) // the first group's heading
	if len(s.tl.rows) >= rows {
		t.Fatalf("collapsing left %d rows of %d", len(s.tl.rows), rows)
	}
	tt.Advance(time.Second) // not a double click
	tt.ClickAt(r.X+100, r.Y+rulerH+groupH/2)
	if len(s.tl.rows) != rows {
		t.Errorf("expanding gave %d rows, want %d", len(s.tl.rows), rows)
	}
}

func TestSummaryHighlights(t *testing.T) {
	s, tt := newTester(t)
	if err := tt.Click("Top kernels"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("gemm"); err != nil {
		t.Fatal(err)
	}
	if s.highlight != "gemm" {
		t.Errorf("highlight %q", s.highlight)
	}
}

func TestRenderPNG(t *testing.T) {
	s := New(testTrace())
	s.SetAnimations(false)
	img := ui.Render(s.View, 800, 500, 1)
	if img.Bounds().Dx() != 800 || img.Bounds().Dy() != 500 {
		t.Fatalf("image %v", img.Bounds())
	}
	// The kernels' colors make the image anything but uniform.
	colors := map[[3]uint8]bool{}
	for y := 0; y < 500; y += 7 {
		for x := 0; x < 800; x += 7 {
			c := img.RGBAAt(x, y)
			colors[[3]uint8{c.R, c.G, c.B}] = true
		}
	}
	if len(colors) < 20 {
		t.Errorf("only %d colors in the render", len(colors))
	}
}

func TestFormat(t *testing.T) {
	cases := map[float64]string{12: "12 ns", 1500: "1.5 µs", 2_345_678: "2.346 ms", 12_345_678_900: "12.35 s"}
	for ns, want := range cases {
		if got := fmtDur(ns); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", ns, got, want)
		}
	}
	if got := tickStep(1000, 100); got != 100_000 {
		t.Errorf("tickStep = %v", got)
	}
	if got := fmtTick(1_500_000, 500_000); got != "1.5 ms" {
		t.Errorf("fmtTick = %q", got)
	}
}

// bigTrace has n kernels on each of 8 streams and n API calls on 4 threads.
func bigTrace(n int) *trace.Trace {
	b := trace.NewBuilder("big.sqlite")
	names := []int32{b.Intern("a"), b.Intern("b"), b.Intern("c")}
	for s := range 8 {
		t := int64(0)
		for i := range n {
			b.AddKernel(trace.Event{Start: t, End: t + 700 + int64(i%5)*100, Name: names[i%3]}, trace.KernelInfo{Stream: int32(s)})
			t += 1500
		}
	}
	for th := range 4 {
		t := int64(0)
		for i := range n {
			b.AddRuntime(int64(th+1), trace.Event{Start: t, End: t + 300, Name: names[i%3]})
			t += 3000
		}
	}
	return b.Finish()
}

// A frame of the whole of a 1.2M-event trace, and one zoomed into it.
func BenchmarkFrame(b *testing.B) {
	for _, zoom := range []bool{false, true} {
		name := "fit"
		if zoom {
			name = "zoomed"
		}
		b.Run(name, func(b *testing.B) {
			s := New(bigTrace(100_000))
			s.SetAnimations(false)
			tt := ui.NewTester(s.View, 1600, 1000)
			tt.Frame()
			if zoom {
				s.ZoomTo(50_000_000, 50_300_000)
			}
			for b.Loop() {
				s.tl.cur.start += 1 // something changed: paint again
				tt.Frame()
			}
		})
	}
}

func TestWelcomeLoadingAndError(t *testing.T) {
	s := New(nil)
	browsed := 0
	s.SetBrowse(func() { browsed++ })
	tt := ui.NewTester(s.View, w, h)
	if err := tt.Click("Open trace…"); err != nil {
		t.Fatal(err)
	}
	if browsed != 1 {
		t.Errorf("Open trace… browsed %d times", browsed)
	}
	s.SetLoading("/tmp/x.sqlite", nil)
	tt.Frame()
	if !tt.HasText("Loading x.sqlite…") {
		t.Errorf("no loading message: %q", tt.Texts())
	}
	s.SetLoading("/tmp/x.sqlite", errTest("not a SQLite database"))
	tt.Frame()
	if !tt.HasText("Could not open x.sqlite") || !tt.HasText("not a SQLite database") {
		t.Errorf("no error message: %q", tt.Texts())
	}
	s.SetTrace(testTrace())
	tt.Frame()
	if !tt.HasText("Top kernels") || s.ctl.browse == nil {
		t.Errorf("the trace does not show, or Browse was lost: %q", tt.Texts())
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

// measure Shift+drags a range on the timeline, from x0 to x1 (canvas
// pixels), and returns it.
func measure(s *App, tt *ui.Tester, x0, x1 float32) (float64, float64) {
	y := float32(rulerH + 60)
	s.input(ui.InputEvent{Kind: ui.InputPointerDown, X: x0, Y: y, Mods: ui.Shift})
	for x := x0; x <= x1; x += 20 {
		s.input(ui.InputEvent{Kind: ui.InputPointerMove, X: x, Y: y, Mods: ui.Shift})
	}
	s.input(ui.InputEvent{Kind: ui.InputPointerUp, X: x1, Y: y, Mods: ui.Shift})
	tt.Frame()
	return min(s.tl.rangeA, s.tl.rangeB), max(s.tl.rangeA, s.tl.rangeB)
}

// A measured range: the summaries are of it, by overlap; Esc, its × in the
// ruler and a click on an empty part of the timeline clear it; Zoom to
// range, Enter and Z zoom to it.
func TestMeasuredRange(t *testing.T) {
	s, tt := newTester(t)
	a, b := measure(s, tt, s.labelW()+200, s.labelW()+500)
	if !s.tl.hasRange || !s.sumRange.ok || b <= a || !tt.HasText("Zoom to range") {
		t.Fatalf("no range measured: %v %v %v", s.tl.hasRange, a, b)
	}
	if err := tt.Click("Top kernels"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("In range") {
		t.Error("the summary does not say it is of the range")
	}
	got, _ := s.stats(trace.KindKernel)
	want := s.tr.TopInRange(100, s.tr.Start+int64(math.Floor(a)), s.tr.Start+int64(math.Ceil(b)), trace.KindKernel)
	if len(got) == 0 || !slices.Equal(got, want) {
		t.Errorf("summary of the range: %d names, want %d", len(got), len(want))
	}
	var inRange, whole int
	for _, st := range got {
		inRange += st.Count
	}
	for _, st := range s.tr.TopByTotal(100, trace.KindKernel) {
		whole += st.Count
	}
	if inRange == 0 || inRange >= whole {
		t.Errorf("%d kernels in the range, %d in all", inRange, whole)
	}

	// Esc, the focus on the panel's tab.
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if s.tl.hasRange || s.sumRange.ok || tt.HasText("Zoom to range") || !tt.HasText("Whole trace · Shift+drag on the timeline to sum a range") {
		t.Error("Esc did not clear the range")
	}

	// The × in the ruler.
	measure(s, tt, s.labelW()+200, s.labelW()+500)
	x := s.tl.rangeClose
	if x.W == 0 {
		t.Fatal("no × painted")
	}
	tt.ClickAt(x.X+x.W/2, x.Y+x.H/2)
	tt.Frame()
	if s.tl.hasRange {
		t.Error("the × did not clear the range")
	}

	// A plain click where there is no event.
	measure(s, tt, s.labelW()+200, s.labelW()+500)
	var ex, ey float32 = -1, -1
	for y := float32(rulerH + 10); y < s.tl.size.H && ex < 0; y += 5 {
		for x := float32(s.labelW() + 20); x < s.tl.size.W; x += 30 {
			if h := s.hitAt(x, y); !h.ok && !h.ruler && !h.gutter && !h.group && !h.idleToggle && y < s.tl.contentH-s.tl.scrollY+rulerH {
				ex, ey = x, y
				break
			}
		}
	}
	if ex < 0 {
		t.Fatal("no empty spot on the timeline")
	}
	tt.ClickAt(s.tl.size.X+ex, s.tl.size.Y+ey)
	tt.Frame()
	if s.tl.hasRange {
		t.Error("a click on an empty spot did not clear the range")
	}

	// Zoom to range, Enter and Z.
	for _, how := range []string{"button", "Enter", "Z"} {
		s.fit(false)
		tt.Frame()
		a, b := measure(s, tt, s.labelW()+200, s.labelW()+500)
		switch how {
		case "button":
			if err := tt.Click("Zoom to range"); err != nil {
				t.Fatal(err)
			}
		case "Enter":
			tt.Key(0, ui.KeyEnter)
		case "Z":
			tt.Key(0, ui.KeyZ)
		}
		tt.Frame()
		v0, v1 := s.tl.cur.start, s.tl.cur.start+s.tl.cur.scale*s.dataW()
		if v0 > a || v1 < b || v1-v0 > 1.1*(b-a) {
			t.Errorf("%s: view %.0f–%.0f, range %.0f–%.0f", how, v0, v1, a, b)
		}
	}
}
