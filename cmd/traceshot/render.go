package main

import (
	"cmp"
	"image"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// The picture: a header (title, window, what is shown), the time ruler,
// then each rank's block, and a legend of what took the time.
//
// A rank's block has, in the "rows" layout, its rows as the viewer shows
// them (GPU streams, a thread's NVTX ranges, Python calls, CPU operators
// and CUDA API calls), under their heading (GPU 0, Process 42); in the
// "summary" layout, as the viewer's rank overview, a busy/idle strip of
// its GPUs and a summary lane per kind of row, every row of that kind
// drawn into a few sub-lanes, what runs at once side by side.

// Metrics, in DIPs.
const (
	headerH  = 62
	rulerH   = 30
	rankH    = 30 // a rank's heading
	groupH   = 20 // a heading of rows (GPU 0, a process) in the rows layout
	stripH   = theme.CompactStripH
	kindGap  = theme.CompactKindGap
	rankGap  = 10
	marginB  = 14
	legendRH = 20 // a legend's row
)

// kinds are the kinds of rows, by the keys -rows takes, in the order a
// rank's summary lanes go (theme.CompactLanes).
var kinds = []struct {
	key, label string
	kind       trace.TrackKind
}{
	{"gpu", "GPU", trace.TrackStream},
	{"nvtx", "NVTX", trace.TrackNVTX},
	{"python", "Python", trace.TrackPython},
	{"cpu", "CPU ops", trace.TrackCPU},
	{"api", "CUDA API", trace.TrackRuntime},
}

func keyOf(k trace.TrackKind) string {
	for _, kd := range kinds {
		if kd.kind == k {
			return kd.key
		}
	}
	return ""
}

// laneOf is the height of a lane of rows of kind k and the space above and
// below their lanes: GPU streams and CUDA API calls are dense, as in the
// viewer.
func laneOf(k trace.TrackKind) (lh, pad float32) {
	if k == trace.TrackStream || k == trace.TrackRuntime {
		return theme.DenseLaneH, theme.DensePad
	}
	return theme.LaneH, theme.TrackPad
}

type options struct {
	t0, t1    int64 // the window, on the session's axis
	width     float32
	scale     float32
	gutter    float32
	pal       *theme.Palette
	layout    string // "rows" or "summary"
	kinds     []string
	match     *regexp.Regexp // rows to keep (by "heading · name"), nil for all
	skip      *regexp.Regexp // rows to leave out
	highlight *regexp.Regexp // events to stand out, the others dimmed
	depth     int            // the most lanes (nesting depths) of a row
	legend    int            // names in the legend
	title     string
	maxH      float32
	empty     bool // keep rows without events in the window
	mark      [][2]int64
}

type itemKind uint8

const (
	itemRank itemKind = iota
	itemGroup
	itemTrack
	itemStrip
	itemSummary
	itemMore
)

// item is a row of the picture.
type item struct {
	kind  itemKind
	y, h  float32
	r     *rank
	label string
	info  string
	// track and lanes are the row and those of its lanes drawn (itemTrack).
	track int
	lanes []int
	// refs are the lanes drawn into a summary lane's sub-lane.
	refs     []trace.LaneRef
	lh, pad  float32
	swatch   trace.TrackKind
	alt      bool
	sepAbove bool
}

// picture is the laid out picture.
type picture struct {
	o      *options
	ss     *session
	items  []item
	rowsH  float32 // the height of the rows, under the ruler
	legend []legendEntry
	legH   float32
	h      float32
	cut    int // rows left out for the height
	busy   map[*rank]float64
	colors map[colorKey]ui.Color
}

type colorKey struct {
	name string
	kind trace.Kind
}

func (pc *picture) color(name string, k trace.Kind) ui.Color {
	key := colorKey{name, k}
	c, ok := pc.colors[key]
	if !ok {
		c = pc.o.pal.EventColor(name, k)
		pc.colors[key] = c
	}
	return c
}

// visible counts the events of lane l in the window, at most 2.
func (o *options) visible(r *rank, l *trace.Lane) int {
	i, j := l.Visible(r.toTrace(o.t0), r.toTrace(o.t1))
	return min(j-i, 2)
}

func (o *options) keepTrack(tk *trace.Track) bool {
	if !slices.Contains(o.kinds, keyOf(tk.Kind)) {
		return false
	}
	name := tk.Group + " · " + tk.Name
	if o.match != nil && !o.match.MatchString(name) {
		return false
	}
	return o.skip == nil || !o.skip.MatchString(name)
}

// trackLanes are the lanes of tk drawn in the rows layout: up to o.depth
// of them (every lane of a GPU stream: work that overlaps), without those
// empty in the window at the bottom; for Python calls, from the first
// depth with more than one call in the window, the outer ones being the
// script's own frames, each as long as the window.
func (o *options) trackLanes(r *rank, tk *trace.Track) []int {
	from := 0
	if tk.Kind == trace.TrackPython {
		for from < len(tk.Lanes)-1 && o.visible(r, tk.Lanes[from]) <= 1 {
			from++
		}
	}
	to := len(tk.Lanes)
	if tk.Kind != trace.TrackStream {
		to = min(to, from+o.depth)
	}
	if !o.empty {
		for to > from && o.visible(r, tk.Lanes[to-1]) == 0 {
			to--
		}
	}
	var out []int
	for li := from; li < to; li++ {
		out = append(out, li)
	}
	return out
}

// layout lays the picture out.
func layout(ss *session, o *options) *picture {
	pc := &picture{o: o, ss: ss, busy: map[*rank]float64{}, colors: map[colorKey]ui.Color{}}
	y := float32(0)
	for ri, r := range ss.ranks {
		pc.busy[r] = o.busyShare(r)
		head := len(pc.items)
		pc.items = append(pc.items, item{kind: itemRank, y: y, h: rankH, r: r, label: r.label, sepAbove: ri > 0})
		y += rankH
		n := 0
		if o.layout == "summary" {
			y, n = pc.layoutSummary(r, y)
		} else {
			y, n = pc.layoutRows(r, y)
		}
		pc.items[head].info = rankInfo(pc.busy[r], n, o.layout)
		if end := r.offset + r.tr.Duration(); o.t1 <= r.offset {
			pc.items[head].info = "its trace starts later, at " + fmtDur(float64(r.offset))
		} else if o.t0 >= end {
			pc.items[head].info = "its trace ended before, at " + fmtDur(float64(end))
		}
		y += rankGap
	}
	pc.rowsH = y
	// A picture taller than maxH leaves its last rows out, and says so.
	room := o.maxH - headerH - rulerH - 80
	if pc.rowsH > room {
		cut := slices.IndexFunc(pc.items, func(it item) bool { return it.y+it.h > room-24 })
		for _, it := range pc.items[cut:] {
			if it.kind == itemTrack || it.kind == itemSummary {
				pc.cut++
			}
		}
		pc.items = pc.items[:cut]
		y = room - 24
		pc.items = append(pc.items, item{kind: itemMore, y: y, h: 24})
		pc.rowsH = y + 24 + rankGap
	}
	pc.legend = pc.topNames(o.legend)
	if len(pc.legend) > 0 {
		cols := legendCols(o.width)
		pc.legH = 40 + float32((len(pc.legend)+cols-1)/cols)*legendRH
	}
	pc.h = headerH + rulerH + pc.rowsH + pc.legH + marginB
	return pc
}

func rankInfo(busy float64, rows int, layout string) string {
	s := ""
	if busy >= 0 {
		s = "GPU busy " + trimFloat(100*busy, 1) + "%"
	}
	what := "rows"
	if layout == "summary" {
		what = "lanes"
	}
	if rows == 1 {
		what = strings.TrimSuffix(what, "s")
	}
	if s != "" {
		s += "  ·  "
	}
	return s + fmtCount(rows) + " " + what
}

func (pc *picture) layoutRows(r *rank, y float32) (float32, int) {
	o := pc.o
	group := ""
	n := 0
	for ti, tk := range r.tr.Tracks {
		if !o.keepTrack(tk) {
			continue
		}
		lanes := o.trackLanes(r, tk)
		if len(lanes) == 0 {
			if !o.empty {
				continue
			}
			lanes = []int{}
		}
		if tk.Group != group {
			group = tk.Group
			pc.items = append(pc.items, item{kind: itemGroup, y: y, h: groupH, r: r, label: tk.Group})
			y += groupH
		}
		lh, pad := laneOf(tk.Kind)
		h := float32(max(len(lanes), 1))*lh + 2*pad
		pc.items = append(pc.items, item{kind: itemTrack, y: y, h: h, r: r, label: tk.Name, track: ti, lanes: lanes,
			lh: lh, pad: pad, swatch: tk.Kind, alt: n%2 == 0})
		y += h
		n++
	}
	return y, n
}

func (pc *picture) layoutSummary(r *rank, y float32) (float32, int) {
	o := pc.o
	r.tr.Overview()
	slots := r.tr.OverviewSlots()
	n := 0
	inside := o.t0 < r.offset+r.tr.Duration() && o.t1 > r.offset
	if slices.Contains(o.kinds, "gpu") && len(slots[trace.TrackStream]) > 0 && (inside || o.empty) {
		pc.items = append(pc.items, item{kind: itemStrip, y: y, h: stripH + 2, r: r})
		y += stripH + 4
	}
	first := true
	for _, kd := range kinds {
		if !slices.Contains(o.kinds, kd.key) {
			continue
		}
		subs := 0
		for _, slot := range slots[kd.kind] {
			var refs []trace.LaneRef
			busy := false
			for _, ref := range slot {
				if !o.keepTrack(r.tr.Tracks[ref.Row]) {
					continue
				}
				refs = append(refs, ref)
				busy = busy || o.visible(r, r.tr.Tracks[ref.Row].Lanes[ref.Lane]) > 0
			}
			if len(refs) == 0 || (!busy && !o.empty) {
				continue
			}
			if subs == 0 && !first {
				y += kindGap
			}
			label := ""
			if subs == 0 {
				label = kd.label
			}
			pc.items = append(pc.items, item{kind: itemSummary, y: y, h: theme.CompactLaneH, r: r, label: label, refs: refs,
				lh: theme.CompactLaneH, swatch: kd.kind, alt: n%2 == 0})
			y += theme.CompactLaneH
			subs++
			n++
		}
		if subs > 0 {
			first = false
		}
	}
	return y, n
}

// busyShare is the share of the window, inside the rank's trace, that any
// of its GPU streams is busy; -1 without streams.
func (o *options) busyShare(r *rank) float64 {
	ivs := o.gpuIntervals(r)
	if ivs == nil {
		return -1
	}
	a, b := max(o.t0, r.offset), min(o.t1, r.offset+r.tr.Duration())
	if b <= a {
		return 0
	}
	var busy int64
	for _, iv := range ivs {
		busy += min(iv[1], b) - max(iv[0], a)
	}
	return float64(busy) / float64(b-a)
}

// gpuIntervals are the times, on the session's axis, that any of r's GPU
// streams is busy in the window, merged and sorted; nil without streams.
func (o *options) gpuIntervals(r *rank) [][2]int64 {
	var ivs [][2]int64
	streams := false
	a, b := r.toTrace(o.t0), r.toTrace(o.t1)
	for _, tk := range r.tr.Tracks {
		if tk.Kind != trace.TrackStream {
			continue
		}
		streams = true
		for _, l := range tk.Lanes {
			i, j := l.Visible(a, b)
			for _, e := range l.Events[i:j] {
				ivs = append(ivs, [2]int64{max(r.toSession(e.Start), o.t0), min(r.toSession(e.End), o.t1)})
			}
		}
	}
	if !streams {
		return nil
	}
	slices.SortFunc(ivs, func(x, y [2]int64) int { return cmp.Compare(x[0], y[0]) })
	out := [][2]int64{}
	for _, iv := range ivs {
		if n := len(out); n > 0 && iv[0] <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], iv[1])
			continue
		}
		out = append(out, iv)
	}
	return out
}

