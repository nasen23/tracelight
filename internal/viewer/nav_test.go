package viewer

import (
	"math"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
)

// navTester shows 10 ms of the 100 ms test trace (room to pan and zoom
// out), the pointer off the timeline.
func navTester(t *testing.T) (*App, *ui.Tester) {
	t.Helper()
	s, tt := newTester(t)
	s.ZoomTo(5_000_000, 15_000_000)
	tt.Frame()
	return s, tt
}

// pxPerSec is how many pixels a second panning moves at full speed.
func pxPerSec(s *App) float64 { return theme.NavPan * s.dataW() }

// Holding D pans at a constant rate a second, after a short ease in; letting
// go eases out and stops, and asks for no more frames.
func TestHeldKeyPansContinuously(t *testing.T) {
	s, tt := navTester(t)
	scale := s.tl.cur.scale
	start := s.tl.cur.start
	tt.KeyDown(ui.KeyD)
	tt.Advance(time.Second)
	first := (s.tl.cur.start - start) / scale
	if want := pxPerSec(s); first < 0.85*want || first > want {
		t.Errorf("first second panned %.0f px, want a little under %.0f (the ease in)", first, want)
	}
	mid := s.tl.cur.start
	tt.Advance(time.Second)
	second := (s.tl.cur.start - mid) / scale
	if want := pxPerSec(s); math.Abs(second-want) > 0.02*want {
		t.Errorf("second second panned %.0f px, want %.0f: not a constant rate", second, want)
	}
	if s.tl.cur.scale != scale {
		t.Error("panning zoomed")
	}
	tt.KeyUp(ui.KeyD)
	released := s.tl.cur.start
	tt.Advance(time.Second)
	after := (s.tl.cur.start - released) / scale
	// The ease out goes on for about NavEase seconds of full speed.
	if want := pxPerSec(s) * theme.NavEase; after <= 0 || after > 1.5*want {
		t.Errorf("after letting go it moved %.1f px, want about %.1f", after, want)
	}
	if s.nav.moving() || tt.FramePending() {
		t.Error("still moving, or asking for frames, after the keys went up")
	}
}

// The distance does not depend on the frame rate.
func TestNavFrameRateIndependent(t *testing.T) {
	run := func(fps int) float64 {
		s, tt := navTester(t)
		_ = tt
		s.nav.press(ui.KeyD)
		start := s.tl.cur.start
		s.stepNav(0) // the press starts it
		for range fps * 2 {
			s.stepNav(1 / float64(fps))
		}
		return (s.tl.cur.start - start) / s.tl.cur.scale
	}
	a, b := run(120), run(30)
	if math.Abs(a-b) > 0.02*a {
		t.Errorf("two seconds at 120 fps panned %.1f px, at 30 fps %.1f", a, b)
	}
}

// W zooms in around the pointer, S out; Shift is three times as fast.
func TestHeldKeyZoomsAroundPointer(t *testing.T) {
	s, tt := navTester(t)
	r := s.tl.size
	x, y := r.X+s.labelW()+300, r.Y+rulerH+60
	tt.Move(x, y)
	before := s.timeAtX(x - r.X)
	scale := s.tl.cur.scale
	tt.KeyDown(ui.KeyW)
	tt.Advance(time.Second)
	tt.KeyUp(ui.KeyW)
	tt.Advance(time.Second)
	ratio := scale / s.tl.cur.scale
	if want := math.Exp(theme.NavZoom); ratio < 0.8*want || ratio > 1.1*want {
		t.Errorf("a second of W zoomed %.2f×, want about %.2f×", ratio, want)
	}
	if after := s.timeAtX(x - r.X); math.Abs(after-before) > s.tl.cur.scale {
		t.Errorf("the time under the pointer moved from %.0f to %.0f", before, after)
	}

	// Shift: three times the rate.
	s2, tt2 := navTester(t)
	scale2 := s2.tl.cur.scale
	tt2.KeyDown(ui.KeyS)
	tt2.HoldModifiers(ui.Shift) // after the key: the Tester's KeyDown carries no modifiers
	tt2.Advance(200 * time.Millisecond)
	tt2.KeyUp(ui.KeyS)
	tt2.HoldModifiers(0)
	tt2.Advance(time.Second)
	if got, want := math.Log(s2.tl.cur.scale/scale2), theme.NavZoom*theme.NavShift*0.2; got < 0.8*want || got > 1.1*want {
		t.Errorf("0.2 s of Shift+S zoomed out e^%.2f, want about e^%.2f", got, want)
	}
}

