package viewer

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// The rank overview: many traces (the ranks of a job) side by side, each a
// block of a busy/idle strip and a summary lane per kind of row (GPU, CUDA
// API, NVTX), every row of that kind drawn into it, all on the session's
// time axis, so that a rank that runs late or waits stands out. The trace's
// name stays in the label gutter as its block scrolls. C toggles it; it is
// on by itself with more than theme.CompactAuto traces (Settings.Compact).

// rankSpan is a trace's block of rows in the rank overview.
type rankSpan struct {
	label  string
	host   string
	y0, y1 float32
	// idle is the share of the view inside the trace that its GPUs were
	// idle, as its strip last painted.
	idle float64
}

// rankInfo is a trace of what is shown: its tracks and where it lies, and
// which part of the session it is.
type rankInfo struct {
	label, host string
	first, n    int
	offset, dur int64
	part        int
}

func (s *App) rankInfos() []rankInfo {
	if ss, ok := s.src.(*sessionSource); ok {
		var out []rankInfo
		for i, p := range ss.parts {
			if ss.partOff(i) {
				continue // left out (traces.go)
			}
			// The label names the host too ("rank3 (node0)"): apart here.
			name := p.label
			if p.tr.Host != "" {
				name = strings.TrimSuffix(name, " ("+p.tr.Host+")")
			}
			out = append(out, rankInfo{name, p.tr.Host, p.first, len(p.tr.Tracks), p.offset, p.tr.Duration(), i})
		}
		return out
	}
	base := filepath.Base(s.tr.Path)
	return []rankInfo{{strings.TrimSuffix(base, filepath.Ext(base)), s.tr.Host, 0, len(s.tr.Tracks), 0, s.tr.Duration(), 0}}
}

// compactOn reports whether the rank overview shows.
func (s *App) compactOn() bool {
	switch s.ctl.settings.Compact {
	case "on":
		return true
	case "off":
		return false
	}
	if s.tr == nil {
		return false
	}
	ss, ok := s.src.(*sessionSource)
	return ok && ss.shownParts() > theme.CompactAuto
}

// setCompact sets the density: "auto", "on" or "off".
func (s *App) setCompact(mode string) {
	if mode == "auto" {
		mode = ""
	}
	s.ctl.settings.Compact = mode
	s.tl.dirty = true
	s.settingsChanged()
}

// toggleCompact switches between the compact and the full view (C).
func (s *App) toggleCompact() {
	if s.compactOn() {
		s.setCompact("off")
	} else {
		s.setCompact("on")
	}
}

// SetCompact sets the density ("auto", "on", "off"), as the sidebar does.
func (s *App) SetCompact(mode string) { s.setCompact(mode) }

// lanesOn are the summary lanes shown.
func (s *App) lanesOn() []string {
	if l := s.ctl.settings.CompactLanes; len(l) > 0 {
		return l
	}
	return theme.CompactDefault
}

func trackKindKey(k trace.TrackKind) string {
	switch k {
	case trace.TrackStream:
		return "stream"
	case trace.TrackRuntime:
		return "api"
	case trace.TrackNVTX:
		return "nvtx"
	case trace.TrackCPU:
		return "cpu"
	case trace.TrackPython:
		return "python"
	}
	return ""
}