// legendEntry is a name of the legend: what took the most time of its
// kind in the window, on the rows shown.
type legendEntry struct {
	name  string
	kind  trace.Kind
	count int
	total int64
	share float64 // of its group's time
}

// topNames are the n names of GPU work that took the most time in the
// window on the rows shown, clipped to it (or of the other events, where
// no GPU work shows).
func (pc *picture) topNames(n int) []legendEntry {
	if n <= 0 {
		return nil
	}
	o := pc.o
	type acc struct {
		kind  trace.Kind
		count int
		total int64
	}
	gpu, other := map[string]*acc{}, map[string]*acc{}
	var gpuSum, otherSum int64
	seen := map[[2]int]bool{}
	add := func(r *rank, ti, li int) {
		if seen[[2]int{ti, li}] {
			return
		}
		seen[[2]int{ti, li}] = true
		tr := r.tr
		l := tr.Tracks[ti].Lanes[li]
		a, b := r.toTrace(o.t0), r.toTrace(o.t1)
		i, j := l.Visible(a, b)
		m, sum := other, &otherSum
		if tr.Tracks[ti].Kind == trace.TrackStream {
			m, sum = gpu, &gpuSum
		}
		for k := i; k < j; k++ {
			e := &l.Events[k]
			d := min(e.End, b) - max(e.Start, a)
			name := tr.Name(e)
			x := m[name]
			if x == nil {
				x = &acc{kind: e.Kind}
				m[name] = x
			}
			x.count++
			x.total += d
			*sum += d
		}
	}
	for _, r := range pc.ss.ranks {
		clear(seen)
		for _, it := range pc.items {
			if it.r != r {
				continue
			}
			for _, li := range it.lanes {
				add(r, it.track, li)
			}
			for _, ref := range it.refs {
				add(r, ref.Row, ref.Lane)
			}
		}
	}
	m, sum := gpu, gpuSum
	if len(gpu) == 0 {
		m, sum = other, otherSum
	}
	var out []legendEntry
	for name, x := range m {
		out = append(out, legendEntry{name, x.kind, x.count, x.total, float64(x.total) / float64(max(sum, 1))})
	}
	slices.SortFunc(out, func(a, b legendEntry) int {
		return cmp.Or(cmp.Compare(b.total, a.total), strings.Compare(a.name, b.name))
	})
	return out[:min(n, len(out))]
}

