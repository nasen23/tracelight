package main

import (
	"cmp"
	"math"
	"slices"

	"github.com/egoist/mygo/ui"
)

// A zoomed-in picture, for reporting what it shows: a context strip above
// the timeline (every rank's GPU busy line over a longer stretch, the
// window boxed in it and opened out to the timeline below), and, for an
// imbalanced collective, where each rank arrived at it: a line and a
// "+1.2 ms" on each rank, the wait between the first and the last shaded.

const (
	ctxPadTop = 8
	ctxFunnel = 22 // the room where the box opens out to the timeline
	ctxStrips = 96 // the height the ranks' lines share, at most
)

// contextRowH is the height of a rank's line in the context strip.
func contextRowH(n int) float32 {
	return min(max(float32(ctxStrips)/float32(max(n, 1)), 1.5), 12)
}

func contextHeight(n int) float32 {
	return ctxPadTop + float32(n)*(contextRowH(n)+1) + 3 + ctxFunnel
}

// contextWindow is the stretch the context strip shows around [t0, t1):
// factor times as long, centered on it, within the session.
func contextWindow(t0, t1, span int64, factor float64) [2]int64 {
	d := int64(float64(t1-t0) * factor)
	if d >= span {
		return [2]int64{0, span}
	}
	a := (t0+t1)/2 - d/2
	a = min(max(a, 0), span-d)
	return [2]int64{a, a + d}
}

// arrivalInfo says when rank r arrived at collective c, for its heading.
func arrivalInfo(c *collective, r *rank) string {
	late, ok := c.lateness(r)
	if !ok {
		return "not in " + c.label()
	}
	wait := c.arr[c.last].start - c.arr[c.first].start - late
	switch {
	case c.arr[c.last].r == r:
		return "arrived last, +" + fmtDur(float64(late))
	case c.arr[c.first].r == r:
		return "arrived first, waited " + fmtDur(float64(wait))
	}
	return "arrived +" + fmtDur(float64(late)) + ", waited " + fmtDur(float64(wait))
}

