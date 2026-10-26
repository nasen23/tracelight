package viewer

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// Metrics of the timeline, in DIPs.
// (shared with the browser client through package theme).
const (
	rulerH   = theme.RulerH
	laneH    = theme.LaneH
	trackPad = theme.TrackPad
	groupH   = theme.GroupH
	// minScale is the deepest zoom: nanoseconds a pixel.
	minScale = theme.MinScale
	// zoomPerNotch is how much a notch of the wheel (50 DIPs) zooms.
	zoomPerNotch = theme.ZoomPerNotch
	fontSmall    = 11
)

type rowKind uint8

const (
	rowGroup rowKind = iota
	rowTrack
	// rowIdle stands under the heading of a GPU without activity.
	rowIdle
	// rowIdleToggle shows or hides the GPUs without activity.
	rowIdleToggle
	// rowStrip is a trace's busy/idle strip in the compact view; rowSummary
	// one of its summary lanes (compactview.go).
	rowStrip
	rowSummary
)

// row is a line of the timeline: a group's heading or a track.
type row struct {
	kind  rowKind
	group string
	track int
	// y is the row's top in the content below the ruler; h its height.
	y, h float32
	// lh is the height of a track's lanes, pad the space above and below
	// them (trackLane).
	lh, pad float32
	// count of tracks and events of a group.
	tracks, events int
	// idle marks the heading of a GPU without activity.
	idle bool
	// In the compact view: the trace (rank) of the row, its summary lane's
	// key, and the tracks it draws (the GPU rows of a strip).
	rank    int
	key     string
	members []int
	// slot is the sub-lane (trace.Track.Overview) a summary row draws, sub
	// its place in its summary lane, band which of the trace's summary
	// lanes it is in.
	slot, sub, band int
	// top: a thread's band of nested ranges starts at it (not the first).
	top bool
}

// viewport is the time the timeline shows: start is the time at the left of
// the data area, relative to the trace's start, and scale the nanoseconds a
// pixel.
type viewport struct{ start, scale float64 }

// timeline is the state of the timeline view.
type timeline struct {
	cur, tgt viewport
	// anchorX is the pixel, in the data area, that the zoom animation keeps
	// still.
	anchorX   float64
	animating bool
	lastFrame time.Time
	fitted    bool
	// want is the view to show first instead of the whole trace (a link's
	// #v=), once the timeline has a size.
	want *[2]float64

	scrollY  float32
	rows     []row
	contentH float32
	dirty    bool

	// size is the canvas's size as last painted.
	size ui.Rect

	mouseX, mouseY float32
	hovering       bool

	dragging, dragMoved, rangeDrag bool
	// gutterDrag: the label gutter's edge is being dragged (its width).
	gutterDrag     bool
	dragX0, dragY0 float32
	dragStart      float64
	dragScroll     float32

	// rangeA and rangeB bound the measured range (relative times);
	// rangeClose is where its × in the ruler is, as last painted.
	rangeA, rangeB float64
	hasRange       bool
	rangeClose     ui.Rect
	// linkChip is where the chip of a flow line leaving the view is, as
	// last painted (empty for none): a click goes to the linked event.
	linkChip ui.Rect

	blocks []vblock

	// ranks are the traces' blocks of rows in the compact view.
	ranks []rankSpan
	// busy and busyIn are scratch for the busy/idle strips.
	busy, busyIn []float64
	// colA, colC and wide are scratch for the summary lanes' columns.
	colA []float32
	colC []ui.Color
	wide []vblock
}

// hit is what is under a point of the canvas.
type hit struct {
	ok     bool
	ruler  bool
	gutter bool
	group  bool
	row    int
	ref    trace.Ref
	// near is the number of events within a few pixels of the point, more
	// than one where they merge.
	near int
	busy int64
	// t0, t1 is the time those events span (relative).
	t0, t1 int64
	// b is the event, or the events, under the point.
	b vblock
	// idleToggle is the row that shows or hides idle GPUs.
	idleToggle bool
}

// labelW is the width of the label gutter (the rows' names): as its edge
// was dragged to, else theme.LabelW; within theme.LabelMinGutter and
// theme.LabelMaxGutter.
func (s *App) labelW() float32 {
	if s.ctl == nil || s.ctl.settings.LabelW <= 0 {
		return theme.LabelW
	}
	return clampLabelW(s.ctl.settings.LabelW)
}

// clampLabelW keeps a gutter's width within its bounds.
func clampLabelW(w float32) float32 {
	return min(max(w, theme.LabelMinGutter), theme.LabelMaxGutter)
}

// onGutterEdge reports whether x (in the timeline) is on the label
// gutter's edge, which drags.
func (s *App) onGutterEdge(x float32) bool {
	return x >= s.labelW()-4 && x <= s.labelW()+3
}

func (s *App) dataW() float64 { return max(float64(s.tl.size.W)-float64(s.labelW()), 50) }

// layoutRows lays out the groups and tracks shown. GPUs that did nothing
// are hidden at first; a row at the top shows them, after the other GPUs.
func (s *App) layoutRows() {
	tl := &s.tl
	tl.rows = tl.rows[:0]
	if s.compactOn() {
		s.layoutCompact()
		return
	}
	tl.ranks = tl.ranks[:0]
	var y float32
	group := ""
	gi := -1
	if len(s.idle) > 0 {
		// The toggle leads the timeline, where it shows without scrolling.
		tl.rows = append(tl.rows, row{kind: rowIdleToggle, y: y, h: laneH + 2})
		y += laneH + 2
	}
	idleDone := len(s.idle) == 0 || !s.showIdle
	idleRows := func() {
		idleDone = true
		for _, id := range s.idle {
			g := idleGroupName(s.tr, id)
			tl.rows = append(tl.rows, row{kind: rowGroup, group: g, y: y, h: groupH, idle: true})
			y += groupH
			if !s.collapsed[g] {
				tl.rows = append(tl.rows, row{kind: rowIdle, group: g, y: y, h: laneH + 2*trackPad})
				y += laneH + 2*trackPad
			}
		}
	}
	for i, tr := range s.tr.Tracks {
		if s.trackOff(i) {
			continue // a trace left out (traces.go)
		}
		if !idleDone && tr.Kind != trace.TrackStream {
			idleRows()
		}
		if tr.Group != group || gi < 0 {
			group = tr.Group
			tl.rows = append(tl.rows, row{kind: rowGroup, group: group, y: y, h: groupH})
			gi = len(tl.rows) - 1
			y += groupH
		}
		tl.rows[gi].tracks++
		tl.rows[gi].events += tr.Count
		if s.collapsed[group] || s.hidden[i] {
			continue
		}
		k := trackKindKey(tr.Kind)
		h := trackHeight(k, len(tr.Lanes))
		lh, pad := trackLane(k)
		tl.rows = append(tl.rows, row{kind: rowTrack, group: group, track: i, y: y, h: h, lh: lh, pad: pad})
		y += h
	}
	if !idleDone {
		idleRows()
	}
	tl.contentH = y
	tl.dirty = false
}