func legendCols(width float32) int { return max(1, min(3, int(width/560))) }

// ---------------------------------------------------------------- painting

// render paints the picture into an image.
func render(pc *picture) *image.RGBA {
	o := pc.o
	tt := ui.NewTester(func(c *ui.Context) {
		ui.Box(c.Key("shot")).Fill().Draw(pc.paint)
	}, int(math.Ceil(float64(o.width))), int(math.Ceil(float64(pc.h))))
	tt.SetScale(o.scale)
	tt.SetDark(o.pal.Dark)
	tt.Frame()
	return tt.Image()
}

func hex(s string) ui.Color { return ui.Hex(s) }

// xAt is where time t of the session's axis is.
func (pc *picture) xAt(t float64) float32 {
	o := pc.o
	dataW := float64(o.width - o.gutter)
	return o.gutter + float32((t-float64(o.t0))/float64(o.t1-o.t0)*dataW)
}

func (pc *picture) nsPerPx() float64 {
	return float64(pc.o.t1-pc.o.t0) / float64(pc.o.width-pc.o.gutter)
}

func (pc *picture) paint(p *ui.Painter, r ui.Rect) {
	o, pal := pc.o, pc.o.pal
	p.Fill(r, hex(pal.Bg), 0)
	pc.paintHeader(p, r)
	top := r.Y + headerH + rulerH
	step := tickStep(pc.nsPerPx(), 110)
	body := ui.Rect{X: r.X, Y: top, W: r.W, H: pc.rowsH}
	// The gutter's panel, all the way down the rows.
	p.Fill(ui.Rect{X: r.X, Y: top, W: o.gutter, H: pc.rowsH}, hex(pal.Panel), 0)
	p.Clip(ui.Rect{X: r.X + o.gutter, Y: top, W: r.W - o.gutter, H: pc.rowsH}, 0, func() {
		pc.paintGrid(p, body, step)
	})
	for i := range pc.items {
		pc.paintItem(p, r, top, &pc.items[i])
	}
	p.Fill(ui.Rect{X: r.X + o.gutter - 1, Y: top, W: 1, H: pc.rowsH}, hex(pal.Border), 0)
	pc.paintMarks(p, body)
	pc.paintRuler(p, ui.Rect{X: r.X, Y: r.Y + headerH, W: r.W, H: rulerH}, step)
	if len(pc.legend) > 0 {
		pc.paintLegend(p, ui.Rect{X: r.X, Y: top + pc.rowsH, W: r.W, H: pc.legH})
	}
}

