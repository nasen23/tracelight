package viewer

import (
	"slices"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// Going to the event a selection's correlation id links it to (a CUDA API
// call's kernel, a kernel's launch): the details' "Go to" buttons and the
// flow line's chip when the other end is out of view.

// goToLinked selects the linked event and shows it with the one it came
// from where both fit (zoomed to both, their rows scrolled into view), else
// the linked one alone. In the rank overview, the summary lane of its kind
// turns on; a trace left out shows again.
func (s *App) goToLinked() {
	in, ok := s.selectedInfo()
	if !ok || in.linked == nil {
		return
	}
	o := in.linked
	t := int(o.ref.Track)
	if ss := s.session(); ss != nil && ss.partOff(ss.trackPart[t]) {
		s.SetTraceShown(ss.trackPart[t], true)
	}
	if s.compactOn() {
		if key := laneKeyOf(s.tr.Tracks[t].Kind); key != "" && !slices.Contains(s.lanesOn(), key) {
			s.setLane(key, true)
		}
	}
	s.selectRef(o.ref)
	s.revealPair(t, int(in.ref.Track))
	a, b := o.start, o.end
	// Both, unless the linked event would be too thin to see.
	if ua, ub := min(in.start, o.start), max(in.end, o.end); float64(b-a)*1000 >= float64(ub-ua) {
		a, b = ua, ub
	}
	s.zoomTo(float64(a), float64(b), true)
}

// laneKeyOf is the key of the overview's summary lane that tracks of kind k
// draw into.
func laneKeyOf(k trace.TrackKind) string {
	kind := trackKindKey(k)
	for _, l := range theme.CompactLanes {
		if l.Kind == kind {
			return l.Key
		}
	}
	return ""
}

// rowOf is where track i is laid out: its row, or in the overview the
// summary lane it draws into.
func (s *App) rowOf(i int) (y, h float32, ok bool) {
	for _, r := range s.tl.rows {
		if r.kind == rowTrack && r.track == i || r.kind == rowSummary && slices.Contains(r.members, i) {
			return r.y, r.h, true
		}
	}
	return 0, 0, false
}

// revealPair scrolls so that track t shows (shown again if hidden or
// collapsed), with track o too where both fit (o < 0 for none).
func (s *App) revealPair(t, o int) {
	tl := &s.tl
	if s.hidden[t] || s.collapsed[s.tr.Tracks[t].Group] {
		delete(s.hidden, t)
		s.collapsed[s.tr.Tracks[t].Group] = false
	}
	s.layoutRows()
	ty, th, ok := s.rowOf(t)
	if !ok {
		return
	}
	view := tl.size.H - rulerH
	if o >= 0 {
		if oy, oh, ok := s.rowOf(o); ok {
			lo, hi := min(ty, oy), max(ty+th, oy+oh)
			if hi-lo <= view {
				if lo < tl.scrollY || hi > tl.scrollY+view {
					tl.scrollY = lo - (view-(hi-lo))/2
					s.clampScroll()
				}
				return
			}
		}
	}
	if ty < tl.scrollY || ty+th > tl.scrollY+view {
		tl.scrollY = ty - view/3
		s.clampScroll()
	}
}

// revealTrack scrolls the timeline so that track i shows.
func (s *App) revealTrack(i int) { s.revealPair(i, -1) }

// goToLabel is what the details' button that goes to the linked event
// says: to a kernel's launch, or to the GPU work an API call launched.
func goToLabel(e *eventInfo) string {
	if e.kind != trace.KindRuntime {
		return "Go to launch"
	}
	switch e.linked.kind {
	case trace.KindMemcpy:
		return "Go to copy"
	case trace.KindMemset:
		return "Go to memset"
	}
	return "Go to kernel"
}

// paintLinkChip draws, where the flow line leaves the view toward the
// linked event (at the point to, inside dataR, toward dir), a chip naming
// it, which goes to it when clicked.
func (s *App) paintLinkChip(p *ui.Painter, dataR ui.Rect, x, y float32, arrow, name string) {
	tl := &s.tl
	label := ui.Span{Text: arrow + " " + ellipsize(name, 48) + "  ·  Go to", Size: 11.5, Weight: 600, Color: colAccentText()}
	w, h := p.MeasureText(0, label)
	box := ui.Rect{X: x - w/2 - 8, Y: y - h/2 - 4, W: w + 16, H: h + 8}
	box.X = min(max(box.X, dataR.X+4), dataR.X+dataR.W-box.W-4)
	box.Y = min(max(box.Y, dataR.Y+4), dataR.Y+dataR.H-box.H-4)
	hot := tl.hovering && box.Contains(tl.size.X+tl.mouseX, tl.size.Y+tl.mouseY)
	bg := colAccentHi
	if hot {
		bg = colAccent
	}
	p.Shadow(box, 6, 0, 1, 6, 1, colShadow)
	p.Fill(box, bg, 6)
	p.RichText(box.X+8, box.Y+4, 0, label)
	tl.linkChip = box
}

// colAccentText is the color of text on the accent.
func colAccentText() ui.Color { return ui.Hex(active.AccentText) }

// edgeArrow is the arrow toward a point outside r, from inside it.
func edgeArrow(r ui.Rect, x, y float32) string {
	switch {
	case y < r.Y:
		return "↑"
	case y > r.Y+r.H:
		return "↓"
	case x < r.X:
		return "←"
	}
	return "→"
}