// idleGroupName is the heading of a GPU without activity.
func idleGroupName(tr *trace.Trace, id int32) string {
	g := "GPU " + fmt.Sprint(id)
	if n := tr.Devices[id]; n != "" {
		g += " · " + n
	}
	return g
}

// setShowIdle shows or hides the GPUs without activity.
func (s *App) setShowIdle(show bool) {
	s.showIdle = show
	s.tl.dirty = true
}

// fit shows the whole trace.
func (s *App) fit(animate bool) {
	a, b := s.shownExtent()
	s.zoomTo(a, b, animate)
}

// zoomTo shows relative times a to b, with a margin.
func (s *App) zoomTo(a, b float64, animate bool) {
	w := s.dataW()
	span := max(b-a, 100)
	margin := span * 0.02
	v := viewport{start: a - margin, scale: (span + 2*margin) / w}
	s.tl.tgt = s.clampView(v)
	s.tl.anchorX = w / 2
	if animate && !s.noAnim {
		s.tl.animating = true
	} else {
		s.tl.cur, s.tl.animating = s.tl.tgt, false
	}
}

func (s *App) clampView(v viewport) viewport {
	w := s.dataW()
	dur := float64(max(s.tr.Duration(), 1000))
	v.scale = min(max(v.scale, minScale), dur*1.5/w)
	span := v.scale * w
	v.start = min(max(v.start, -span*0.8), dur-span*0.2)
	return v
}

// zoomAt zooms by factor (more than 1 zooms out) around the pixel x of the
// data area.
func (s *App) zoomAt(factor, x float64) {
	tl := &s.tl
	t := tl.tgt.start + x*tl.tgt.scale
	scale := tl.tgt.scale * factor
	tl.tgt = s.clampView(viewport{start: t - x*scale, scale: scale})
	// The zoom keeps the time under x still unless clamping moved it.
	tl.anchorX = x
	if s.noAnim {
		tl.cur = tl.tgt
	} else {
		tl.animating = true
	}
}

// panBy moves the view by dx pixels at once.
func (s *App) panBy(dx float64) {
	tl := &s.tl
	tl.cur.start += dx * tl.cur.scale
	tl.cur = s.clampView(tl.cur)
	tl.tgt.start += dx * tl.tgt.scale
	tl.tgt = s.clampView(tl.tgt)
}

func (s *App) scrollBy(dy float32) {
	tl := &s.tl
	tl.scrollY += dy
	s.clampScroll()
}

func (s *App) clampScroll() {
	tl := &s.tl
	maxY := max(tl.contentH-(tl.size.H-rulerH)+laneH, 0)
	tl.scrollY = min(max(tl.scrollY, 0), maxY)
}

// step moves the zoom animation on to now; it reports whether it goes on.
func (s *App) step(now time.Time) bool {
	tl := &s.tl
	dt := now.Sub(tl.lastFrame).Seconds()
	tl.lastFrame = now
	if !tl.animating {
		return false
	}
	if dt <= 0 {
		return true // no time passed: nothing moves yet
	}
	if dt > 0.1 {
		dt = 1.0 / 60 // the first frame after a pause
	}
	k := 1 - math.Exp(-dt/0.065)
	x := tl.anchorX
	ls := math.Log(tl.cur.scale) + (math.Log(tl.tgt.scale)-math.Log(tl.cur.scale))*k
	scale := math.Exp(ls)
	ac := tl.cur.start + x*tl.cur.scale
	at := tl.tgt.start + x*tl.tgt.scale
	a := ac + (at-ac)*k
	tl.cur = viewport{start: a - x*scale, scale: scale}
	if math.Abs(math.Log(tl.cur.scale/tl.tgt.scale)) < 0.002 && math.Abs(tl.cur.start-tl.tgt.start) < 0.3*tl.tgt.scale {
		tl.cur, tl.animating = tl.tgt, false
	}
	return tl.animating
}