// layoutCompact lays out the rank overview (compactLayout, as the browser).
func (s *App) layoutCompact() {
	tl := &s.tl
	tl.ranks = tl.ranks[:0]
	ranks := s.rankInfos()
	subs := make([]map[string]int, len(ranks))
	byKind := make([]map[string][]int, len(ranks))
	// slots are the sub-lanes in use of each kind, in order; tops those
	// where a thread's band of nested ranges starts.
	slots := make([]map[string][]int, len(ranks))
	tops := make([]map[string][]int, len(ranks))
	for ri, rk := range ranks {
		byKind[ri], subs[ri], slots[ri], tops[ri] = map[string][]int{}, map[string]int{}, map[string][]int{}, map[string][]int{}
		for i := rk.first; i < rk.first+rk.n; i++ {
			if s.hidden[i] {
				continue
			}
			k := trackKindKey(s.tr.Tracks[i].Kind)
			byKind[ri][k] = append(byKind[ri][k], i)
			for li := range s.tr.Tracks[i].Lanes {
				sl := s.slotOf(i, li)
				if sl >= 0 && !slices.Contains(slots[ri][k], sl) {
					slots[ri][k] = append(slots[ri][k], sl)
				}
				if sl >= 0 && li == 0 && k != "stream" {
					tops[ri][k] = append(tops[ri][k], sl)
				}
			}
		}
		for k, m := range byKind[ri] {
			slices.Sort(slots[ri][k])
			subs[ri][k] = max(len(slots[ri][k]), min(len(m), 1))
		}
	}
	rows, height := compactLayout(subs, s.lanesOn())
	band, prev := 0, ""
	for _, cr := range rows {
		r := row{rank: cr.Trace, key: cr.Key, sub: cr.Sub, y: float32(cr.Y), h: float32(cr.H)}
		if cr.Key == "" {
			r.kind = rowStrip
			r.members = byKind[cr.Trace]["stream"]
			tl.ranks = append(tl.ranks, rankSpan{label: ranks[cr.Trace].label, host: ranks[cr.Trace].host, y0: r.y})
			band, prev = -1, ""
		} else {
			r.kind = rowSummary
			for _, l := range theme.CompactLanes {
				if l.Key == cr.Key {
					r.members = byKind[cr.Trace][l.Kind]
					if sl := slots[cr.Trace][l.Kind]; cr.Sub < len(sl) {
						r.slot = sl[cr.Sub]
					}
					r.top = cr.Sub > 0 && slices.Contains(tops[cr.Trace][l.Kind], r.slot)
				}
			}
			if cr.Key != prev {
				band, prev = band+1, cr.Key
			}
			r.band = band
		}
		tl.ranks[len(tl.ranks)-1].y1 = r.y + r.h
		tl.rows = append(tl.rows, r)
	}
	tl.contentH = float32(height)
	tl.dirty = false
}

// slotOf is the sub-lane of its summary lane that lane li of a track
// draws into (laneSlot), -1 for none.
func (s *App) slotOf(track, li int) int {
	tk := s.tr.Tracks[track]
	return laneSlot(trackKindKey(tk.Kind), len(tk.Lanes), tk.Overview, li, !s.ctl.settings.CompactOverlaid)
}

// setOverlaid draws the rows of a kind over each other (true) or stacks
// what runs in parallel (false).
func (s *App) setOverlaid(on bool) {
	s.ctl.settings.CompactOverlaid = on
	s.tl.dirty = true
	s.settingsChanged()
}

func compactLabel(key string) string {
	for _, l := range theme.CompactLanes {
		if l.Key == key {
			return l.Label
		}
	}
	return key
}