func (pc *picture) paintHeader(p *ui.Painter, r ui.Rect) {
	o, pal, ss := pc.o, pc.o.pal, pc.ss
	p.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W, H: headerH}, hex(pal.Panel), 0)
	p.Fill(ui.Rect{X: r.X, Y: r.Y + headerH - 1, W: r.W, H: 1}, hex(pal.Border), 0)
	// The accent's tab, as the app's brand.
	p.Fill(ui.Rect{X: r.X + 16, Y: r.Y + 14, W: 4, H: 34}, hex(pal.Accent), 2)
	title := o.title
	if title == "" {
		title = ss.name
	}
	ranks := pc.rankList()
	p.Clip(ui.Rect{X: r.X + 30, Y: r.Y, W: r.W - 200, H: headerH}, 0, func() {
		p.RichText(r.X+30, r.Y+11, 0,
			ui.Span{Text: title, Size: 16, Weight: 650, Color: hex(pal.Text)},
			ui.Span{Text: "   " + ranks, Size: 12.5, Weight: 500, Color: hex(pal.Muted)})
		parts := []string{fmtSpan(o.t0, o.t1)}
		if b := ss.basis(); b != "" {
			parts = append(parts, b)
		}
		var shown []string
		for _, kd := range kinds {
			if slices.Contains(o.kinds, kd.key) {
				shown = append(shown, kd.label)
			}
		}
		parts = append(parts, o.layout+": "+strings.Join(shown, ", "))
		if o.highlight != nil {
			parts = append(parts, "highlight /"+o.highlight.String()+"/")
		}
		p.RichText(r.X+30, r.Y+35, 0, ui.Span{Text: strings.Join(parts, "   ·   "), Size: 11.5, Color: hex(pal.Muted)})
	})
	brand := ui.Span{Text: "Tracelight", Size: 11, Weight: 600, Color: hex(pal.Faint)}
	w, _ := p.MeasureText(0, brand)
	p.RichText(r.X+r.W-w-18, r.Y+13, 0, brand)
}