// input takes the canvas's pointer, wheel and keys.
func (s *App) input(ev ui.InputEvent) bool {
	tl := &s.tl
	switch ev.Kind {
	case ui.InputPointerMove:
		tl.mouseX, tl.mouseY, tl.hovering = ev.X, ev.Y, true
		if tl.gutterDrag {
			s.ctl.settings.LabelW = clampLabelW(ev.X)
			return true
		}
		if tl.dragging {
			dx, dy := ev.X-tl.dragX0, ev.Y-tl.dragY0
			if math.Abs(float64(dx)) > 3 || math.Abs(float64(dy)) > 3 {
				tl.dragMoved = true
			}
			if tl.rangeDrag {
				tl.rangeB = s.timeAtX(ev.X)
				tl.hasRange = true
			} else if tl.dragMoved {
				start := tl.dragStart - float64(dx)*tl.cur.scale
				tl.cur.start, tl.tgt.start = start, start
				tl.tgt.scale = tl.cur.scale
				tl.animating = false
				tl.cur = s.clampView(tl.cur)
				tl.tgt = tl.cur
				tl.scrollY = tl.dragScroll - dy
				s.clampScroll()
			}
		}
		return true
	case ui.InputPointerDown:
		if ev.Button != 0 {
			return false
		}
		tl.mouseX, tl.mouseY = ev.X, ev.Y
		if tl.hasRange && tl.rangeClose.W > 0 && tl.rangeClose.Contains(tl.size.X+ev.X, tl.size.Y+ev.Y) {
			s.clearRange()
			return true
		}
		if tl.linkChip.W > 0 && tl.linkChip.Contains(tl.size.X+ev.X, tl.size.Y+ev.Y) {
			s.goToLinked()
			return true
		}
		// The label gutter's edge: dragged, its width; double-clicked, back
		// to the default.
		if s.onGutterEdge(ev.X) {
			if ev.Clicks == 2 {
				s.ctl.settings.LabelW = 0
				s.settingsChanged()
				return true
			}
			tl.gutterDrag = true
			return true
		}
		tl.dragging, tl.dragMoved = true, false
		tl.dragX0, tl.dragY0 = ev.X, ev.Y
		tl.dragStart, tl.dragScroll = tl.cur.start, tl.scrollY
		tl.rangeDrag = ev.Mods&ui.Shift != 0 && ev.X > s.labelW()
		if tl.rangeDrag {
			tl.rangeA = s.timeAtX(ev.X)
			tl.rangeB = tl.rangeA
		}
		if ev.Clicks == 2 && !tl.rangeDrag {
			if h := s.hitAt(ev.X, ev.Y); h.ok {
				s.selectRef(h.ref)
				s.zoomTo(float64(h.b.start), float64(h.b.end), true)
			}
		}
		return true
	case ui.InputPointerUp:
		if ev.Button != 0 {
			return false
		}
		if tl.gutterDrag {
			tl.gutterDrag = false
			s.settingsChanged() // kept, as the other settings
			return true
		}
		// The second click of a double click on an event zoomed to it as
		// it went down; elsewhere it counts as a click of its own.
		if tl.dragging && !tl.dragMoved && !tl.rangeDrag && (ev.Clicks < 2 || !s.hitAt(ev.X, ev.Y).ok) {
			s.click(ev.X, ev.Y)
		}
		if tl.rangeDrag {
			if tl.dragMoved {
				s.rangeEnded()
			} else {
				s.clearRange()
			}
		}
		tl.dragging, tl.rangeDrag = false, false
		return true
	case ui.InputScroll:
		x := float64(ev.X) - float64(s.labelW())
		mods := ev.Mods | s.mods
		zoomMods := ui.Ctrl | ui.Super | ui.Alt
		switch {
		case mods&zoomMods != 0:
			// Ctrl/Cmd + wheel zooms around the pointer, as in Nsight Systems.
			s.zoomAt(math.Pow(zoomPerNotch, float64(ev.DY)/50), max(x, 0))
		case mods&ui.Shift != 0 && ev.DX == 0:
			s.panBy(float64(ev.DY))
		default:
			if ev.DX != 0 {
				s.panBy(float64(ev.DX))
			}
			if ev.DY != 0 {
				s.scrollBy(ev.DY)
			}
		}
		return true
	case ui.InputKeyDown:
		// Navigation keys move the view while held (nav.go): the frame loop
		// moves it, so their auto-repeat is ignored.
		if navKeyOf(ev.Key) != 0 && ev.Mods&^ui.Shift == 0 {
			if !ev.Repeat {
				s.nav.press(ev.Key)
			}
			return true
		}
		return s.key(ev)
	case ui.InputKeyUp:
		return s.nav.release(ev.Key)
	}
	return false
}

func (s *App) key(ev ui.InputEvent) bool {
	tl := &s.tl
	w := s.dataW()
	anchor := w / 2
	if tl.hovering && float64(tl.mouseX) > float64(s.labelW()) {
		anchor = float64(tl.mouseX) - float64(s.labelW())
	}
	switch ev.Key {
	case ui.KeyEqual:
		s.zoomAt(1/1.6, anchor)
	case ui.KeyMinus:
		s.zoomAt(1.6, anchor)
	case ui.KeyUp:
		s.scrollBy(-3 * laneH)
	case ui.KeyDown:
		s.scrollBy(3 * laneH)
	case ui.KeyPageUp:
		s.scrollBy(-(tl.size.H - rulerH))
	case ui.KeyPageDown:
		s.scrollBy(tl.size.H - rulerH)
	case ui.KeyF:
		if in, ok := s.selectedInfo(); ok {
			s.zoomTo(float64(in.start), float64(in.end), true)
		} else if s.selected == nil {
			s.fit(true)
		}
	case ui.KeyG: // to the event the selection is linked to (kernel, launch)
		s.goToLinked()
	case ui.KeyZ, ui.KeyEnter:
		s.zoomToRange()
	case ui.Key0, ui.KeyHome:
		s.fit(true)
	case ui.KeyEscape:
		s.selected = nil
		s.clearRange()
	case ui.KeyP:
		s.showPerf = !s.showPerf
	case ui.KeyC:
		s.toggleCompact()
	default:
		return false
	}
	return true
}

func (s *App) panAnimated(dx float64) {
	tl := &s.tl
	tl.tgt.start += dx * tl.tgt.scale
	tl.tgt = s.clampView(tl.tgt)
	tl.anchorX = s.dataW() / 2
	if s.noAnim {
		tl.cur = tl.tgt
	} else {
		tl.animating = true
	}
}

// timeAtX is the relative time at canvas pixel x.
func (s *App) timeAtX(x float32) float64 {
	return s.tl.cur.start + (float64(x)-float64(s.labelW()))*s.tl.cur.scale
}

// xAt is the canvas pixel of relative time t.
func (s *App) xAt(t float64) float64 {
	return float64(s.labelW()) + (t-s.tl.cur.start)/s.tl.cur.scale
}

// rowAt returns the row at canvas y, or -1.
func (s *App) rowAt(y float32) int {
	tl := &s.tl
	cy := y - rulerH + tl.scrollY
	i := sort.Search(len(tl.rows), func(i int) bool { return tl.rows[i].y+tl.rows[i].h > cy })
	if i < len(tl.rows) && tl.rows[i].y <= cy {
		return i
	}
	return -1
}