// paintSummary draws a sub-lane of a summary lane: the lanes of the rows
// of its kind that draw into it (slotOf), overlaid.
func (s *App) paintSummary(p *ui.Painter, r ui.Rect, y float32, ri int, row *row) {
	tl := &s.tl
	if row.band%2 == 0 {
		p.Fill(ui.Rect{X: r.X + s.labelW(), Y: y, W: r.W - s.labelW(), H: row.h}, colRowAlt, 0)
	}
	switch {
	case row.top: // another thread's band of nested ranges
		p.Fill(ui.Rect{X: r.X + s.labelW(), Y: y, W: r.W - s.labelW(), H: 1}, colBorder, 0)
	case row.sub > 0:
		p.Fill(ui.Rect{X: r.X + s.labelW(), Y: y, W: r.W - s.labelW(), H: 1}, colBorder.Alpha(0.5), 0)
	}
	dataX := float64(r.X) + float64(s.labelW())
	dataW := float64(r.W) - float64(s.labelW())
	view := tl.cur
	t0, t1 := view.start, view.start+dataW*view.scale
	// The blocks as in the full view (theme.CompactLaneH is its lane's
	// height), names inside where they fit. Every row of the kind draws
	// here, over each other: the merged and the narrow blocks go into a
	// column a pixel, each keeping the strongest, painted as runs (a
	// column's worth, whatever the rows); the wide ones on top.
	n := max(int(math.Ceil(dataW)), 0)
	if len(tl.colA) != n {
		tl.colA, tl.colC = make([]float32, n), make([]ui.Color, n)
	}
	tl.wide = tl.wide[:0]
	for _, ti := range row.members {
		for li := range s.tr.Tracks[ti].Lanes {
			if s.slotOf(ti, li) != row.slot {
				continue
			}
			tl.blocks = s.src.lod(ti, li, t0, t1, view.scale, tl.blocks[:0])
			s.perf.blocks += len(tl.blocks)
			for _, b := range tl.blocks {
				x0 := (float64(b.start) - view.start) / view.scale
				x1 := (float64(b.end) - view.start) / view.scale
				if !b.merged && x1-x0 >= 3 || b.kind == trace.KindNVTXMark && !b.merged {
					tl.wide = append(tl.wide, b)
					continue
				}
				col := s.pal.of(b.name, b.kind)
				dim := s.highlight != "" && (b.merged || b.name != s.highlight)
				a := float32(1)
				switch {
				case b.merged:
					a = float32(0.3 + 0.6*b.density())
					col = col.Mix(colBg, 0.3)
				case dim:
					col = col.Mix(colBg, 0.75)
				}
				if b.merged && dim {
					a *= 0.3
				}
				for c := max(int(math.Floor(x0)), 0); c < min(int(math.Ceil(max(x1, x0+1))), n); c++ {
					if a > tl.colA[c] {
						tl.colA[c], tl.colC[c] = a, col
					}
				}
			}
		}
	}
	for c := 0; c < n; {
		a := tl.colA[c]
		if a == 0 {
			c++
			continue
		}
		col := tl.colC[c]
		e := c + 1
		for e < n && tl.colA[e] == a && tl.colC[e] == col {
			e++
		}
		p.Fill(ui.Rect{X: float32(dataX) + float32(c), Y: y + 1, W: float32(e - c), H: row.h - 3}, col.Alpha(a), 0)
		c = e
	}
	clear(tl.colA)
	s.paintBlocks(p, tl.wide, y, theme.CompactLaneH, dataX, dataW)
	s.perf.blocks -= len(tl.wide) // counted above
	s.paintCompactGutter(p, r, y, row.h)
	if row.sub > 0 {
		return
	}
	tag := ui.Span{Text: compactLabel(row.key), Size: 11, Color: colMuted}
	w, h := p.MeasureText(0, tag)
	p.RichText(r.X+s.labelW()-10-w, y+(row.h-h)/2, 0, tag)
}

func (s *App) paintCompactGutter(p *ui.Painter, r ui.Rect, y, h float32) {
	p.Fill(ui.Rect{X: r.X, Y: y, W: s.labelW(), H: h}, colPanel, 0)
	p.Fill(ui.Rect{X: r.X + s.labelW() - 1, Y: y, W: 1, H: h}, colBorder, 0)
}

// rankLoading is a rank's idle share while its tiles of the view have not
// come: it says "loading…", not that its GPUs idle.
const rankLoading = -2

// partWaiting reports whether the part of the session (or the trace) is
// streamed and the tiles of the last frame have not all come.
func (s *App) partWaiting(part int) bool {
	src := s.src
	if ss, ok := src.(*sessionSource); ok {
		if part < 0 || part >= len(ss.parts) {
			return false
		}
		src = ss.parts[part].src
	}
	rs, ok := src.(*remoteSource)
	if !ok {
		return false
	}
	_, complete, _ := rs.status()
	return !complete
}

// viewLoading reports whether what the view shows is still coming from a
// server: tiles of it asked for, or not all come.
func (s *App) viewLoading() bool {
	if s.src == nil {
		return false
	}
	if s.src.loading() > 0 {
		return true
	}
	if ss, ok := s.src.(*sessionSource); ok {
		for i := range ss.parts {
			if !ss.partOff(i) && s.partWaiting(i) {
				return true
			}
		}
		return false
	}
	return s.partWaiting(0)
}

