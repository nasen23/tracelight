package viewer

import (
	"fmt"
	"strconv"

	"github.com/egoist/mygo/ui"
)

// The traces of a session that show: each can be left out (its rows, its
// block in the rank overview) and shown again, from the sidebar's TRACES
// list or a link's #only= / #omit=. The traces stay loaded and aligned as
// before, so the others do not move.

// session is the session shown, or nil.
func (s *App) session() *sessionSource {
	ss, _ := s.src.(*sessionSource)
	return ss
}

// partOff reports whether the session's part pi is left out.
func (ss *sessionSource) partOff(pi int) bool { return pi < len(ss.off) && ss.off[pi] }

// trackOff reports whether track i belongs to a trace left out.
func (s *App) trackOff(i int) bool {
	ss := s.session()
	return ss != nil && ss.partOff(ss.trackPart[i])
}

// shownParts is the number of the session's traces shown.
func (ss *sessionSource) shownParts() int {
	n := 0
	for i := range ss.parts {
		if !ss.partOff(i) {
			n++
		}
	}
	return n
}

// SetTraceShown shows or leaves out trace pi of the session. The last one
// shown stays.
func (s *App) SetTraceShown(pi int, show bool) {
	ss := s.session()
	if ss == nil || pi < 0 || pi >= len(ss.parts) || ss.partOff(pi) == !show {
		return
	}
	if !show && ss.shownParts() <= 1 {
		return
	}
	off := make([]bool, len(ss.parts))
	copy(off, ss.off)
	off[pi] = !show
	s.setOff(off)
}

// SoloTrace shows only trace pi of the session, or, if it is the only one
// shown already, every trace (Alt/⌥-click in the TRACES list).
func (s *App) SoloTrace(pi int) {
	ss := s.session()
	if ss == nil || pi < 0 || pi >= len(ss.parts) {
		return
	}
	off := make([]bool, len(ss.parts))
	if !(ss.shownParts() == 1 && !ss.partOff(pi)) {
		for i := range off {
			off[i] = i != pi
		}
	}
	s.setOff(off)
}

// ShowAllTraces shows every trace of the session.
func (s *App) ShowAllTraces() {
	if ss := s.session(); ss != nil {
		s.setOff(make([]bool, len(ss.parts)))
	}
}

// setOff sets the traces left out: the selection and the focus leave them.
func (s *App) setOff(off []bool) {
	ss := s.session()
	ss.off = off
	if s.selected != nil && s.trackOff(int(s.selected.Track)) {
		s.selected = nil
	}
	if ss.partOff(ss.focus) {
		for i := range ss.parts {
			if !ss.partOff(i) {
				ss.focus = i
				break
			}
		}
	}
	s.tl.dirty = true
}

// traceKeys are the names a link gives the session's traces (traceBase).
func (ss *sessionSource) traceKeys() []string {
	keys := make([]string, len(ss.parts))
	for i, p := range ss.parts {
		keys[i] = p.key
	}
	return keys
}

// applyTraceFilter leaves out the traces a link's only and omit say.
func (s *App) applyTraceFilter(only, omit []string) {
	if ss := s.session(); ss != nil && (len(only) > 0 || len(omit) > 0) {
		s.setOff(filterOff(ss.traceKeys(), only, omit))
	}
}

// shownExtent is the time the traces shown cover, on the session's axis.
func (s *App) shownExtent() (float64, float64) {
	ss := s.session()
	if ss == nil || ss.shownParts() == len(ss.parts) {
		return 0, float64(s.tr.Duration())
	}
	lo, hi := int64(-1), int64(0)
	for i, p := range ss.parts {
		if ss.partOff(i) {
			continue
		}
		if lo < 0 || p.offset < lo {
			lo = p.offset
		}
		hi = max(hi, p.offset+p.tr.Duration())
	}
	return float64(max(lo, 0)), float64(hi)
}

// tracesControl lists the session's traces, each with a box to show it or
// leave it out; Alt/⌥-click (or "only") shows it alone.
func (s *App) tracesControl(c *ui.Context, ss *sessionSource) {
	n, all := ss.shownParts(), len(ss.parts)
	ui.Row(c).Gap(8).Children(func() {
		ui.Text(c, fmt.Sprintf("%d of %d shown", n, all)).FontSize(11.5).TextColor(colFaint).Grow(1)
		if n < all {
			b := ui.Row(c.Key("all")).Focusable().Label("Show all traces").Tooltip("Show every trace")
			if b.Clicked() {
				s.ShowAllTraces()
			}
			b.Children(func() { ui.Text(c, "Show all").FontSize(11.5).TextColor(colAccent) })
		}
	})
	ui.Column(c).Gap(1).Children(func() {
		for i, p := range ss.parts {
			shown := !ss.partOff(i)
			row := ui.Row(c.Key("t:"+strconv.Itoa(i))).Gap(6).Padding(2, 4).Radius(5)
			if row.Hovered() {
				row.Background(colPanelAlt)
			}
			row.Children(func() {
				cb := ui.Checkbox(c.Key("cb"), &shown, "").Label("Show " + p.label).Tooltip("Show or leave out · ⌥/Alt-click: only this one")
				if cb.Changed() {
					if cb.ClickModifiers()&ui.Alt != 0 {
						s.SoloTrace(i)
					} else {
						s.SetTraceShown(i, shown)
					}
				}
				name := ui.Text(c, p.label).FontSize(11.5).SingleLine().Grow(1).Shrink(1)
				if !shown {
					name.TextColor(colFaint)
				} else if i == ss.focus && n > 1 {
					name.TextColor(colAccentHi)
				}
				ui.Text(c, compact(p.tr.EventCount())).FontSize(10.5).TextColor(colFaint)
				only := "only"
				if n == 1 && shown {
					only = "all"
				}
				o := ui.Row(c.Key("only")).Focusable().Label(map[string]string{"only": "Show only " + p.label, "all": "Show all traces"}[only])
				if o.Clicked() {
					s.SoloTrace(i)
				}
				col := colFaint
				if o.Hovered() {
					col = colAccent
				}
				o.Children(func() { ui.Text(c, only).FontSize(10.5).TextColor(col) })
			})
		}
	})
}

// TracesShown is how many of the session's traces show, of how many (1, 1
// for a single trace; 0, 0 for none).
func (s *App) TracesShown() (shown, all int) {
	if ss := s.session(); ss != nil {
		return ss.shownParts(), len(ss.parts)
	}
	if s.tr != nil {
		return 1, 1
	}
	return 0, 0
}