// hitAt finds what is at canvas point x, y.
func (s *App) hitAt(x, y float32) hit {
	tl := &s.tl
	if y < rulerH {
		return hit{ruler: true, row: -1}
	}
	ri := s.rowAt(y)
	if ri < 0 {
		return hit{row: -1}
	}
	r := tl.rows[ri]
	switch r.kind {
	case rowGroup:
		return hit{group: true, row: ri}
	case rowIdleToggle:
		return hit{idleToggle: true, row: ri}
	case rowIdle, rowStrip:
		return hit{row: ri}
	case rowSummary:
		if x < s.labelW() {
			return hit{gutter: true, row: ri}
		}
		return s.hitSummary(ri, x)
	}
	if x < s.labelW() {
		return hit{gutter: true, row: ri}
	}
	cy := y - rulerH + tl.scrollY
	lane := int(math.Floor(float64((cy - r.y - r.pad) / r.lh)))
	tr := s.tr.Tracks[r.track]
	if lane < 0 || lane >= len(tr.Lanes) {
		return hit{row: ri}
	}
	scale := tl.cur.scale
	b, ok := s.src.hit(r.track, lane, s.timeAtX(x), 3*scale, scale)
	if !ok {
		return hit{row: ri}
	}
	return hit{ok: true, row: ri, ref: trace.Ref{Track: int32(r.track), Lane: int32(lane), Index: int32(b.index)},
		near: max(b.count, 1), busy: b.busy, t0: b.start, t1: b.end, b: b}
}

func (s *App) click(x, y float32) {
	h := s.hitAt(x, y)
	switch {
	case h.idleToggle:
		s.setShowIdle(!s.showIdle)
	case h.group:
		g := s.tl.rows[h.row].group
		s.collapsed[g] = !s.collapsed[g]
		s.tl.dirty = true
	case h.ok:
		s.selectRef(h.ref)
	case !h.ruler && !h.gutter:
		s.selected = nil
		s.clearRange()
	}
}

func (s *App) selectRef(r trace.Ref) {
	s.selected = &r
	s.panelTab = 0
}

// paint draws the timeline into r.
func (s *App) paint(p *ui.Painter, r ui.Rect) {
	paintStart := time.Now()
	s.perf.blocks = 0
	tl := &s.tl
	resized := tl.size.W != r.W || tl.size.H != r.H
	tl.size = r
	if resized {
		if !tl.fitted {
			if tl.want != nil { // exactly, as the link says (the web client's way)
				tl.cur = s.clampView(viewport{start: tl.want[0], scale: (tl.want[1] - tl.want[0]) / s.dataW()})
				tl.tgt, tl.want = tl.cur, nil
			} else {
				s.fit(false)
			}
			tl.fitted = true
		}
		tl.cur, tl.tgt = s.clampView(tl.cur), s.clampView(tl.tgt)
		s.clampScroll()
	}
	if tl.dirty {
		s.layoutRows()
		s.clampScroll()
	}
	p.Fill(r, colBg, 0)
	dataX := r.X + s.labelW()
	dataW := r.W - s.labelW()
	top := r.Y + rulerH
	view := tl.cur
	step := tickStep(view.scale, 110)
	s.prepareStream()
	s.src.frame(view.start, view.start+float64(dataW)*view.scale, view.scale)

	// Alternate shading and grid lines under the events.
	p.Clip(ui.Rect{X: r.X, Y: top, W: r.W, H: r.H - rulerH}, 0, func() {
		s.paintGrid(p, ui.Rect{X: dataX, Y: top, W: dataW, H: r.H - rulerH}, step)
		for ri := s.firstVisibleRow(); ri < len(tl.rows); ri++ {
			row := &tl.rows[ri]
			y := top + row.y - tl.scrollY
			if y > r.Y+r.H {
				break
			}
			switch row.kind {
			case rowGroup:
				s.paintGroup(p, r, y, row)
			case rowTrack:
				s.paintTrack(p, r, y, ri, row)
			case rowIdle:
				p.Fill(ui.Rect{X: r.X, Y: y, W: s.labelW(), H: row.h}, colPanel, 0)
				p.Fill(ui.Rect{X: r.X + s.labelW() - 1, Y: y, W: 1, H: row.h}, colBorder, 0)
				p.RichText(r.X+22, y+trackPad+3, 0, ui.Span{Text: "No activity", Size: 12, Color: colFaint})
				p.RichText(r.X+s.labelW()+12, y+trackPad+3, 0,
					ui.Span{Text: "no kernels, memcpy or memset were recorded on this GPU", Size: 11.5, Color: colFaint})
			case rowIdleToggle:
				s.paintIdleToggle(p, r, y, row)
			case rowStrip:
				s.paintStrip(p, r, y, row)
			case rowSummary:
				s.paintSummary(p, r, y, ri, row)
			}
		}
		s.paintRankLabels(p, r)
		s.paintOverlays(p, r)
	})
	s.paintRuler(p, r, step)
	if tl.hovering && !tl.dragging {
		s.paintTooltip(p, r)
	}
	s.perf.add(time.Since(paintStart))
	if s.showPerf {
		s.paintPerf(p, r)
	}
}

// perf keeps the time of the last paints of the timeline, for the overlay
// (P).
type perf struct {
	times  [120]time.Duration
	n      int
	blocks int // LOD blocks drawn in the last paint
}

func (pf *perf) add(d time.Duration) {
	pf.times[pf.n%len(pf.times)] = d
	pf.n++
}

// stats returns the last, median, 95th percentile and largest paint time.
func (pf *perf) stats() (last, p50, p95, worst time.Duration) {
	n := min(pf.n, len(pf.times))
	if n == 0 {
		return
	}
	s := make([]time.Duration, n)
	copy(s, pf.times[:n])
	slices.Sort(s)
	return pf.times[(pf.n-1)%len(pf.times)], s[n/2], s[min(n-1, n*95/100)], s[n-1]
}