func (pc *picture) paintContext(p *ui.Painter, r ui.Rect) {
	o, pal := pc.o, pc.o.pal
	p.Fill(r, hex(pal.Panel), 0)
	n := len(pc.ss.ranks)
	rowH := contextRowH(n)
	g := r.X + o.gutter
	dataW := r.W - o.gutter
	c0, c1 := o.ctx[0], o.ctx[1]
	xc := func(t int64) float32 { return g + float32(float64(t-c0)/float64(c1-c0)*float64(dataW)) }
	y0 := r.Y + ctxPadTop
	stripsH := float32(n) * (rowH + 1)
	// The gutter: what this is, and its span.
	p.RichText(r.X+14, y0-2, 0, ui.Span{Text: "Context", Size: 11.5, Weight: 650, Color: hex(pal.Muted)})
	p.RichText(r.X+14, y0+14, 0, ui.Span{Text: fmtSpan(c0, c1), Size: 10, Color: hex(pal.Faint)})
	p.RichText(r.X+14, y0+28, 0, ui.Span{Text: "GPU busy, a line a rank", Size: 10, Color: hex(pal.Faint)})
	p.Fill(ui.Rect{X: g, Y: y0 - 2, W: dataW, H: stripsH + 3}, hex(pal.Bg), 2)
	scale := float64(c1-c0) / float64(dataW)
	cols := make([]float64, int(dataW))
	idle, accent := hex(pal.Warn).Alpha(0.55), hex(pal.Accent)
	for i, rk := range pc.ss.ranks {
		clear(cols)
		co := &options{t0: c0, t1: c1}
		for _, iv := range co.gpuIntervals(rk) {
			s, e := float64(iv[0]-c0), float64(iv[1]-c0)
			for c := max(int(s/scale), 0); c < len(cols) && float64(c)*scale < e; c++ {
				a, b := max(s, float64(c)*scale), min(e, float64(c+1)*scale)
				cols[c] += max(b-a, 0) / scale
			}
		}
		y := y0 + float32(i)*(rowH+1)
		a := max(int(math.Ceil(float64(rk.offset-c0)/scale)), 0)
		b := min(int(float64(rk.offset+rk.tr.Duration()-c0)/scale), len(cols)-1)
		for c := a; c <= b; {
			v := min(cols[c], 1)
			busy := v >= 0.05
			e := c + 1
			for e <= b && (min(cols[e], 1) >= 0.05) == busy && (!busy || math.Abs(min(cols[e], 1)-v) < 0.15) {
				e++
			}
			box := ui.Rect{X: g + float32(c), Y: y, W: float32(e - c), H: rowH}
			if busy {
				p.Fill(box, accent.Alpha(float32(0.25+0.6*v)), 0)
			} else {
				p.Fill(box, idle, 0)
			}
			c = e
		}
	}
	// The window, boxed, opening out to the timeline below.
	bx0, bx1 := xc(o.t0), xc(o.t1)
	if bx1-bx0 < 3 {
		m := (bx0 + bx1) / 2
		bx0, bx1 = m-1.5, m+1.5
	}
	rng := hex(pal.Range)
	box := ui.Rect{X: bx0, Y: y0 - 4, W: bx1 - bx0, H: stripsH + 7}
	p.Fill(box, rng.Alpha(0.14), 2)
	p.Stroke(box, rng, 2, 1.5)
	fy0, fy1 := box.Y+box.H, r.Y+r.H
	var f ui.Path
	f.MoveTo(bx0, fy0).LineTo(bx1, fy0).LineTo(r.X+r.W, fy1).LineTo(g, fy1).Close()
	p.FillPath(&f, rng.Alpha(0.07))
	p.Line(bx0, fy0, g, fy1, 1.2, rng.Alpha(0.75))
	p.Line(bx1, fy0, r.X+r.W, fy1, 1.2, rng.Alpha(0.75))
	zoom := float64(c1-c0) / float64(max(o.t1-o.t0, 1))
	if zoom >= 1.5 {
		label := ui.Span{Text: trimFloat(zoom, 1) + "× zoom", Size: 10, Weight: 600, Color: rng}
		w, _ := p.MeasureText(0, label)
		lx := min(bx1+6, r.X+r.W-w-6)
		if lx < bx1 {
			lx = max(bx0-w-6, g+4)
		}
		p.RichText(lx, fy0+2, 0, label)
	}
}

// paintArrivals marks where each rank arrived at the collective -imbalance
// picked.
func (pc *picture) paintArrivals(p *ui.Painter, r ui.Rect, top float32) {
	c := pc.o.imb
	if c == nil {
		return
	}
	o, pal := pc.o, pc.o.pal
	g := r.X + o.gutter
	warn, rng, muted := hex(pal.Warn), hex(pal.Range), hex(pal.Muted)
	first, last := c.arr[c.first], c.arr[c.last]
	xf, xl := pc.xAt(float64(first.start)), pc.xAt(float64(last.start))
	p.Clip(ui.Rect{X: g, Y: top, W: r.W - o.gutter, H: pc.rowsH}, 0, func() {
		// The wait: from the first rank's arrival to the last's, shaded.
		p.Fill(ui.Rect{X: xf, Y: top, W: max(xl-xf, 1), H: pc.rowsH}, warn.Alpha(0.09), 0)
		dashed(p, xf, top, pc.rowsH, rng.Alpha(0.8))
		dashed(p, xl, top, pc.rowsH, warn.Alpha(0.9))
		for _, a := range c.arr {
			head, y1 := -1, float32(0)
			for i, it := range pc.items {
				if it.r != a.r {
					continue
				}
				if it.kind == itemRank {
					head = i
				}
				y1 = max(y1, it.y+it.h)
			}
			if head < 0 {
				continue
			}
			hy := top + pc.items[head].y
			x := pc.xAt(float64(a.start))
			col := muted
			switch a {
			case last:
				col = warn
			case first:
				col = rng
			}
			p.Fill(ui.Rect{X: x - 0.75, Y: hy + rankH, W: 1.5, H: top + y1 - hy - rankH}, col, 0)
			var tri ui.Path
			tri.MoveTo(x-5, hy+rankH-9).LineTo(x+5, hy+rankH-9).LineTo(x, hy+rankH-2).Close()
			p.FillPath(&tri, col)
			late := a.start - first.start
			text := "+" + fmtDur(float64(late))
			switch a {
			case first:
				text = "first"
			case last:
				text = "+" + fmtDur(float64(late)) + "  last"
			}
			label := ui.Span{Text: text, Size: 10.5, Weight: 650, Color: col}
			w, _ := p.MeasureText(0, label)
			lx := x + 8
			if lx+w > r.X+r.W-4 {
				lx = x - 8 - w
			}
			p.RichText(lx, hy+8, 0, label)
		}
	})
}