// rankList says which ranks show: "ranks 0–3, 8" or "rank0".
func (pc *picture) rankList() string {
	rs := pc.ss.ranks
	if len(rs) == 1 {
		return rs[0].label
	}
	nums := make([]int, len(rs))
	for i, r := range rs {
		nums[i] = r.num
	}
	slices.Sort(nums)
	var parts []string
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		switch {
		case j == i:
			parts = append(parts, itoa(nums[i]))
		default:
			parts = append(parts, itoa(nums[i])+"–"+itoa(nums[j]))
		}
		i = j + 1
	}
	return itoa(len(rs)) + " ranks (" + strings.Join(parts, ", ") + ")"
}

func itoa(n int) string { return fmtCount(n) }

func (pc *picture) paintGrid(p *ui.Painter, r ui.Rect, step float64) {
	grid := hex(pc.o.pal.Grid)
	first := math.Floor(float64(pc.o.t0)/step) * step
	for t := first; ; t += step {
		x := pc.xAt(t)
		if x > r.X+r.W {
			break
		}
		if x >= r.X+pc.o.gutter {
			p.Fill(ui.Rect{X: x, Y: r.Y, W: 1, H: r.H}, grid, 0)
		}
	}
}

func (pc *picture) paintRuler(p *ui.Painter, r ui.Rect, step float64) {
	o, pal := pc.o, pc.o.pal
	p.Fill(r, hex(pal.Ruler), 0)
	p.Fill(ui.Rect{X: r.X, Y: r.Y + rulerH - 1, W: r.W, H: 1}, hex(pal.Border), 0)
	p.Clip(ui.Rect{X: r.X + o.gutter, Y: r.Y, W: r.W - o.gutter, H: rulerH}, 0, func() {
		minor := step / 5
		first := math.Floor(float64(o.t0)/minor) * minor
		for i := 0; ; i++ {
			t := first + float64(i)*minor
			x := pc.xAt(t)
			if x > r.X+r.W+2 {
				break
			}
			if math.Abs(math.Remainder(t, step)) < minor/2 {
				p.Fill(ui.Rect{X: x, Y: r.Y + rulerH - 10, W: 1, H: 10}, hex(pal.Muted), 0)
				if x < r.X+o.gutter {
					continue // its label would be cut
				}
				p.RichText(x+4, r.Y+5, 0, ui.Span{Text: fmtTick(t, step), Size: 10.5, Color: hex(pal.Muted)})
			} else {
				p.Fill(ui.Rect{X: x, Y: r.Y + rulerH - 5, W: 1, H: 5}, hex(pal.Faint), 0)
			}
		}
	})
	p.Fill(ui.Rect{X: r.X, Y: r.Y, W: o.gutter, H: rulerH - 1}, hex(pal.Panel), 0)
	p.Fill(ui.Rect{X: r.X + o.gutter - 1, Y: r.Y, W: 1, H: rulerH}, hex(pal.Border), 0)
	p.RichText(r.X+14, r.Y+8, 0,
		ui.Span{Text: "Window ", Size: 11, Color: hex(pal.Faint)},
		ui.Span{Text: fmtDur(float64(o.t1 - o.t0)), Size: 11, Weight: 600, Color: hex(pal.Muted)})
}

// paintMarks draws the -mark times: a line down the rows, its time on top.
func (pc *picture) paintMarks(p *ui.Painter, r ui.Rect) {
	col := hex(pc.o.pal.Range)
	p.Clip(ui.Rect{X: r.X + pc.o.gutter, Y: r.Y, W: r.W - pc.o.gutter, H: r.H}, 0, func() {
		for _, m := range pc.o.mark {
			a, b := pc.xAt(float64(m[0])), pc.xAt(float64(m[1]))
			if m[1] > m[0] {
				p.Fill(ui.Rect{X: a, Y: r.Y, W: max(b-a, 1), H: r.H}, col.Alpha(0.10), 0)
				p.Fill(ui.Rect{X: b, Y: r.Y, W: 1, H: r.H}, col.Alpha(0.7), 0)
			}
			p.Fill(ui.Rect{X: a, Y: r.Y, W: 1, H: r.H}, col.Alpha(0.7), 0)
		}
	})
}