// paintPerf draws the overlay of paint times in the timeline's corner.
func (s *App) paintPerf(p *ui.Painter, r ui.Rect) {
	last, p50, p95, worst := s.perf.stats()
	msf := func(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d)/1e6) }
	lines := [][]ui.Span{
		{{Text: "timeline paint  ", Size: 11, Color: colFaint}, {Text: msf(last) + " ms", Size: 11, Weight: 600, Color: colText}},
		{{Text: "p50 " + msf(p50) + " · p95 " + msf(p95) + " · max " + msf(worst) + " ms", Size: 11, Color: colMuted}},
		{{Text: fmt.Sprintf("last %d frames · %s blocks drawn", min(s.perf.n, len(s.perf.times)), fmtCount(s.perf.blocks)), Size: 11, Color: colFaint}},
		{{Text: fmtCount(s.tr.EventCount()) + " events · P hides", Size: 11, Color: colFaint}},
	}
	if st := s.streamStatus(); st != "" {
		lines = append(lines, []ui.Span{{Text: st, Size: 11, Color: colFaint}})
	}
	var w, h float32
	for _, l := range lines {
		lw, lh := p.MeasureText(0, l...)
		w, h = max(w, lw), h+lh+2
	}
	box := ui.Rect{X: r.X + r.W - w - 34, Y: r.Y + r.H - h - 30, W: w + 20, H: h + 14}
	p.Fill(box, colTooltip.Alpha(0.92), 8)
	p.Stroke(box, colBorder, 8, 1)
	y := box.Y + 7
	for _, l := range lines {
		_, lh := p.MeasureText(0, l...)
		p.RichText(box.X+10, y, 0, l...)
		y += lh + 2
	}
}

func (s *App) firstVisibleRow() int {
	tl := &s.tl
	return sort.Search(len(tl.rows), func(i int) bool { return tl.rows[i].y+tl.rows[i].h > tl.scrollY })
}

func (s *App) paintGrid(p *ui.Painter, r ui.Rect, step float64) {
	view := s.tl.cur
	first := math.Floor(view.start/step) * step
	for t := first; ; t += step {
		x := float32(s.xAt(t))
		if x > r.X+r.W {
			break
		}
		if x >= r.X {
			p.Fill(ui.Rect{X: x, Y: r.Y, W: 1, H: r.H}, colGrid, 0)
		}
	}
}

func (s *App) paintGroup(p *ui.Painter, r ui.Rect, y float32, row *row) {
	collapsed := s.collapsed[row.group]
	p.Fill(ui.Rect{X: r.X, Y: y, W: r.W, H: groupH}, colPanelAlt, 0)
	p.Fill(ui.Rect{X: r.X, Y: y + groupH - 1, W: r.W, H: 1}, colBorder, 0)
	// A chevron, right when collapsed, down when open.
	cx, cy := r.X+14, y+groupH/2
	var path ui.Path
	if collapsed {
		path.MoveTo(cx-2, cy-4).LineTo(cx+2, cy).LineTo(cx-2, cy+4)
	} else {
		path.MoveTo(cx-4, cy-2).LineTo(cx, cy+2).LineTo(cx+4, cy-2)
	}
	p.StrokePath(&path, 1.5, colMuted)
	if row.idle {
		p.RichText(r.X+26, y+5, 0,
			ui.Span{Text: row.group, Size: 12, Weight: 600, Color: colMuted},
			ui.Span{Text: "   idle", Size: fontSmall, Color: colFaint})
		return
	}
	p.RichText(r.X+26, y+5, 0,
		ui.Span{Text: row.group, Size: 12, Weight: 600, Color: colText},
		ui.Span{Text: "   " + fmtCount(row.tracks) + " rows · " + fmtCount(row.events) + " events", Size: fontSmall, Color: colFaint})
}

// paintIdleToggle draws the row that shows or hides the GPUs without
// activity.
func (s *App) paintIdleToggle(p *ui.Painter, r ui.Rect, y float32, row *row) {
	p.Fill(ui.Rect{X: r.X, Y: y, W: r.W, H: row.h}, colRuler, 0)
	p.Fill(ui.Rect{X: r.X, Y: y + row.h - 1, W: r.W, H: 1}, colBorder, 0)
	n := len(s.idle)
	gpus := "GPUs"
	if n == 1 {
		gpus = "GPU"
	}
	action, state := "Show", "hidden"
	if s.showIdle {
		action, state = "Hide", "shown"
	}
	hover := s.tl.hovering && s.rowAt(s.tl.mouseY) >= 0 && s.tl.rows[s.rowAt(s.tl.mouseY)].kind == rowIdleToggle
	link := colAccent
	if hover {
		link = colAccentHi
	}
	p.RichText(r.X+26, y+4, 0,
		ui.Span{Text: fmt.Sprintf("%d %s with no activity %s  ·  ", n, gpus, state), Size: 11.5, Color: colFaint},
		ui.Span{Text: action, Size: 11.5, Weight: 600, Color: link, Underline: hover})
}