// paintLoadingBar draws, along the top of the timeline under the ruler, a
// segment going back and forth while the view's tiles stream: the view is
// not complete yet (as the loading screen said of the traces before).
func (s *App) paintLoadingBar(p *ui.Painter, r ui.Rect) {
	x0, w := r.X+s.labelW(), r.W-s.labelW()
	if w <= 0 {
		return
	}
	y := r.Y + rulerH
	p.Fill(ui.Rect{X: x0, Y: y, W: w, H: 3}, colAccent.Alpha(0.15), 0)
	t := float64(p.Now().UnixMilli()%1400) / 1400 // a pass a 1.4 s
	seg := w / 4
	x := x0 - seg + float32(t)*(w+seg)
	p.Clip(ui.Rect{X: x0, Y: y, W: w, H: 3}, 0, func() {
		p.Fill(ui.Rect{X: x, Y: y, W: seg, H: 3}, colAccent, 1.5)
	})
	// The next step in loadingBarStep, not the next frame: a bar this thin
	// moves smoothly enough at 20 steps a second, and a frame of the whole
	// timeline at the display's rate while a server is slow would spend a
	// core on waiting. (0.6.4 at first asked for every frame.)
	p.After(loadingBarStep)
}

// loadingBarStep is how often the loading bar moves.
const loadingBarStep = 50 * time.Millisecond

// paintStrip draws a trace's busy/idle strip: the share of each pixel
// column its GPUs are busy (busyColumns), idle columns inside the trace
// tinted, so that gaps stand out from rank to rank.
func (s *App) paintStrip(p *ui.Painter, r ui.Rect, y float32, row *row) {
	tl := &s.tl
	rk := s.rankInfos()[row.rank]
	dataX := float64(r.X) + float64(s.labelW())
	dataW := float64(r.W) - float64(s.labelW())
	view := tl.cur
	t0, t1 := view.start, view.start+dataW*view.scale
	tl.busyIn = tl.busyIn[:0]
	for _, ti := range row.members {
		for li := range s.tr.Tracks[ti].Lanes {
			tl.blocks = s.src.lod(ti, li, t0, t1, view.scale, tl.blocks[:0])
			for _, b := range tl.blocks {
				tl.busyIn = append(tl.busyIn, float64(b.start), float64(b.end), float64(b.busy))
			}
		}
	}
	n := int(dataW)
	tl.busy = busyColumns(tl.busyIn, view.start, view.scale, n, tl.busy)
	// The columns inside the trace.
	c0 := max(int(math.Ceil((float64(rk.offset)-view.start)/view.scale)), 0)
	c1 := min(int(math.Floor((float64(rk.offset+rk.dur)-view.start)/view.scale)), n-1)
	idle, inside := 0, 0
	idleCol := colWarn.Alpha(0.85)
	// Streamed, its tiles of the view not come yet: nothing says it idles.
	if s.partWaiting(rk.part) && !slices.ContainsFunc(tl.busy[max(c0, 0):max(c1+1, max(c0, 0))], func(v float64) bool { return v > 0 }) {
		if c1 >= c0 {
			p.Fill(ui.Rect{X: float32(dataX) + float32(c0), Y: y, W: float32(c1 - c0 + 1), H: row.h - 1}, colFaint.Alpha(0.18), 0)
		}
		tl.ranks[row.rank].idle = rankLoading
		s.paintCompactGutter(p, r, y, row.h)
		if row.rank > 0 {
			p.Fill(ui.Rect{X: r.X, Y: y - theme.CompactGap/2, W: r.W, H: 1}, colBorder, 0)
		}
		return
	}
	for c := c0; c <= c1; {
		v := tl.busy[c]
		level := 0
		if v >= theme.CompactIdle {
			level = 1 + int(v*7)
		}
		e := c + 1
		for e <= c1 {
			w := tl.busy[e]
			l := 0
			if w >= theme.CompactIdle {
				l = 1 + int(w*7)
			}
			if l != level {
				break
			}
			e++
		}
		box := ui.Rect{X: float32(dataX) + float32(c), Y: y, W: float32(e - c), H: row.h - 1}
		if level == 0 {
			idle += e - c
			p.Fill(box, idleCol, 0)
		} else {
			p.Fill(box, colAccent.Alpha(0.12+0.4*float32(level)/8), 0)
		}
		inside += e - c
		c = e
	}
	if inside > 0 {
		tl.ranks[row.rank].idle = float64(idle) / float64(inside)
	} else {
		tl.ranks[row.rank].idle = -1
	}
	s.paintCompactGutter(p, r, y, row.h)
	// The separator from the trace above, in the middle of the gap.
	if row.rank > 0 {
		p.Fill(ui.Rect{X: r.X, Y: y - theme.CompactGap/2, W: r.W, H: 1}, colBorder, 0)
	}
}

