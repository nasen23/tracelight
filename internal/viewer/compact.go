package viewer

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"tracelight/internal/theme"
)

// The rank overview's layout and busy columns: the same functions as
// shared.js's compactLayout and busyColumns (TestBrowserClientMatchesDesktop
// checks them against each other), so that both clients line ranks up alike.

// compactRow is a row of the rank overview: a trace's busy/idle strip (key
// "") or a sub-lane (Sub) of one of its summary lanes.
type compactRow struct {
	Trace int     `json:"trace"`
	Key   string  `json:"key"`
	Sub   int     `json:"sub"`
	Y     float64 `json:"y"`
	H     float64 `json:"h"`
}

// compactLayout lays out traces, each given as how many sub-lanes the
// summary lane of each kind of its rows has ({"stream": 3, "nvtx": 2}; no
// kind it has no rows of), with the summary lanes of keys in on.
func compactLayout(subs []map[string]int, on []string) ([]compactRow, float64) {
	var rows []compactRow
	y := 0.0
	for ti, ks := range subs {
		if ti > 0 {
			y += theme.CompactGap
		}
		rows = append(rows, compactRow{ti, "", 0, y, theme.CompactStripH})
		y += theme.CompactStripH
		first := true
		for _, l := range theme.CompactLanes {
			n := ks[l.Kind]
			if !slices.Contains(on, l.Key) || n <= 0 {
				continue
			}
			if !first {
				y += theme.CompactKindGap
			}
			first = false
			for sub := range n {
				rows = append(rows, compactRow{ti, l.Key, sub, y, theme.CompactLaneH})
				y += theme.CompactLaneH
			}
		}
	}
	return rows, y
}

// trackLane is the height of a lane of a row of kind ("stream", "api",
// "nvtx", ...) and the space above and below its lanes: GPU streams and
// CUDA API calls are denser than the others. As the browser client's.
func trackLane(kind string) (lh, pad float32) {
	if kind == "stream" || kind == "api" {
		return theme.DenseLaneH, theme.DensePad
	}
	return theme.LaneH, theme.TrackPad
}

// trackHeight is the height of a row of kind with lanes lanes.
func trackHeight(kind string, lanes int) float32 {
	lh, pad := trackLane(kind)
	return float32(max(lanes, 1))*lh + 2*pad
}

// laneSlot is the sub-lane of its kind's summary lane that lane li of a
// row (of kind "stream", "api" or "nvtx", of lanes lanes) draws into, or -1
// if none: where trace.Track.Overview puts it when stacked (or, without
// it, the first: every lane of a stream, the outermost of the others), the
// first when overlaid.
func laneSlot(kind string, lanes int, overview []int, li int, stacked bool) int {
	switch {
	case li >= lanes:
		return -1
	case stacked && overview != nil:
		if li < len(overview) {
			return overview[li]
		}
		return -1
	case kind == "stream" || li == 0:
		return 0
	}
	return -1
}

// busyColumns is the share of each of n pixel columns from time t0 at scale
// ns a pixel that blocks ([start, end, busy] triples, flat) keep busy.
func busyColumns(blocks []float64, t0, scale float64, n int, out []float64) []float64 {
	n = max(n, 0) // a window narrower than the label gutter
	out = slices.Grow(out[:0], n)[:n]
	clear(out)
	for i := 0; i+2 < len(blocks); i += 3 {
		s, e, busy := blocks[i], blocks[i+1], blocks[i+2]
		d := 1.0
		if e > s {
			d = math.Min(busy/(e-s), 1)
		}
		c0 := int(math.Max(math.Floor((s-t0)/scale), 0))
		c1 := int(math.Min(math.Floor((e-t0)/scale), float64(n-1)))
		for c := c0; c <= c1; c++ {
			a, b := math.Max(s, t0+float64(c)*scale), math.Min(e, t0+float64(c+1)*scale)
			if e > s {
				out[c] += d * math.Max(b-a, 0) / scale
			} else {
				out[c] += 1.0 / 3
			}
		}
	}
	for c := range out {
		out[c] = math.Min(out[c], 1)
	}
	return out
}

// tabTitle names a tab by the paths it was opened with: the first one's
// last element, without a trace file's extension, and how many more; "New
// tab" for none. The same function as shared.js's tabTitle.
func tabTitle(inputs []string) string {
	if len(inputs) == 0 {
		return "New tab"
	}
	if len(inputs) > 1 {
		return traceBase(inputs[0]) + " +" + strconv.Itoa(len(inputs)-1)
	}
	return traceBase(inputs[0])
}

// traceExts are the endings of trace files' names, the longest first
// (shared.js's traceBase has them too).
var traceExts = []string{".sqlite", ".nsys-rep", ".qdrep", ".pt.trace.json.gz", ".pt.trace.json", ".json.gz", ".json"}

// traceBase is the last element of a path without a trace file's ending:
// "rank0" of "/runs/rank0.pt.trace.json".
func traceBase(p string) string {
	// A rank of a merged file ("…/job.json.gz#rank8", trace.Members).
	if i := strings.LastIndex(p, "#rank"); i >= 0 && i+5 < len(p) && strings.Trim(p[i+5:], "0123456789") == "" {
		return "rank" + p[i+5:]
	}
	s := strings.TrimRight(p, "/\\")
	b := s[strings.LastIndexAny(s, "/\\")+1:]
	if b == "" {
		b = p
	}
	for _, ext := range traceExts {
		if len(b) > len(ext) && strings.EqualFold(b[len(b)-len(ext):], ext) {
			return b[:len(b)-len(ext)]
		}
	}
	return b
}

// traceFilter is how a session's link says which of its traces (by key,
// traceBase) are left out (off): "only" and the keys of those shown, if
// fewer, else "omit" and the keys of those left out; "" if none is. As the
// browser client's.
func traceFilter(keys []string, off []bool) (string, []string) {
	var shown, omitted []string
	for i, k := range keys {
		if i < len(off) && off[i] {
			omitted = append(omitted, k)
		} else {
			shown = append(shown, k)
		}
	}
	switch {
	case len(omitted) == 0:
		return "", nil
	case len(shown) < len(omitted):
		return "only", shown
	}
	return "omit", omitted
}

// filterOff is which traces (by key) a link's only and omit leave out:
// with only, those it does not name; else those omit names. None if that
// would be every trace. As the browser client's.
func filterOff(keys, only, omit []string) []bool {
	off := make([]bool, len(keys))
	n := 0
	for i, k := range keys {
		if len(only) > 0 {
			off[i] = !slices.Contains(only, k)
		} else {
			off[i] = slices.Contains(omit, k)
		}
		if off[i] {
			n++
		}
	}
	if n == len(keys) {
		clear(off)
	}
	return off
}