func (s *App) paintTrack(p *ui.Painter, r ui.Rect, y float32, ri int, row *row) {
	tl := &s.tl
	tr := s.tr.Tracks[row.track]
	if ri%2 == 0 {
		p.Fill(ui.Rect{X: r.X + s.labelW(), Y: y, W: r.W - s.labelW(), H: row.h}, colRowAlt, 0)
	}
	p.Fill(ui.Rect{X: r.X, Y: y + row.h - 1, W: r.W, H: 1}, colGrid, 0)

	dataX := float64(r.X) + float64(s.labelW())
	dataW := float64(r.W) - float64(s.labelW())
	view := tl.cur
	t0, t1 := view.start, view.start+dataW*view.scale
	for li := range tr.Lanes {
		ly := y + row.pad + float32(li)*row.lh
		if ly > r.Y+r.H || ly+row.lh < r.Y+rulerH {
			continue
		}
		tl.blocks = s.src.lod(row.track, li, t0, t1, view.scale, tl.blocks[:0])
		s.paintBlocks(p, tl.blocks, ly, row.lh, dataX, dataW)
	}

	// The label gutter, over events that start before the view.
	g := ui.Rect{X: r.X, Y: y, W: s.labelW(), H: row.h}
	p.Fill(g, colPanel, 0)
	p.Fill(ui.Rect{X: r.X + s.labelW() - 1, Y: y, W: 1, H: row.h}, colBorder, 0)
	p.Fill(ui.Rect{X: r.X, Y: y + row.h - 1, W: s.labelW(), H: 1}, colGrid, 0)
	// A swatch of the track's kind.
	sw := colSwatchStream
	switch tr.Kind {
	case trace.TrackRuntime:
		sw = colSwatchRuntime
	case trace.TrackNVTX:
		sw = colSwatchNVTX
	case trace.TrackCPU:
		sw = colSwatchCPU
	case trace.TrackPython:
		sw = colSwatchPython
	}
	// The name and swatch centered on the first lane (of a stream's, of
	// laneH where they are smaller).
	ny := y + row.pad + (row.lh-laneH)/2
	p.Fill(ui.Rect{X: r.X + 12, Y: ny + 5, W: 3, H: laneH - 10}, sw, 1.5)
	p.Clip(ui.Rect{X: r.X + 20, Y: y, W: s.labelW() - 28, H: row.h}, 0, func() {
		p.RichText(r.X+22, ny+3-(laneH-row.lh)/2, 0, ui.Span{Text: tr.Name, Size: 12, Color: colText})
		// Tracks with room for a second line say how busy they are.
		if row.h >= 2*row.lh {
			info := fmtCount(tr.Count) + " events"
			if d := s.tr.Duration(); d > 0 && tr.Kind == trace.TrackStream {
				info += " · " + trimFloat(100*float64(tr.Busy)/float64(d), 1) + "% busy"
			}
			if len(tr.Lanes) > 1 {
				info += " · depth " + fmtCount(len(tr.Lanes))
			}
			p.RichText(r.X+22, y+row.pad+row.lh+1, 0, ui.Span{Text: info, Size: 10.5, Color: colFaint})
		}
	})
}

// paintBlocks draws a lane's blocks with its top at ly, lh high: events with their
// names where they fit, events merged as blocks as opaque as they are dense.
func (s *App) paintBlocks(p *ui.Painter, blocks []vblock, ly, lh float32, dataX, dataW float64) {
	view := s.tl.cur
	s.perf.blocks += len(blocks)
	for i := range blocks {
		b := &blocks[i]
		x0 := dataX + (float64(b.start)-view.start)/view.scale
		x1 := dataX + (float64(b.end)-view.start)/view.scale
		x0, x1 = max(x0, dataX-4), min(x1, dataX+dataW+4)
		w := max(x1-x0, 1)
		box := ui.Rect{X: float32(x0), Y: ly + 1, W: float32(w), H: lh - 3}
		col := s.pal.of(b.name, b.kind)
		dim := s.highlight != "" && (b.merged || b.name != s.highlight)
		switch {
		case b.kind == trace.KindNVTXMark && !b.merged:
			s.paintMark(p, float32(x0), ly+lh/2, col)
		case b.merged:
			// Events too small to tell apart: a block as opaque as they are
			// dense, in the color of the first, with a hairline on top to
			// tell it from a single event.
			a := float32(0.3 + 0.6*b.density())
			if dim {
				a *= 0.3
			}
			p.Fill(box, col.Mix(colBg, 0.3).Alpha(a), 0)
			p.Fill(ui.Rect{X: box.X, Y: box.Y, W: box.W, H: 1}, col.Alpha(a), 0)
		default:
			if dim {
				col = col.Mix(colBg, 0.75)
			}
			radius := float32(0)
			if w > 6 {
				radius = 3
			}
			// A seam between events back to back: where one ends and the
			// next starts shows.
			if w >= 4 {
				box.W--
			}
			p.Fill(box, col, radius)
			if w >= theme.LabelMinW {
				s.paintLabel(p, box, b.name, b.end-b.start)
			}
		}
	}
}

func (s *App) paintMark(p *ui.Painter, x, cy float32, col ui.Color) {
	var d ui.Path
	d.MoveTo(x, cy-6).LineTo(x+5, cy).LineTo(x, cy+6).LineTo(x-5, cy).Close()
	p.FillPath(&d, col)
}

func (s *App) paintLabel(p *ui.Painter, box ui.Rect, name string, dur int64) {
	if len(name) > 200 {
		name = name[:200]
	}
	label := ui.Span{Text: name, Size: fontSmall, Weight: 500, Color: colOnEvent}
	w, h := p.MeasureText(0, label)
	pad := float32(5)
	if box.W < 40 { // a narrow box: room for a letter or two more
		pad = 3
	}
	x := box.X + pad
	if w+2*pad > box.W {
		// Clip names that do not fit, from the left of the box.
		p.Clip(ui.Rect{X: box.X + 2, Y: box.Y, W: box.W - 4, H: box.H}, 0, func() {
			p.RichText(x, box.Y+(box.H-h)/2, 0, label)
		})
		return
	}
	if box.W > w+2*pad+60 {
		ds := ui.Span{Text: "  " + fmtDur(float64(dur)), Size: 10, Color: colOnEvent.Alpha(0.65)}
		dw, _ := p.MeasureText(0, ds)
		if w+dw+2*pad <= box.W {
			p.RichText(x, box.Y+(box.H-h)/2, 0, label, ds)
			return
		}
	}
	p.RichText(x, box.Y+(box.H-h)/2, 0, label)
}

// eventBox returns where the event at ref is drawn, if its track is shown
// (and, from a server, its details have come).
func (s *App) eventBox(ref trace.Ref) (ui.Rect, bool) {
	in, ok := s.src.event(ref)
	if !ok {
		return ui.Rect{}, false
	}
	return s.boxAt(ref, in.start, in.end)
}

// boxAt returns where an event of ref's lane from start to end is drawn.
func (s *App) boxAt(ref trace.Ref, start, end int64) (ui.Rect, bool) {
	tl := &s.tl
	for _, row := range tl.rows {
		if row.kind == rowSummary && slices.Contains(row.members, int(ref.Track)) && s.slotOf(int(ref.Track), int(ref.Lane)) == row.slot {
			x0 := max(s.xAt(float64(start)), float64(s.labelW())-4)
			x1 := min(s.xAt(float64(end)), float64(tl.size.W)+4)
			y := tl.size.Y + rulerH + row.y - tl.scrollY
			return ui.Rect{X: tl.size.X + float32(x0), Y: y + 1, W: float32(max(x1-x0, 1)), H: row.h - 2}, true
		}
		if row.kind != rowTrack || row.track != int(ref.Track) {
			continue
		}
		x0 := s.xAt(float64(start))
		x1 := s.xAt(float64(end))
		y := tl.size.Y + rulerH + row.y - tl.scrollY + row.pad + float32(ref.Lane)*row.lh
		x0 = max(x0, float64(s.labelW())-4)
		x1 = min(x1, float64(tl.size.W)+4)
		return ui.Rect{X: tl.size.X + float32(x0), Y: y + 1, W: float32(max(x1-x0, 1)), H: row.lh - 3}, true
	}
	return ui.Rect{}, false
}