// paintRankLabels writes each trace's name in the gutter of its block, kept
// in view while any of the block is.
func (s *App) paintRankLabels(p *ui.Painter, r ui.Rect) {
	tl := &s.tl
	top := r.Y + rulerH
	for _, rk := range tl.ranks {
		y0, y1 := top+rk.y0-tl.scrollY, top+rk.y1-tl.scrollY
		if y1 < top || y0 > r.Y+r.H {
			continue
		}
		// The name as track names are (12 px), then the host and how idle
		// its GPUs are in the view, while the block has room.
		name := ui.Span{Text: rk.label, Size: 12, Weight: 600, Color: colText}
		_, nh := p.MeasureText(0, name)
		ly := min(max(y0+theme.CompactStripH+2, top+2), y1-nh)
		p.Clip(ui.Rect{X: r.X + 8, Y: max(y0, top), W: s.labelW() - 84, H: y1 - max(y0, top)}, 0, func() {
			p.RichText(r.X+12, ly, 0, name)
			var sub []ui.Span
			if rk.host != "" {
				sub = append(sub, ui.Span{Text: rk.host, Size: 10.5, Color: colFaint})
			}
			if rk.idle == rankLoading {
				if sub != nil {
					sub = append(sub, ui.Span{Text: " · ", Size: 10.5, Color: colFaint})
				}
				sub = append(sub, ui.Span{Text: "loading…", Size: 10.5, Color: colFaint})
			}
			if rk.idle >= 0 {
				col := colFaint
				if rk.idle > 0.2 {
					col = colWarn
				}
				if sub != nil {
					sub = append(sub, ui.Span{Text: " · ", Size: 10.5, Color: colFaint})
				}
				sub = append(sub, ui.Span{Text: "GPU idle " + trimFloat(100*rk.idle, 0) + "%", Size: 10.5, Color: col})
			}
			if sub != nil && y1-ly >= nh+12 {
				p.RichText(r.X+12, ly+nh, 0, sub...)
			}
		})
	}
}

// hitSummary finds the event under x in a summary lane: the nearest of its
// rows' events.
func (s *App) hitSummary(ri int, x float32) hit {
	tl := &s.tl
	row := &tl.rows[ri]
	scale := tl.cur.scale
	t := s.timeAtX(x)
	best := hit{row: ri}
	bestD := math.Inf(1)
	for _, ti := range row.members {
		for li := range s.tr.Tracks[ti].Lanes {
			if s.slotOf(ti, li) != row.slot {
				continue
			}
			b, ok := s.src.hit(ti, li, t, 3*scale, scale)
			if !ok {
				continue
			}
			d := 0.0
			if t < float64(b.start) {
				d = float64(b.start) - t
			} else if t > float64(b.end) {
				d = t - float64(b.end)
			}
			if d <= bestD { // the later row is drawn on top
				bestD = d
				best = hit{ok: true, row: ri, ref: trace.Ref{Track: int32(ti), Lane: int32(li), Index: int32(b.index)},
					near: max(b.count, 1), busy: b.busy, t0: b.start, t1: b.end, b: b}
			}
		}
	}
	return best
}

// setLane shows or hides a summary lane of every trace (at least one
// stays).
func (s *App) setLane(key string, show bool) {
	on := s.lanesOn()
	var next []string
	for _, m := range theme.CompactLanes {
		if (m.Key == key && show) || (m.Key != key && slices.Contains(on, m.Key)) {
			next = append(next, m.Key)
		}
	}
	if len(next) == 0 {
		next = []string{"gpu"}
	}
	s.ctl.settings.CompactLanes = next
	s.tl.dirty = true
	s.settingsChanged()
}

var compactModes = []string{"", "on", "off"}

