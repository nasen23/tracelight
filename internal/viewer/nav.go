package viewer

import (
	"math"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
)

// Continuous navigation with held keys: while A or D (or ←, →) is held the
// view pans, and while W or S is held it zooms, at a constant rate per
// second (theme.NavPan, theme.NavZoom), times the navigation speed setting
// (theme.NavSpeeds), and three times faster with Shift. The
// frame loop moves the view by the time since the last frame, not by the
// keyboard's auto-repeat; the rates ease in and out (theme.NavEase). Keys go
// up on key-up events, and all at once when the timeline loses the keyboard.
//
// The browser client runs the same model (navAdvance in shared.js), which
// TestBrowserClientMatchesDesktop checks against this one.

// navKey is a key that moves the view while it is held.
type navKey uint8

const (
	navLeft navKey = 1 << iota
	navRight
	navIn
	navOut
)

func navKeyOf(k ui.Key) navKey {
	switch k {
	case ui.KeyA, ui.KeyLeft:
		return navLeft
	case ui.KeyD, ui.KeyRight:
		return navRight
	case ui.KeyW:
		return navIn
	case ui.KeyS:
		return navOut
	}
	return 0
}

// nav is the state of held-key navigation.
type nav struct {
	// held is the keys held, by key (A and ← both pan left, and either may
	// be let go first).
	held map[ui.Key]navKey
	// vPan is the pan speed, in view widths a second (positive to the
	// right); vZoom the zoom speed, in natural log of the scale a second
	// (positive zooms out).
	vPan, vZoom float64
}

// dirs is the directions of the keys held.
func (n *nav) dirs() navKey {
	var d navKey
	for _, k := range n.held {
		d |= k
	}
	return d
}

func (n *nav) press(k ui.Key) bool {
	d := navKeyOf(k)
	if d == 0 {
		return false
	}
	if n.held == nil {
		n.held = map[ui.Key]navKey{}
	}
	n.held[k] = d
	return true
}

func (n *nav) release(k ui.Key) bool {
	if navKeyOf(k) == 0 {
		return false
	}
	delete(n.held, k)
	return true
}

// stop lets go of every key and stops at once, as when the timeline loses
// the keyboard.
func (n *nav) stop() {
	clear(n.held)
	n.vPan, n.vZoom = 0, 0
}

// moving reports whether the view moves, or will in the next frame.
func (n *nav) moving() bool { return n.dirs() != 0 || n.vPan != 0 || n.vZoom != 0 }

// navTargets is the speeds the keys held ask for, at speed times the
// default rates.
func navTargets(d navKey, shift bool, speed float64) (pan, zoom float64) {
	if d&navRight != 0 {
		pan += theme.NavPan
	}
	if d&navLeft != 0 {
		pan -= theme.NavPan
	}
	if d&navOut != 0 {
		zoom += theme.NavZoom
	}
	if d&navIn != 0 {
		zoom -= theme.NavZoom
	}
	if shift {
		pan, zoom = pan*theme.NavShift, zoom*theme.NavShift
	}
	return pan * speed, zoom * speed
}

// navAdvance moves the speeds toward the targets over dt seconds, with an
// exponential ease, and returns how far they move the view meanwhile: dPan
// view widths and dZoom natural log of the scale (the trapezoid of the
// speeds at either end of the step). Speeds easing out to a target of zero
// stop below 2% of full speed.
func navAdvance(vPan, vZoom, tPan, tZoom, dt float64) (newPan, newZoom, dPan, dZoom float64) {
	k := 1 - math.Exp(-dt/theme.NavEase)
	newPan = vPan + (tPan-vPan)*k
	newZoom = vZoom + (tZoom-vZoom)*k
	// Below 2% of full speed the ease out is past seeing: stop there, so
	// that it lasts about a third of a second, not a long, idle tail.
	if tPan == 0 && math.Abs(newPan) < 0.02*theme.NavPan {
		newPan = 0
	}
	if tZoom == 0 && math.Abs(newZoom) < 0.02*theme.NavZoom {
		newZoom = 0
	}
	return newPan, newZoom, (vPan + newPan) / 2 * dt, (vZoom + newZoom) / 2 * dt
}

// stepNav moves the view by dt seconds of held-key navigation, and reports
// whether it should go on in the next frame. A step that starts the motion
// moves nothing, so that the time the window spent idle does not count.
func (s *App) stepNav(dt float64) bool {
	n := &s.nav
	if !n.moving() {
		return false
	}
	starting := n.vPan == 0 && n.vZoom == 0
	if starting || dt < 0 {
		dt = 0
	}
	dt = min(dt, 0.1) // a stalled frame moves no further than a tenth of a second
	tPan, tZoom := navTargets(n.dirs(), s.mods&ui.Shift != 0, s.NavSpeed())
	if starting {
		// Start at the first step's speed, so the first frame moves.
		n.vPan, n.vZoom, _, _ = navAdvance(0, 0, tPan, tZoom, 1.0/60)
		return true
	}
	var dPan, dZoom float64
	n.vPan, n.vZoom, dPan, dZoom = navAdvance(n.vPan, n.vZoom, tPan, tZoom, dt)
	tl := &s.tl
	w := s.dataW()
	anchor := w / 2
	if tl.hovering && float64(tl.mouseX) > float64(s.labelW()) {
		anchor = float64(tl.mouseX) - float64(s.labelW())
	}
	v := tl.cur
	if dZoom != 0 {
		t := v.start + anchor*v.scale
		v.scale *= math.Exp(dZoom)
		v.start = t - anchor*v.scale
	}
	v.start += dPan * w * v.scale
	// Held keys take over from a zoom animation, at the view it reached.
	tl.cur = s.clampView(v)
	tl.tgt, tl.animating = tl.cur, false
	return n.moving()
}