func (s *App) paintOverlays(p *ui.Painter, r ui.Rect) {
	tl := &s.tl
	dataR := ui.Rect{X: r.X + s.labelW(), Y: r.Y + rulerH, W: r.W - s.labelW(), H: r.H - rulerH}
	p.Clip(dataR, 0, func() {
		if tl.hasRange {
			a, b := s.xAt(min(tl.rangeA, tl.rangeB)), s.xAt(max(tl.rangeA, tl.rangeB))
			p.Fill(ui.Rect{X: r.X + float32(a), Y: dataR.Y, W: float32(max(b-a, 1)), H: dataR.H}, colRange.Alpha(0.10), 0)
			p.Fill(ui.Rect{X: r.X + float32(a), Y: dataR.Y, W: 1, H: dataR.H}, colRange.Alpha(0.6), 0)
			p.Fill(ui.Rect{X: r.X + float32(b), Y: dataR.Y, W: 1, H: dataR.H}, colRange.Alpha(0.6), 0)
		}
		tl.linkChip = ui.Rect{}
		if in, has := s.selectedInfo(); has {
			sel, ok := s.boxAt(in.ref, in.start, in.end)
			if o := in.linked; o != nil {
				ob, ok2 := s.boxAt(o.ref, o.start, o.end)
				// The other end, out of the view (its row scrolled away, or
				// before or after the times shown): the line goes to the
				// view's edge, where a chip names it.
				inner := ui.Rect{X: dataR.X + 10, Y: dataR.Y + 10, W: dataR.W - 20, H: dataR.H - 20}
				x1, y1 := ob.X+min(ob.W/2, 6), ob.Y+ob.H/2
				out := ok2 && (x1 < dataR.X || x1 > dataR.X+dataR.W || y1 < dataR.Y || y1 > dataR.Y+dataR.H)
				if ok2 && ok {
					// A flow line from the API call to the GPU work.
					arrow := edgeArrow(dataR, x1, y1)
					if out {
						x1, y1 = min(max(x1, inner.X), inner.X+inner.W), min(max(y1, inner.Y), inner.Y+inner.H)
					}
					var path ui.Path
					x0, y0 := sel.X+min(sel.W/2, 6), sel.Y+sel.H/2
					my := (y0 + y1) / 2
					path.MoveTo(x0, y0).CubeTo(x0, my, x1, my, x1, y1)
					p.StrokePath(&path, 1.5, colAccentHi.Alpha(0.85))
					if out {
						s.paintLinkChip(p, dataR, x1, y1, arrow, o.name)
					}
				}
				if ok2 && !out {
					p.Stroke(grow(ob, 1.5), colAccentHi, 3, 1.5)
				}
				if ok && !ok2 {
					// Not laid out (a lane off in the overview, a row
					// hidden, a trace left out): a chip by the selection.
					s.paintLinkChip(p, dataR, sel.X+sel.W/2, sel.Y-14, "↗", o.name)
				}
			}
			if ok {
				p.Shadow(sel, 3, 0, 0, 8, 1, colSelection.Alpha(0.35))
				p.Stroke(grow(sel, 1.5), colSelection, 3, 2)
			}
		}
		if tl.hovering && !tl.dragging && float64(tl.mouseX) > float64(s.labelW()) && tl.mouseY > rulerH {
			x := r.X + tl.mouseX
			p.Fill(ui.Rect{X: x, Y: dataR.Y, W: 1, H: dataR.H}, colText.Alpha(0.25), 0)
			if h := s.hitAt(tl.mouseX, tl.mouseY); h.ok && h.near == 1 {
				if b, ok := s.boxAt(h.ref, h.b.start, h.b.end); ok {
					p.Stroke(grow(b, 1), colText.Alpha(0.8), 3, 1)
				}
			}
		}
	})
}

func grow(r ui.Rect, d float32) ui.Rect {
	return ui.Rect{X: r.X - d, Y: r.Y - d, W: r.W + 2*d, H: r.H + 2*d}
}