func (pc *picture) paintItem(p *ui.Painter, r ui.Rect, top float32, it *item) {
	o, pal := pc.o, pc.o.pal
	y := top + it.y
	switch it.kind {
	case itemRank:
		if it.sepAbove {
			p.Fill(ui.Rect{X: r.X, Y: y - rankGap/2, W: r.W, H: 1}, hex(pal.Border), 0)
		}
		p.Fill(ui.Rect{X: r.X, Y: y, W: r.W, H: it.h}, hex(pal.PanelAlt), 0)
		p.Fill(ui.Rect{X: r.X, Y: y + it.h - 1, W: r.W, H: 1}, hex(pal.Border), 0)
		p.Fill(ui.Rect{X: r.X + 12, Y: y + 8, W: 3, H: it.h - 16}, hex(pal.Accent), 1.5)
		p.RichText(r.X+22, y+7, 0,
			ui.Span{Text: it.label, Size: 13, Weight: 650, Color: hex(pal.Text)},
			ui.Span{Text: "     " + it.info, Size: 11, Color: hex(pal.Faint)})
		if t := it.r; o.t0 < t.offset || o.t1 > t.offset+t.tr.Duration() {
			// The parts of the window outside the trace: hatched faintly.
			pc.paintOutside(p, r, top, it.r)
		}
	case itemGroup:
		p.Fill(ui.Rect{X: r.X, Y: y, W: o.gutter, H: it.h}, hex(pal.Panel), 0)
		p.RichText(r.X+14, y+4, 0, ui.Span{Text: it.label, Size: 11, Weight: 600, Color: hex(pal.Muted)})
		p.Fill(ui.Rect{X: r.X + o.gutter, Y: y + it.h - 1, W: r.W - o.gutter, H: 1}, hex(pal.Grid), 0)
	case itemTrack, itemSummary:
		if it.alt {
			p.Fill(ui.Rect{X: r.X + o.gutter, Y: y, W: r.W - o.gutter, H: it.h}, hex(pal.RowAlt).Alpha(0.6), 0)
		}
		p.Clip(ui.Rect{X: r.X + o.gutter, Y: y, W: r.W - o.gutter, H: it.h}, 0, func() {
			if it.kind == itemTrack {
				tk := it.r.tr.Tracks[it.track]
				for i, li := range it.lanes {
					pc.paintLane(p, it.r, tk.Lanes[li], y+it.pad+float32(i)*it.lh, it.lh)
				}
			} else {
				for _, ref := range it.refs {
					pc.paintLane(p, it.r, it.r.tr.Tracks[ref.Row].Lanes[ref.Lane], y, it.lh)
				}
			}
		})
		pc.paintGutter(p, r, y, it)
	case itemStrip:
		pc.paintStrip(p, r, y, it)
	case itemMore:
		p.RichText(r.X+14, y+5, 0, ui.Span{Text: "+ " + fmtCount(pc.cut) + " more rows not shown: narrow with -ranks, -rows, -match, or raise -max-height",
			Size: 11.5, Color: hex(pal.Faint)})
	}
}

// paintOutside hatches, across a rank's block, the window's times before
// its trace starts or after it ends.
func (pc *picture) paintOutside(p *ui.Painter, r ui.Rect, top float32, rk *rank) {
	y0, y1 := float32(math.Inf(1)), float32(0)
	for _, it := range pc.items {
		if it.r == rk && it.kind != itemRank {
			y0, y1 = min(y0, it.y), max(y1, it.y+it.h)
		}
	}
	if y1 <= y0 {
		return
	}
	shade := hex(pc.o.pal.Faint).Alpha(0.10)
	x0, x1 := pc.xAt(float64(rk.offset)), pc.xAt(float64(rk.offset+rk.tr.Duration()))
	g := r.X + pc.o.gutter
	if x0 > g {
		p.Fill(ui.Rect{X: g, Y: top + y0, W: x0 - g, H: y1 - y0}, shade, 0)
	}
	if x1 < r.X+r.W {
		p.Fill(ui.Rect{X: x1, Y: top + y0, W: r.X + r.W - x1, H: y1 - y0}, shade, 0)
	}
}

func (pc *picture) swatch(k trace.TrackKind) ui.Color {
	pal := pc.o.pal
	switch k {
	case trace.TrackRuntime:
		return hex(pal.SwatchRuntime)
	case trace.TrackNVTX:
		return hex(pal.SwatchNVTX)
	case trace.TrackCPU:
		return hex(pal.SwatchCPU)
	case trace.TrackPython:
		return hex(pal.SwatchPython)
	}
	return hex(pal.SwatchStream)
}