// compactControl chooses the density and the summary lanes.
func (s *App) compactControl(c *ui.Context) {
	ctl := s.ctl
	i := 0
	for k, m := range compactModes {
		if m == ctl.settings.Compact {
			i = k
		}
	}
	if ui.Segmented(c.Key("density"), &i, "Auto", "Overview", "Full").Label("Density").Changed() {
		s.setCompact(compactModes[i])
	}
	note := "Rank overview with more than " + fmt.Sprint(theme.CompactAuto) + " traces · C toggles"
	ui.Text(c, note).FontSize(11.5).TextColor(colFaint)
	if !s.compactOn() {
		return
	}
	on := s.lanesOn()
	ui.Row(c).Gap(12).Wrap().Children(func() {
		for _, l := range theme.CompactLanes {
			checked := slices.Contains(on, l.Key)
			if ui.Checkbox(c.Key("lane:"+l.Key), &checked, l.Label).Changed() {
				s.setLane(l.Key, checked)
			}
		}
	})
	j := 0
	if s.ctl.settings.CompactOverlaid {
		j = 1
	}
	if ui.Segmented(c.Key("parallel"), &j, "Stacked", "Overlaid").Label("Parallel work").Changed() {
		s.setOverlaid(j == 1)
	}
}

// alignControl aligns the traces of a session (alignSession): on their
// collective kernels (the default), their captures' clocks, or their first
// events; and says what each trace's place rests on.
func (s *App) alignControl(c *ui.Context) {
	ss, ok := s.src.(*sessionSource)
	if !ok {
		return
	}
	modes := []string{"collectives", "abs", "start"}
	i := slices.Index(modes, ss.alignMode())
	if ui.Segmented(c.Key("align"), &i, "Collectives", "Clocks", "Starts").Label("Align").Changed() {
		s.SetAlign(modes[i])
	}
	ui.Text(c, map[string]string{
		"collectives": "Collective kernels (all-reduce, all-gather, ...) end together on every rank: lined up on them, to microseconds; clocks where too few match",
		"abs":         "At the moment each event happened, by the captures' clocks (hosts' clocks differ by up to milliseconds)",
		"start":       "Every trace's first event at 0",
	}[ss.alignMode()]).FontSize(11.5).TextColor(colFaint).MaxWidth(240)
	if ss.alignMode() == "start" {
		return
	}
	// The traces lined up on collectives in a line, the others each.
	var coll []*part
	var ref *part
	for _, p := range ss.parts {
		switch {
		case p.align.Ref:
			ref = p
		case p.align.Basis == "collectives":
			coll = append(coll, p)
		}
	}
	if len(coll) > 0 && ref != nil {
		lo, hi := coll[0].align.Matches, coll[0].align.Matches
		var sp []float64
		for _, p := range coll {
			lo, hi = min(lo, p.align.Matches), max(hi, p.align.Matches)
			sp = append(sp, p.align.Spread)
		}
		slices.Sort(sp)
		ui.Text(c, fmt.Sprintf("%d traces lined up on their collective kernels: %d–%d matched each, ends within ±%s (worst ±%s), against %s",
			len(coll)+1, lo, hi, fmtDur(sp[len(sp)/2]), fmtDur(sp[len(sp)-1]), ref.label)).FontSize(11.5).TextColor(colFaint).MaxWidth(240)
	}
	for _, p := range ss.parts {
		col := colFaint
		var how string
		switch a := p.align; a.Basis {
		case "collectives":
			continue
		case "wall":
			how = "its wall clock"
		case "host":
			how = "its host's clock, against a trace of the same host"
		case "monotonic":
			how, col = "its host's clock only, among the host's traces: not placed against the others", colWarn
			if p.tr.Clock != "" && p.host == "" {
				how, col = "the clock of the file it was merged into, with the other ranks of that file", colFaint
			}
		default:
			how, col = "no clock: at its start", colWarn
		}
		ui.Text(c, p.label+": "+how).FontSize(11.5).TextColor(col).MaxLines(3).MaxWidth(240)
	}
}

// SetAlign aligns the traces of a session: "collectives", "abs" (their
// clocks) or "start" (their first events).
func (s *App) SetAlign(mode string) {
	ss, ok := s.src.(*sessionSource)
	if !ok || ss.alignMode() == mode {
		return
	}
	ss.mode = mode
	ss.align(s.tr)
	s.tl.dirty = true
	s.tl.cur, s.tl.tgt = s.clampView(s.tl.cur), s.clampView(s.tl.tgt)
}

// SetAlignStarts aligns the traces on their first events (true) or on
// their collectives and clocks.
func (s *App) SetAlignStarts(starts bool) {
	if starts {
		s.SetAlign("start")
	} else {
		s.SetAlign("collectives")
	}
}