func (s *App) paintRuler(p *ui.Painter, r ui.Rect, step float64) {
	tl := &s.tl
	bar := ui.Rect{X: r.X, Y: r.Y, W: r.W, H: rulerH}
	p.Fill(bar, colRuler, 0)
	p.Fill(ui.Rect{X: r.X, Y: r.Y + rulerH - 1, W: r.W, H: 1}, colBorder, 0)
	dataR := ui.Rect{X: r.X + s.labelW(), Y: r.Y, W: r.W - s.labelW(), H: rulerH}
	p.Clip(dataR, 0, func() {
		view := tl.cur
		minor := step / 5
		first := math.Floor(view.start/minor) * minor
		for i := 0; ; i++ {
			t := first + float64(i)*minor
			x := float32(s.xAt(t))
			if x > r.W+2 {
				break
			}
			major := math.Abs(math.Remainder(t, step)) < minor/2
			if major {
				p.Fill(ui.Rect{X: r.X + x, Y: r.Y + rulerH - 10, W: 1, H: 10}, colMuted, 0)
				if x >= s.labelW() {
					p.RichText(r.X+x+4, r.Y+5, 0, ui.Span{Text: fmtTick(t, step), Size: 10.5, Color: colMuted})
				}
			} else {
				p.Fill(ui.Rect{X: r.X + x, Y: r.Y + rulerH - 5, W: 1, H: 5}, colFaint, 0)
			}
		}
		if tl.hasRange {
			a, b := s.xAt(min(tl.rangeA, tl.rangeB)), s.xAt(max(tl.rangeA, tl.rangeB))
			band := ui.Rect{X: r.X + float32(a), Y: r.Y + 2, W: float32(max(b-a, 1)), H: rulerH - 4}
			p.Fill(band, colRange.Alpha(0.25), 3)
			// The span, and an × that clears the range.
			label := ui.Span{Text: fmtDur(math.Abs(tl.rangeB - tl.rangeA)), Size: 11, Weight: 600, Color: colText}
			w, h := p.MeasureText(0, label)
			lx := max(band.X+(band.W-w-18)/2, band.X+4)
			p.RichText(lx, band.Y+(band.H-h)/2, 0, label)
			cx, cy := lx+w+11, band.Y+band.H/2
			hot := tl.hovering && tl.rangeClose.Contains(r.X+tl.mouseX, r.Y+tl.mouseY)
			alpha := float32(0.3)
			if hot {
				alpha = 0.55
			}
			tl.rangeClose = ui.Rect{X: cx - 9, Y: cy - 9, W: 18, H: 18}
			p.Fill(ui.Rect{X: cx - 7, Y: cy - 7, W: 14, H: 14}, colRange.Alpha(alpha), 7)
			p.StrokePolyline([][2]float32{{cx - 3, cy - 3}, {cx + 3, cy + 3}}, 1.4, colText)
			p.StrokePolyline([][2]float32{{cx + 3, cy - 3}, {cx - 3, cy + 3}}, 1.4, colText)
		} else {
			tl.rangeClose = ui.Rect{}
		}
		if tl.hovering && float64(tl.mouseX) > float64(s.labelW()) {
			x := r.X + tl.mouseX
			label := ui.Span{Text: fmtTime(s.timeAtX(tl.mouseX), view.scale), Size: 10.5, Weight: 600, Color: colInverseText}
			w, h := p.MeasureText(0, label)
			bx := min(x+1, r.X+r.W-w-10)
			p.Fill(ui.Rect{X: bx, Y: r.Y + 4, W: w + 10, H: h + 4}, colInverse, 4)
			p.RichText(bx+5, r.Y+6, 0, label)
		}
	})
	// The corner: the view's span.
	p.Fill(ui.Rect{X: r.X, Y: r.Y, W: s.labelW(), H: rulerH - 1}, colPanel, 0)
	p.Fill(ui.Rect{X: r.X + s.labelW() - 1, Y: r.Y, W: 1, H: rulerH}, colBorder, 0)
	span := tl.cur.scale * (float64(r.W) - float64(s.labelW()))
	p.RichText(r.X+12, r.Y+8, 0,
		ui.Span{Text: "View ", Size: 11, Color: colFaint},
		ui.Span{Text: fmtDur(span), Size: 11, Weight: 600, Color: colMuted})
}

func (s *App) paintTooltip(p *ui.Painter, r ui.Rect) {
	tl := &s.tl
	h := s.hitAt(tl.mouseX, tl.mouseY)
	if !h.ok {
		return
	}
	b := h.b
	tr := s.tr.Tracks[h.ref.Track]
	name := b.name
	if len(name) > 90 {
		name = name[:87] + "…"
	}
	type line struct{ spans []ui.Span }
	var lines []line
	col := s.pal.of(b.name, b.kind)
	if h.near > 1 {
		lines = append(lines,
			line{[]ui.Span{{Text: fmtCount(h.near) + " events", Size: 12.5, Weight: 600, Color: colText}}},
			line{[]ui.Span{{Text: "busy " + trimFloat(100*float64(h.busy)/float64(max(h.t1-h.t0, 1)), 1) + "% of " + fmtDur(float64(h.t1-h.t0)), Size: 11.5, Color: colMuted}}},
			line{[]ui.Span{{Text: "nearest: ", Size: 11.5, Color: colFaint}, {Text: name, Size: 11.5, Color: colMuted}}},
			line{[]ui.Span{{Text: "zoom in to tell them apart", Size: 11, Color: colFaint}}},
		)
	} else {
		lines = append(lines,
			line{[]ui.Span{{Text: name, Size: 12.5, Weight: 600, Color: colText}}},
			line{[]ui.Span{{Text: b.kind.String(), Size: 11.5, Color: col}, {Text: "  ·  " + tr.Name, Size: 11.5, Color: colFaint}}},
			line{[]ui.Span{{Text: "Duration  ", Size: 11.5, Color: colFaint}, {Text: fmtDur(float64(b.end - b.start)), Size: 11.5, Weight: 600, Color: colText}}},
			line{[]ui.Span{{Text: "Start  ", Size: 11.5, Color: colFaint}, {Text: fmtTime(float64(b.start), 0.1), Size: 11.5, Color: colMuted}}},
		)
		if n, ok := s.src.memBytes(h.ref); ok {
			lines = append(lines, line{[]ui.Span{{Text: "Size  ", Size: 11.5, Color: colFaint}, {Text: fmtBytes(n), Size: 11.5, Color: colMuted}}})
		}
	}
	var w, hgt float32
	sizes := make([][2]float32, len(lines))
	for i, l := range lines {
		lw, lh := p.MeasureText(0, l.spans...)
		sizes[i] = [2]float32{lw, lh}
		w = max(w, lw)
		hgt += lh + 2
	}
	w += 22
	hgt += 14
	x := r.X + tl.mouseX + 16
	y := r.Y + tl.mouseY + 18
	if x+w > r.X+r.W-4 {
		x = r.X + tl.mouseX - w - 12
	}
	if y+hgt > r.Y+r.H-4 {
		y = r.Y + tl.mouseY - hgt - 10
	}
	box := ui.Rect{X: x, Y: y, W: w, H: hgt}
	p.Shadow(box, 8, 0, 6, 18, 0, colShadow)
	p.Fill(box, colTooltip, 8)
	p.Stroke(box, colBorder, 8, 1)
	p.Fill(ui.Rect{X: x + 1, Y: y + 8, W: 3, H: hgt - 16}, col, 1.5)
	ly := y + 7
	for i, l := range lines {
		p.RichText(x+12, ly, 0, l.spans...)
		ly += sizes[i][1] + 2
	}
}