// dashed draws a vertical dashed line at x from y, h long.
func dashed(p *ui.Painter, x, y, h float32, c ui.Color) {
	for d := float32(0); d < h; d += 7 {
		p.Fill(ui.Rect{X: x - 0.5, Y: y + d, W: 1, H: min(4, h-d)}, c, 0)
	}
}

// The arrivals chart: how late each rank started the collective, after the
// first, a bar a rank in the ranks' order, the last one in the warning
// color: the finding of an -imbalance picture, at a glance.

const arrRowH = 18

func arrivalCols(n int, width float32) int {
	return min(max(1, (n+15)/16), max(1, int(width/420)))
}

func arrivalsHeight(n int, width float32) float32 {
	cols := arrivalCols(n, width)
	return 44 + float32((n+cols-1)/cols)*arrRowH + 8
}

func (pc *picture) paintArrivalChart(p *ui.Painter, r ui.Rect) {
	c, pal := pc.o.imb, pc.o.pal
	p.Fill(r, hex(pal.Panel), 0)
	p.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W, H: 1}, hex(pal.Border), 0)
	p.RichText(r.X+16, r.Y+12, 0,
		ui.Span{Text: "Arrivals at " + c.label(), Size: 12, Weight: 650, Color: hex(pal.Text)},
		ui.Span{Text: "   when each rank started the collective, after the first; the others waited for the last (" +
			fmtDur(float64(c.waited)) + " of GPU time in all)", Size: 11, Color: hex(pal.Faint)})
	arr := slices.Clone(c.arr)
	slices.SortFunc(arr, func(a, b arrival) int { return cmp.Compare(a.r.num, b.r.num) })
	cols := arrivalCols(len(arr), r.W)
	rows := (len(arr) + cols - 1) / cols
	colW := (r.W - 32) / float32(cols)
	first, last := c.arr[c.first], c.arr[c.last]
	for i, a := range arr {
		x := r.X + 16 + float32(i/rows)*colW
		y := r.Y + 38 + float32(i%rows)*arrRowH
		col := hex(pal.Accent)
		switch a {
		case last:
			col = hex(pal.Warn)
		case first:
			col = hex(pal.Range)
		}
		p.RichText(x, y+2, 0, ui.Span{Text: a.r.label, Size: 11, Color: hex(pal.Text)})
		barX, barW := x+110, colW-110-110
		p.Fill(ui.Rect{X: barX, Y: y + 4, W: barW, H: arrRowH - 8}, hex(pal.Bg), 2)
		late := a.start - first.start
		if c.spread > 0 && late > 0 {
			p.Fill(ui.Rect{X: barX, Y: y + 4, W: max(barW*float32(float64(late)/float64(c.spread)), 2), H: arrRowH - 8}, col, 2)
		}
		text := "+" + fmtDur(float64(late))
		switch a {
		case first:
			text = "first"
		case last:
			text += "  last"
		}
		p.RichText(barX+barW+8, y+2, 0, ui.Span{Text: text, Size: 10.5, Weight: 600, Color: col})
	}
}