func (pc *picture) paintGutter(p *ui.Painter, r ui.Rect, y float32, it *item) {
	o, pal := pc.o, pc.o.pal
	p.Fill(ui.Rect{X: r.X, Y: y, W: o.gutter, H: it.h}, hex(pal.Panel), 0)
	if it.label == "" {
		return
	}
	// The name and swatch centered on the first lane.
	label := ui.Span{Text: it.label, Size: 11.5, Color: hex(pal.Text)}
	label.Text = fitText(p, label, o.gutter-34, true)
	_, th := p.MeasureText(0, label)
	cy := y + it.pad + it.lh/2
	p.Fill(ui.Rect{X: r.X + 14, Y: cy - 5, W: 3, H: 10}, pc.swatch(it.swatch), 1.5)
	p.Clip(ui.Rect{X: r.X + 22, Y: y, W: o.gutter - 30, H: it.h}, 0, func() {
		p.RichText(r.X+24, cy-th/2, 0, label)
	})
}

// paintLane draws a lane's events in the window with its top at ly, lh
// high: as the viewer, each wide enough to see on its own with its name
// where it fits, those too small to tell apart merged into blocks as
// opaque as they are dense.
func (pc *picture) paintLane(p *ui.Painter, rk *rank, l *trace.Lane, ly, lh float32) {
	o, pal := pc.o, pc.o.pal
	tr := rk.tr
	scale := pc.nsPerPx()
	a, b := rk.toTrace(o.t0), rk.toTrace(o.t1)+1
	bg := hex(pal.Bg)
	for _, bl := range l.LOD(a, b, scale, 1, 5, nil) {
		e := &l.Events[bl.Index]
		name := tr.Name(e)
		x0 := float64(pc.xAt(float64(rk.toSession(bl.Start))))
		x1 := float64(pc.xAt(float64(rk.toSession(bl.End))))
		x0, x1 = max(x0, float64(o.gutter)-4), min(x1, float64(o.width)+4)
		w := max(x1-x0, 1)
		box := ui.Rect{X: float32(x0), Y: ly + 1, W: float32(w), H: lh - 3}
		col := pc.color(name, e.Kind)
		dim := o.highlight != nil && (bl.Merged || !o.highlight.MatchString(name))
		switch {
		case e.Kind == trace.KindNVTXMark && !bl.Merged:
			cy := ly + lh/2
			var d ui.Path
			x := float32(x0)
			d.MoveTo(x, cy-6).LineTo(x+5, cy).LineTo(x, cy+6).LineTo(x-5, cy).Close()
			p.FillPath(&d, col)
		case bl.Merged:
			al := float32(0.3 + 0.6*bl.Density())
			if dim {
				al *= 0.3
			}
			p.Fill(box, col.Mix(bg, 0.3).Alpha(al), 0)
			p.Fill(ui.Rect{X: box.X, Y: box.Y, W: box.W, H: 1}, col.Alpha(al), 0)
		default:
			if dim {
				col = col.Mix(bg, 0.75)
			}
			radius := float32(0)
			if w > 6 {
				radius = 3
			}
			if w >= 4 {
				box.W-- // a seam between events back to back
			}
			p.Fill(box, col, radius)
			if w >= theme.LabelMinW {
				pc.paintLabel(p, box, name, bl.End-bl.Start, dim)
			}
		}
	}
}

func (pc *picture) paintLabel(p *ui.Painter, box ui.Rect, name string, dur int64, dim bool) {
	if len(name) > 200 {
		name = name[:200]
	}
	on := hex(pc.o.pal.OnEvent)
	if dim {
		on = on.Alpha(0.5)
	}
	label := ui.Span{Text: name, Size: 10.5, Weight: 500, Color: on}
	w, h := p.MeasureText(0, label)
	pad := float32(5)
	if box.W < 40 {
		pad = 3
	}
	x := box.X + pad
	if w+2*pad > box.W {
		p.Clip(ui.Rect{X: box.X + 2, Y: box.Y, W: box.W - 4, H: box.H}, 0, func() {
			p.RichText(x, box.Y+(box.H-h)/2, 0, label)
		})
		return
	}
	if box.W > w+2*pad+60 {
		ds := ui.Span{Text: "  " + fmtDur(float64(dur)), Size: 9.5, Color: on.Alpha(0.65)}
		dw, _ := p.MeasureText(0, ds)
		if w+dw+2*pad <= box.W {
			p.RichText(x, box.Y+(box.H-h)/2, 0, label, ds)
			return
		}
	}
	p.RichText(x, box.Y+(box.H-h)/2, 0, label)
}

