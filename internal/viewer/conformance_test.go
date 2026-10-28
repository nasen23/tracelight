package viewer

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// The browser client (internal/server/web) is a second renderer of the same
// timeline. Its look comes from theme.js, generated from package theme as
// the desktop's does, and its formatting and event colors from shared.js:
// this runs shared.js in Node and checks that it says what the desktop app
// says, so the two renderers cannot drift. See DESIGN.md.
func TestBrowserClientMatchesDesktop(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	shared, err := os.ReadFile(filepath.Join("..", "server", "web", "shared.js"))
	if err != nil {
		t.Fatal(err)
	}
	durs := []float64{0, 1, 12, 999, 1000, 1500, 12_345, 999_999, 1_000_000, 2_345_678, 45_600_000, 999_999_999, 1e9, 12_345_678_900, -2_500}
	type tp struct{ T, S float64 }
	times := []tp{{0, 1}, {1_234_567, 0.1}, {1_234_567, 1000}, {987_654_321, 50_000}, {12_345_678_901, 3e6}, {450, 0.05}, {55_000, 10}}
	scales := []float64{0.02, 0.5, 1, 7, 100, 4096, 1e5, 2.5e7}
	ticks := []tp{{0, 1}, {1500, 500}, {1_500_000, 500_000}, {2e9, 1e9}, {120, 20}, {350_000, 50_000}, {7e8, 1e8}}
	bytes := []int64{0, 1, 1023, 1024, 1536, 1 << 20, 64 << 20, 3 << 30, 123456789}
	names := []string{"", "gemm", "ampere_sgemm_128x128_tn", "cudaLaunchKernel_v7000", "layer.07", "flash_fwd_kernel",
		"ncclDevKernel_AllReduce_Sum_f32_RING_LL", "Memcpy HtoD", "Memcpy DtoH", "Memcpy DtoD", "Memcpy PtoP", "Memset", "µs ünïcödé ✓", "step 42"}
	kinds := []trace.Kind{trace.KindKernel, trace.KindMemcpy, trace.KindMemset, trace.KindRuntime, trace.KindNVTX, trace.KindNVTXMark, trace.KindSync}
	for i := range 200 {
		names = append(names, fmt.Sprintf("kernel_%d<float, %d>", i, i*7))
	}

	kindIDs := make([]int, len(kinds)) // ints: JSON writes a []uint8 as base64
	for i, k := range kinds {
		kindIDs[i] = int(k)
	}
	// A held-key session: press, hold, Shift, zoom while panning, opposite
	// keys, let go, at uneven frame times.
	type navStep struct {
		L, R, I, O, Shift bool
		DT, Speed         float64
	}
	var navSteps []navStep
	for i := range 240 {
		st := navStep{DT: 1.0/60 + float64(i%7)*0.002, Speed: theme.NavSpeeds[i/60%len(theme.NavSpeeds)].Times}
		switch {
		case i < 40:
			st.R = true
		case i < 70:
			st.R, st.Shift = true, true
		case i < 110:
			st.R, st.I = true, true
		case i < 130:
			st.L, st.R = true, true
		case i < 170:
			st.O = true
		}
		navSteps = append(navSteps, st)
	}
	// Compact layouts: traces with some kinds of rows, lanes switched on
	// and off; busy columns of random blocks, some instants, some apart.
	type layoutCase struct {
		Subs []map[string]int
		On   []string
	}
	kindSets := [][]string{{"stream", "api", "nvtx"}, {"stream"}, {"api", "nvtx"}, {}, {"nvtx", "stream"}}
	var layouts []layoutCase
	for i := range 12 {
		var lc layoutCase
		for j := range 1 + i%5 {
			subs := map[string]int{}
			for n, k := range kindSets[(i+j)%len(kindSets)] {
				subs[k] = 1 + (i+j+n)%4
			}
			lc.Subs = append(lc.Subs, subs)
		}
		for k, l := range theme.CompactLanes {
			if (i>>k)&1 == 0 {
				lc.On = append(lc.On, l.Key)
			}
		}
		if lc.On == nil {
			lc.On = []string{}
		}
		layouts = append(layouts, lc)
	}
	// The sub-lanes lanes draw into: stacked or not, with or without the
	// trace's Overview.
	type slotCase struct {
		Kind     string
		Lanes    int
		Overview []int
		Li       int
		Stacked  bool
	}
	var slotCases []slotCase
	for _, k := range []string{"stream", "api", "nvtx"} {
		for _, ov := range [][]int{nil, {2, 0, 1}, {0}} {
			for li := range 4 {
				for _, st := range []bool{true, false} {
					slotCases = append(slotCases, slotCase{k, 3, ov, li, st})
				}
			}
		}
	}
	// Row heights: of every kind of row, of some lanes.
	type heightCase struct {
		Kind  string
		Lanes int
	}
	var heightCases []heightCase
	for _, k := range []string{"stream", "api", "nvtx", "cpu", "python"} {
		for _, n := range []int{0, 1, 2, 7} {
			heightCases = append(heightCases, heightCase{k, n})
		}
	}
	// Which traces a link shows, both ways.
	type filterCase struct {
		Keys, Only, Omit []string
		Off              []bool
	}
	keys := []string{"rank0", "rank1", "rank2", "rank8", "rank8"}
	filterCases := []filterCase{
		{keys, nil, nil, []bool{false, false, false, false, false}},
		{keys, nil, nil, []bool{true, false, false, false, false}},
		{keys, nil, nil, []bool{true, true, true, false, false}},
		{keys, nil, nil, []bool{true, true, false, true, true}},
		{keys, nil, nil, []bool{true, true, true, true, true}},
		{keys, nil, nil, []bool{false, true}},
		{keys, []string{"rank8"}, nil, nil},
		{keys, []string{"rank9"}, nil, nil},
		{keys, nil, []string{"rank1", "rank2"}, nil},
		{keys, []string{"rank0"}, []string{"rank0"}, nil},
		{keys, nil, keys, nil},
		{nil, nil, nil, nil},
	}
	// Tab titles.
	titleCases := [][]string{{}, {"/data/run/rank0.sqlite"}, {"/data/run/"}, {"/data/run/*.nsys-rep"}, {"/a/b.QDREP"},
		{"rank1.nsys-rep", "/x/rank2.nsys-rep", "/x/rank3.sqlite"}, {"/"}, {".sqlite"}, {`C:\traces\job.sqlite`}, {"/data/ranks16"}, {"/a/b.sqlite.sqlite"},
		{"/runs/rank0.pt.trace.json"}, {"/runs/host_123.1729.pt.trace.json.gz"}, {"trace.json.gz"}, {"/a/b.trace.json"}, {"/a/.json"}, {"/a/x.JSON"},
		{"/runs/job.json.gz#rank8"}, {"/runs/job.json#rank"}, {"/runs/a#rank8x"}, {"/runs/job.json.gz#rank16", "/runs/job.json.gz#rank24"}}
	type busyCase struct {
		Blocks    []float64
		T0, Scale float64
		N         int
	}
	var busies []busyCase
	for i := range 20 {
		bc := busyCase{T0: float64(i * 1000), Scale: 37.5 * float64(1+i%4), N: 50 + i}
		t := bc.T0 - 500
		for range 30 {
			t += float64((i*7919)%300) + 11
			d := float64((i*104729)%900) * float64(i%3)
			bc.Blocks = append(bc.Blocks, t, t+d, d*0.6)
			t += d
		}
		busies = append(busies, bc)
	}
	// Alignments: wall clocks, reports without one on a host with an
	// export, hosts without any, no clocks; both modes.
	const utc0, mono0 = 1_792_411_200_123_456_789.0, 1_515_000_000_000_000.0
	alignSets := [][]alignIn{
		{{UTC: utc0, Mono: mono0, Host: "a", Start: 11e6}, {UTC: utc0 + 250e6, Mono: mono0 + 250e6, Host: "a", Start: 9e6}, {UTC: utc0 + 40e6, Mono: 7e15, Host: "b", Start: 0}},
		{{UTC: utc0, Mono: mono0, Host: "a", Start: 1e6}, {Mono: mono0 + 123_456, Host: "a", Start: 2e6}, {Mono: 3e15, Host: "b", Start: 5e6}, {Mono: 3e15 + 7e6, Host: "b", Start: 1e6}, {Host: "c"}},
		{{Mono: mono0, Host: "n0", Start: 1e6}, {Mono: mono0 + 99e6, Host: "n0", Start: 1e6}, {Mono: 9e14, Host: "n1"}},
		{{}, {UTC: utc0, Start: 5}},
		// Ranks merged into one file: no host, one clock (and another
		// file's, and a trace of neither).
		{{Mono: 1.95e15, Clock: "/r/job.json.gz", Start: 6e6}, {Mono: 1.95e15, Clock: "/r/job.json.gz", Start: 1e6}, {Mono: 1.95e15, Clock: "/r/other.json", Start: 2e6}, {Mono: 5e12, Start: 3e6}},
		{{Mono: 2e15, Clock: "c", Start: 1e6}, {Mono: 2e15 + 4e6, Host: "h", Start: 0}, {Mono: 2e15 + 9e6, Host: "h", Start: 0}},
	}
	// Collectives: one job's sequence of ends (irregular gaps), seen by
	// each rank from its own first event, with jitter, over windows of its
	// own; clocks right to a few ms, wrong by much more, or none.
	collOf := func(shift float64, from, to int, jitter int) map[string][]float64 {
		m := map[string][]float64{}
		t := 0.0
		for k := 0; k < to; k++ {
			t += 2e6 + float64((k*k*7919+k*31)%97)*1.7e4
			if k < from {
				continue
			}
			op := "allreduce"
			if k%5 == 0 {
				op = "allgather"
			}
			m[op] = append(m[op], t-shift+float64(((k+jitter)*104729)%41-20)*1e3)
		}
		return m
	}
	collSets := [][]alignIn{
		{{UTC: 1, Host: "a", Start: 9e6, Coll: collOf(0, 0, 120, 0)}, {UTC: 1 + 480e6 + 2.3e6, Host: "b", Start: 9e6, Coll: collOf(480e6, 10, 130, 3)},
			{UTC: 1 + 380e6 - 0.5e6, Host: "c", Start: 9e6, Coll: collOf(380e6, 4, 110, 5)}, {Host: "d", Start: 9e6, Coll: collOf(311e6, 0, 125, 7)}},
		{{Host: "a", Coll: collOf(0, 0, 60, 1)}, {Host: "b", Coll: collOf(70e6, 3, 64, 2)}, {Host: "c", Coll: map[string][]float64{"allreduce": {1, 2, 3}}}},
		{{UTC: 1, Host: "a", Coll: collOf(0, 0, 50, 0)}, {UTC: 1 + 900e6, Host: "b", Coll: collOf(5e6, 0, 50, 9)}},
	}
	type alignCase struct {
		Ts   []alignIn
		Mode string
	}
	var aligns []alignCase
	for _, ts := range alignSets {
		aligns = append(aligns, alignCase{ts, "abs"}, alignCase{ts, "start"}, alignCase{ts, "collectives"})
	}
	for _, ts := range collSets {
		aligns = append(aligns, alignCase{ts, "collectives"}, alignCase{ts, "abs"})
	}
	// Zooms: in and out, at either edge and the middle, of views inside
	// the bounds, past them, and at the widest and the deepest.
	type zoomCase struct{ Start, Scale, Factor, X, Lo, Hi, W float64 }
	var zooms []zoomCase
	for _, b := range [][3]float64{{0, 100e6, 1200}, {5e6, 2.5e9, 900}, {0, 500, 640}} {
		lo, hi, w := b[0], b[1], b[2]
		widest := max(hi-lo, 1000) * 1.5 / w
		for _, v := range [][2]float64{{lo, (hi - lo) / w}, {lo + (hi-lo)*0.4, (hi - lo) / w / 50}, {lo - widest*w*0.8, widest}, {hi - 3, minScale}, {lo, widest * 0.9}} {
			for _, f := range []float64{1 / 1.6, 1 / 1.35, 1.0001, 1.35, 1.6, 40, 1e-6} {
				for _, x := range []float64{0, w * 0.1, w / 2, w - 1} {
					zooms = append(zooms, zoomCase{v[0], v[1], f, x, lo, hi, w})
				}
			}
		}
	}
	// Tabs dragged along a bar of tabs of uneven widths, to every place.
	type dropCase struct {
		Lefts, Widths []float64
		From          int
		Middle        float64
	}
	var drops []dropCase
	for n := 1; n <= 6; n++ {
		lefts, widths := make([]float64, n), make([]float64, n)
		x := 10.0
		for i := range n {
			lefts[i], widths[i] = x, 96+float64((i*37)%124)
			x += widths[i] + 2
		}
		for from := range n {
			for m := -50.0; m <= x+50; m += 23 {
				drops = append(drops, dropCase{lefts, widths, from, m})
			}
		}
	}
	// Too many traces: counts and sizes about the bounds, picks spread.
	type chooseCase struct {
		Count  int
		Bytes  int64
		Inputs int
	}
	var chooses []chooseCase
	for _, n := range []int{0, 1, 2, 8, 15, 16, 17, 64, 256} {
		for _, b := range []int64{0, 1 << 20, theme.ChooseBytes, theme.ChooseBytes + 1, 40 << 30} {
			for _, in := range []int{1, 2, n} {
				chooses = append(chooses, chooseCase{n, b, in})
			}
		}
	}
	type spreadCase struct{ N, K int }
	var spreads []spreadCase
	for n := 0; n <= 70; n += 7 {
		for _, k := range []int{0, 1, 2, 3, 8, 16} {
			spreads = append(spreads, spreadCase{n, k})
		}
	}
	palettes := []string{"dark", "light"}
	input, _ := json.Marshal(map[string]any{"chooses": chooses, "spreads": spreads, "drops": drops, "zooms": zooms, "durs": durs, "times": times, "scales": scales, "ticks": ticks, "bytes": bytes,
		"names": names, "kinds": kindIDs, "palettes": palettes, "nav": navSteps, "layouts": layouts, "slots": slotCases, "heights": heightCases, "filters": filterCases, "titles": titleCases, "busies": busies, "aligns": aligns})
	script := `
const vm = require("vm"), fs = require("fs");
const ctx = vm.createContext({ TextEncoder, INPUT: JSON.parse(fs.readFileSync(0, "utf8")) });
vm.runInContext(fs.readFileSync(process.argv[1], "utf8"), ctx);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
process.stdout.write(vm.runInContext(` + "`" + `JSON.stringify({
  durs: INPUT.durs.map(fmtDur),
  times: INPUT.times.map((p) => fmtTime(p.T, p.S)),
  steps: INPUT.scales.map((s) => tickStep(s, 110)),
  ticks: INPUT.ticks.map((p) => fmtTick(p.T, p.S)),
  bytes: INPUT.bytes.map(fmtBytes),
  colors: INPUT.palettes.flatMap((p) => INPUT.names.flatMap((n) => INPUT.kinds.map((k) => eventRGB(n, k, THEME.palettes[p])))),
  layouts: INPUT.layouts.map((l) => compactLayout(l.Subs, l.On)),
  titles: INPUT.titles.map((t) => tabTitle(t)),
  slots: INPUT.slots.map((c) => laneSlot(c.Kind, c.Lanes, c.Overview, c.Li, c.Stacked)),
  heights: INPUT.heights.map((c) => [trackHeight(c.Kind, c.Lanes), ...trackLane(c.Kind)]),
  filters: INPUT.filters.map((c) => ({ f: traceFilter(c.Keys || [], c.Off || []), off: filterOff(c.Keys || [], c.Only || [], c.Omit || []) })),
  aligns: INPUT.aligns.map((a) => alignSession(a.Ts, a.Mode)),
  busies: INPUT.busies.map((b) => busyColumns(b.Blocks, b.T0, b.Scale, b.N)),
  chooses: INPUT.chooses.map((c) => shouldChoose(c.Count, c.Bytes, c.Inputs)),
  spreads: INPUT.spreads.map((c) => spreadPick(c.N, c.K)),
  drops: INPUT.drops.map((d) => tabDropIndex(d.Lefts, d.Widths, d.From, d.Middle)),
  zooms: INPUT.zooms.map((z) => { const v = zoomViewport({ start: z.Start, scale: z.Scale }, z.Factor, z.X, z.Lo, z.Hi, z.W); return [v.start, v.scale]; }),
  nav: (() => {
    let vp = 0, vz = 0;
    return INPUT.nav.map((st) => {
      const [tp, tz] = navTargets(st.L, st.R, st.I, st.O, st.Shift, st.Speed);
      const r = navAdvance(vp, vz, tp, tz, st.DT);
      vp = r[0]; vz = r[1];
      return r;
    });
  })(),
})` + "`" + `, ctx));`
	dir := t.TempDir()
	themePath, sharedPath := filepath.Join(dir, "theme.js"), filepath.Join(dir, "shared.js")
	os.WriteFile(themePath, theme.JS(), 0o644)
	os.WriteFile(sharedPath, shared, 0o644)
	cmd := exec.Command(node, "-e", script, themePath, sharedPath)
	cmd.Stdin = strings.NewReader(string(input))
	outb, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node: %v: %s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var got struct {
		Durs, Times, Ticks, Bytes []string
		Steps                     []float64
		Colors                    [][3]int
		Nav                       [][4]float64
		Zooms                     [][2]float64
		Drops                     []int
		Chooses                   []bool
		Spreads                   [][]int
		Layouts                   []struct {
			Rows   []compactRow
			Height float64
		}
		Busies  [][]float64
		Aligns  [][]alignOut
		Slots   []int
		Heights [][3]float32
		Filters []struct {
			F   [2]any
			Off []bool
		}
		Titles []string
	}
	if err := json.Unmarshal(outb, &got); err != nil {
		t.Fatalf("%v: %s", err, outb)
	}
	for i, d := range durs {
		if want := fmtDur(d); got.Durs[i] != want {
			t.Errorf("fmtDur(%v): browser %q, desktop %q", d, got.Durs[i], want)
		}
	}
	for i, p := range times {
		if want := fmtTime(p.T, p.S); got.Times[i] != want {
			t.Errorf("fmtTime(%v, %v): browser %q, desktop %q", p.T, p.S, got.Times[i], want)
		}
	}
	for i, s := range scales {
		if want := tickStep(s, 110); got.Steps[i] != want {
			t.Errorf("tickStep(%v): browser %v, desktop %v", s, got.Steps[i], want)
		}
	}
	for i, p := range ticks {
		if want := fmtTick(p.T, p.S); got.Ticks[i] != want {
			t.Errorf("fmtTick(%v, %v): browser %q, desktop %q", p.T, p.S, got.Ticks[i], want)
		}
	}
	for i, b := range bytes {
		if want := fmtBytes(b); got.Bytes[i] != want {
			t.Errorf("fmtBytes(%v): browser %q, desktop %q", b, got.Bytes[i], want)
		}
	}
	i := 0
	for _, pn := range palettes {
		pal := theme.Palettes[pn]
		for _, n := range names {
			for _, k := range kinds {
				c := pal.EventColor(n, k)
				if want := [3]int{int(c.R), int(c.G), int(c.B)}; got.Colors[i] != want {
					t.Errorf("%s color of %q (%s): browser %v, desktop %v", pn, n, k, got.Colors[i], want)
				}
				i++
			}
		}
	}
	for i, l := range layouts {
		rows, height := compactLayout(l.Subs, l.On)
		if g := got.Layouts[i]; g.Height != height || len(g.Rows) != len(rows) || (len(rows) > 0 && !slices.Equal(g.Rows, rows)) {
			t.Errorf("compactLayout(%v, %v): browser %+v, desktop %+v %v", l.Subs, l.On, g, rows, height)
		}
	}
	for i, in := range titleCases {
		if i >= len(got.Titles) || got.Titles[i] != tabTitle(in) {
			t.Errorf("tabTitle(%q): browser %q, desktop %q", in, got.Titles, tabTitle(in))
		}
	}
	if len(got.Slots) != len(slotCases) {
		t.Fatalf("laneSlot: %d answers for %d cases", len(got.Slots), len(slotCases))
	}
	for i, c := range slotCases {
		if want := laneSlot(c.Kind, c.Lanes, c.Overview, c.Li, c.Stacked); got.Slots[i] != want {
			t.Errorf("laneSlot(%+v): browser %d, desktop %d", c, got.Slots[i], want)
		}
	}
	if len(got.Heights) != len(heightCases) {
		t.Fatalf("trackHeight: %d answers for %d cases", len(got.Heights), len(heightCases))
	}
	for i, c := range heightCases {
		lh, pad := trackLane(c.Kind)
		if want := [3]float32{trackHeight(c.Kind, c.Lanes), lh, pad}; got.Heights[i] != want {
			t.Errorf("trackHeight(%+v): browser %v, desktop %v", c, got.Heights[i], want)
		}
	}
	if len(got.Filters) != len(filterCases) {
		t.Fatalf("traceFilter: %d answers for %d cases", len(got.Filters), len(filterCases))
	}
	for i, c := range filterCases {
		param, names := traceFilter(c.Keys, c.Off)
		g := got.Filters[i]
		var gotNames []string
		if l, ok := g.F[1].([]any); ok {
			for _, n := range l {
				gotNames = append(gotNames, n.(string))
			}
		}
		if g.F[0] != param || !slices.Equal(gotNames, names) {
			t.Errorf("traceFilter(%v, %v): browser %v, desktop %q %q", c.Keys, c.Off, g.F, param, names)
		}
		off := filterOff(c.Keys, c.Only, c.Omit)
		if !slices.Equal(g.Off, off) && !(len(off) == 0 && len(g.Off) == 0) {
			t.Errorf("filterOff(%v, %v, %v): browser %v, desktop %v", c.Keys, c.Only, c.Omit, g.Off, off)
		}
	}
	for i, a := range aligns {
		if want := alignSession(a.Ts, a.Mode); !slices.Equal(got.Aligns[i], want) {
			t.Errorf("alignTraces case %d (%s): browser %+v, desktop %+v", i, a.Mode, got.Aligns[i], want)
		}
	}
	for i, b := range busies {
		want := busyColumns(b.Blocks, b.T0, b.Scale, b.N, nil)
		for c, v := range want {
			if math.Abs(got.Busies[i][c]-v) > 1e-9 {
				t.Fatalf("busyColumns case %d column %d: browser %v, desktop %v", i, c, got.Busies[i][c], v)
			}
		}
	}
	for i, c := range chooses {
		if want := shouldChoose(c.Count, c.Bytes, c.Inputs); got.Chooses[i] != want {
			t.Fatalf("shouldChoose%+v: browser %v, desktop %v", c, got.Chooses[i], want)
		}
	}
	for i, c := range spreads {
		if want := spreadPick(c.N, c.K); !slices.Equal(got.Spreads[i], want) && (len(want) > 0 || len(got.Spreads[i]) > 0) {
			t.Fatalf("spreadPick%+v: browser %v, desktop %v", c, got.Spreads[i], want)
		}
	}
	for i, d := range drops {
		if want := tabDropIndex(d.Lefts, d.Widths, d.From, d.Middle); got.Drops[i] != want {
			t.Fatalf("tabDropIndex case %d (%+v): browser %d, desktop %d", i, d, got.Drops[i], want)
		}
	}
	for i, z := range zooms {
		v := zoomViewport(viewport{start: z.Start, scale: z.Scale}, z.Factor, z.X, z.Lo, z.Hi, z.W)
		for j, want := range [2]float64{v.start, v.scale} {
			if g := got.Zooms[i][j]; math.Abs(g-want) > 1e-9*max(1, math.Abs(want)) {
				t.Fatalf("zoomViewport case %d (%+v) value %d: browser %v, desktop %v", i, z, j, g, want)
			}
		}
		// The time under x stays, unless the view's bounds or an end of the
		// times reaching its edge (zooming out) hold it: no drift.
		tx, after := z.Start+z.X*z.Scale, v.start+z.X*v.scale
		span, dur := v.scale*z.W, z.Hi-z.Lo
		held := v.start == z.Lo-span*0.8 || v.start == z.Hi-span*0.2 ||
			(v.scale > z.Scale && (v.start <= z.Lo || v.start+span >= z.Hi || span >= dur))
		if !held && math.Abs(after-tx) > 1e-6*max(1, math.Abs(tx)) {
			t.Errorf("zoomViewport case %d (%+v): the time under x moved from %v to %v", i, z, tx, after)
		}
		// Zooming out never brings in empty time that was not there,
		// beyond what a view longer than the times needs.
		if v.scale > z.Scale {
			need, eps := max(span-dur, 0), 1e-6*span
			if l, l0 := z.Lo-v.start, max(z.Lo-z.Start, 0); l > max(l0, need)+eps {
				t.Errorf("zoomViewport case %d (%+v): zooming out brought in %v ns of empty time on the left", i, z, l)
			}
			if r, r0 := v.start+span-z.Hi, max(z.Start+z.Scale*z.W-z.Hi, 0); r > max(r0, need)+eps {
				t.Errorf("zoomViewport case %d (%+v): zooming out brought in %v ns of empty time on the right", i, z, r)
			}
		}
	}
	var vp, vz float64
	for i, st := range navSteps {
		var d navKey
		for _, h := range []struct {
			on bool
			k  navKey
		}{{st.L, navLeft}, {st.R, navRight}, {st.I, navIn}, {st.O, navOut}} {
			if h.on {
				d |= h.k
			}
		}
		tp, tz := navTargets(d, st.Shift, st.Speed)
		var dp, dz float64
		vp, vz, dp, dz = navAdvance(vp, vz, tp, tz, st.DT)
		for j, want := range [4]float64{vp, vz, dp, dz} {
			if g := got.Nav[i][j]; math.Abs(g-want) > 1e-12*max(1, math.Abs(want)) {
				t.Fatalf("navigation step %d value %d: browser %v, desktop %v", i, j, g, want)
			}
		}
	}
	if vp != 0 || vz != 0 {
		t.Errorf("navigation did not come to rest after the keys went up: %v %v", vp, vz)
	}
}