// The navigation speed setting multiplies the rates; the control in the
// sidebar sets it and says so.
func TestNavSpeedSetting(t *testing.T) {
	pan := func(speed float64) float64 {
		s, tt := navTester(t)
		s.SetNavSpeed(speed)
		s.ZoomTo(0, 4_000_000) // room to pan fast
		tt.Frame()
		start, scale := s.tl.cur.start, s.tl.cur.scale
		tt.KeyDown(ui.KeyD)
		tt.Advance(300 * time.Millisecond)
		tt.KeyUp(ui.KeyD)
		tt.Advance(time.Second)
		return (s.tl.cur.start - start) / scale
	}
	normal := pan(1)
	for _, v := range theme.NavSpeeds {
		if got := pan(v.Times); math.Abs(got-v.Times*normal) > 0.03*v.Times*normal {
			t.Errorf("%s panned %.0f px, want %.0f (%g× Normal)", v.Name, got, v.Times*normal, v.Times)
		}
	}
	s, tt := newTester(t)
	var told float64
	s.OnNavSpeed = func(v float64) { told = v }
	if err := tt.Click("Very fast"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.NavSpeed() != 3 || told != 3 {
		t.Errorf("choosing Very fast: speed %g, told %g", s.NavSpeed(), told)
	}
	s.SetNavSpeed(1.7) // not a choice: Normal
	if s.NavSpeed() != 1 {
		t.Errorf("an unknown speed gave %g", s.NavSpeed())
	}
}

// Keys combine: D and W pan and zoom at once; A and D cancel.
func TestHeldKeysCombine(t *testing.T) {
	s, tt := navTester(t)
	start, scale := s.tl.cur.start, s.tl.cur.scale
	tt.KeyDown(ui.KeyD)
	tt.KeyDown(ui.KeyW)
	tt.Advance(500 * time.Millisecond)
	if s.tl.cur.scale >= scale || s.tl.cur.start <= start {
		t.Errorf("D+W: scale %.1f (was %.1f), start %.0f (was %.0f)", s.tl.cur.scale, scale, s.tl.cur.start, start)
	}
	tt.KeyUp(ui.KeyW)
	tt.KeyDown(ui.KeyA) // now A and D: no pan
	tt.Advance(time.Second)
	held := s.tl.cur.start
	tt.Advance(500 * time.Millisecond)
	if math.Abs(s.tl.cur.start-held) > s.tl.cur.scale {
		t.Errorf("A and D together still pan: %.0f px", (s.tl.cur.start-held)/s.tl.cur.scale)
	}
	tt.KeyUp(ui.KeyA)
	tt.KeyUp(ui.KeyD)
	tt.Advance(time.Second)
	if s.nav.moving() {
		t.Error("moving with no key held")
	}
}

// Losing the keyboard stops every motion at once: the key-up would never
// come.
func TestNavStopsOnBlur(t *testing.T) {
	s, tt := navTester(t)
	tt.KeyDown(ui.KeyD)
	tt.KeyDown(ui.KeyS)
	tt.Advance(300 * time.Millisecond)
	tt.SetFocused(false)
	tt.Frame()
	if s.nav.moving() {
		t.Fatal("still moving after the window lost the keyboard")
	}
	at, scale := s.tl.cur.start, s.tl.cur.scale
	tt.Advance(time.Second)
	if s.tl.cur.start != at || s.tl.cur.scale != scale {
		t.Error("the view moved after the window lost the keyboard")
	}
	tt.SetFocused(true)
}

// Auto-repeat does not press a key that is up, nor move anything itself.
func TestNavIgnoresRepeat(t *testing.T) {
	s, _ := navTester(t)
	if !s.input(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyD, Repeat: true}) {
		t.Error("a repeat of D was not taken")
	}
	if s.nav.moving() {
		t.Error("a repeat pressed D")
	}
}

// The appearance follows the system's, live, unless the user chose one.
func TestThemeFollowsSystem(t *testing.T) {
	s, tt := newTester(t)
	tt.SetDark(false)
	tt.Frame()
	if active != theme.Light || colBg != ui.Hex(theme.Light.Bg) {
		t.Fatal("a light system does not get the light palette")
	}
	tt.SetDark(true)
	tt.Frame()
	if active != theme.Dark {
		t.Fatal("the palette did not follow the system to dark")
	}
	var chosen theme.Mode
	s.OnThemeMode = func(m theme.Mode) { chosen = m }
	if err := tt.Click("Light"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if active != theme.Light || chosen != theme.ModeLight || s.ThemeMode() != theme.ModeLight {
		t.Errorf("choosing Light: palette %s, told %q", active.Name, chosen)
	}
	tt.SetDark(false)
	tt.SetDark(true) // the system changes: the choice stays
	tt.Frame()
	if active != theme.Light {
		t.Error("a chosen appearance followed the system")
	}
	if err := tt.Click("System"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if active != theme.Dark {
		t.Error("back to System, the palette is not the system's")
	}
	// Event colors follow the palette.
	e := &s.tr.Tracks[0].Lanes[0].Events[0]
	dark := s.pal.of(s.tr.Name(e), e.Kind)
	tt.SetDark(false)
	tt.Frame()
	if light := s.pal.of(s.tr.Name(e), e.Kind); light == dark || light != theme.Light.EventColor(s.tr.Name(e), e.Kind) {
		t.Error("event colors did not change with the palette")
	}
	applyPalette(theme.Dark)
}