// paintStrip draws a rank's busy/idle strip: the share of each pixel
// column its GPUs are busy, columns where they idle in the warning color.
func (pc *picture) paintStrip(p *ui.Painter, r ui.Rect, y float32, it *item) {
	o, pal := pc.o, pc.o.pal
	rk := it.r
	p.Fill(ui.Rect{X: r.X, Y: y, W: o.gutter, H: it.h}, hex(pal.Panel), 0)
	n := int(o.width - o.gutter)
	cols := make([]float64, n)
	scale := pc.nsPerPx()
	for _, iv := range o.gpuIntervals(rk) {
		s, e := float64(iv[0]-o.t0), float64(iv[1]-o.t0)
		for c := max(int(s/scale), 0); c < n && float64(c)*scale < e; c++ {
			a, b := max(s, float64(c)*scale), min(e, float64(c+1)*scale)
			cols[c] += max(b-a, 0) / scale
		}
	}
	c0 := max(int(math.Ceil(float64(rk.offset-o.t0)/scale)), 0)
	c1 := min(int(math.Floor(float64(rk.offset+rk.tr.Duration()-o.t0)/scale)), n-1)
	level := func(v float64) int {
		if v < theme.CompactIdle {
			return 0
		}
		return 1 + int(min(v, 1)*7)
	}
	idle, accent := hex(pal.Warn).Alpha(0.85), hex(pal.Accent)
	for c := c0; c <= c1; {
		lv := level(cols[c])
		e := c + 1
		for e <= c1 && level(cols[e]) == lv {
			e++
		}
		box := ui.Rect{X: r.X + o.gutter + float32(c), Y: y + 1, W: float32(e - c), H: stripH}
		if lv == 0 {
			p.Fill(box, idle, 0)
		} else {
			p.Fill(box, accent.Alpha(0.12+0.4*float32(lv)/8), 0)
		}
		c = e
	}
}

func (pc *picture) paintLegend(p *ui.Painter, r ui.Rect) {
	o, pal := pc.o, pc.o.pal
	p.Fill(r, hex(pal.Panel), 0)
	p.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W, H: 1}, hex(pal.Border), 0)
	what := "GPU work"
	if pc.legend[0].kind != trace.KindKernel && pc.legend[0].kind != trace.KindMemcpy && pc.legend[0].kind != trace.KindMemset {
		what = "events"
	}
	p.RichText(r.X+16, r.Y+12, 0,
		ui.Span{Text: "Top " + what + " in the window", Size: 12, Weight: 650, Color: hex(pal.Text)},
		ui.Span{Text: "   by total time on the rows shown, of all ranks", Size: 11, Color: hex(pal.Faint)})
	cols := legendCols(o.width)
	colW := (r.W - 32) / float32(cols)
	rows := (len(pc.legend) + cols - 1) / cols
	for i, e := range pc.legend {
		cx := r.X + 16 + float32(i/rows)*colW
		cy := r.Y + 36 + float32(i%rows)*legendRH
		p.Fill(ui.Rect{X: cx, Y: cy + 3, W: 12, H: 12}, pc.color(e.name, e.kind), 3)
		stat := ui.Span{Text: fmtDur(float64(e.total)) + "  " + trimFloat(100*e.share, 1) + "%  ×" + fmtCount(e.count), Size: 10.5, Color: hex(pal.Muted)}
		sw, _ := p.MeasureText(0, stat)
		p.RichText(cx+colW-sw-16, cy+1, 0, stat)
		p.Clip(ui.Rect{X: cx + 18, Y: cy, W: colW - sw - 42, H: legendRH}, 0, func() {
			name := ui.Span{Text: e.name, Size: 11, Color: hex(pal.Text)}
			name.Text = fitText(p, name, colW-sw-46, false)
			p.RichText(cx+20, cy+1, 0, name)
		})
	}
}

// fitText is sp's text cut to fit w with an ellipsis: at its end, or, with
// keepTail, before its last " · " part (a row's kind: "thread 42 · CPU
// ops"), which stays.
func fitText(p *ui.Painter, sp ui.Span, w float32, keepTail bool) string {
	if tw, _ := p.MeasureText(0, sp); tw <= w {
		return sp.Text
	}
	head, tail := sp.Text, ""
	if i := strings.LastIndex(head, " · "); keepTail && i > 0 {
		head, tail = head[:i], head[i:]
	}
	r := []rune(head)
	lo, hi := 0, len(r) // the most runes of head that fit
	for lo < hi {
		mid := (lo + hi + 1) / 2
		sp.Text = string(r[:mid]) + "…" + tail
		if tw, _ := p.MeasureText(0, sp); tw <= w {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimRight(string(r[:lo]), " ") + "…" + tail
}
