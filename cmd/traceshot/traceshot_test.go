package main

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"tracelight/internal/torchgen"
	"tracelight/internal/trace"
)

func TestParseTime(t *testing.T) {
	for in, want := range map[string]int64{
		"1.25s": 1_250_000_000, "300ms": 300_000_000, "40us": 40_000, "40µs": 40_000, "500ns": 500, "2": 2_000_000_000, " 1.5 ms ": 1_500_000,
	} {
		if got, err := parseTime(in); err != nil || got != want {
			t.Errorf("parseTime(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ms", "1h", "x1s", "NaNs"} {
		if _, err := parseTime(in); err == nil {
			t.Errorf("parseTime(%q): no error", in)
		}
	}
}

func TestParseWindow(t *testing.T) {
	const span = 10_000_000_000
	for in, want := range map[string][2]int64{
		"1s:2s":      {1e9, 2e9},
		"1.2s+150ms": {1_200_000_000, 1_350_000_000},
		":2s":        {0, 2e9},
		"9s:":        {9e9, span},
	} {
		a, b, err := parseWindow(in, span)
		if err != nil || a != want[0] || b != want[1] {
			t.Errorf("parseWindow(%q) = %d, %d, %v; want %v", in, a, b, err, want)
		}
	}
	for _, in := range []string{"2s:1s", "1s", "a:b"} {
		if _, _, err := parseWindow(in, span); err == nil {
			t.Errorf("parseWindow(%q): no error", in)
		}
	}
}

// fmtArg's times read back exactly.
func TestFmtArg(t *testing.T) {
	for _, ns := range []int64{0, 1, 999, 1_000_000, 23_054_796, 1_200_000_000, 11_338_196_343, 123_456_789_012} {
		got, err := parseTime(fmtArg(ns))
		if err != nil || got != ns {
			t.Errorf("parseTime(fmtArg(%d) = %q) = %d, %v", ns, fmtArg(ns), got, err)
		}
	}
}

func TestParseRanks(t *testing.T) {
	set, err := parseRanks("0, 2,4-7")
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]bool{0: true, 1: false, 2: true, 3: false, 4: true, 7: true, 8: false} {
		if set.has(n) != want {
			t.Errorf("has(%d) = %v", n, !want)
		}
	}
	if s, _ := parseRanks(""); !s.has(99) {
		t.Error("an empty selection selects all")
	}
	for _, bad := range []string{"a", "3-1", "1-"} {
		if _, err := parseRanks(bad); err == nil {
			t.Errorf("parseRanks(%q): no error", bad)
		}
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"/runs/rank3.sqlite": "rank3", "/r/rank0.pt.trace.json.gz": "rank0", "/r/job.json#rank8": "rank8", "/r/rank-16.nsys-rep": "rank-16",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
	if labelNum("rank-16") != 16 || labelNum("trace") != -1 {
		t.Error("labelNum")
	}
}

// torchJob writes a synthetic job of 4 ranks: a file each, and the same
// merged into one.
func torchJob(t *testing.T) (dir, merged string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "ranks")
	o := torchgen.Options{Ranks: 4, Steps: 4, Layers: 2}
	if _, err := torchgen.WriteDir(dir, o, true); err != nil {
		t.Fatal(err)
	}
	merged = filepath.Join(base, "job.json")
	if err := torchgen.WriteMerged(merged, o, false); err != nil {
		t.Fatal(err)
	}
	return dir, merged
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if err := run(args, &out, &errb); err != nil {
		t.Fatalf("traceshot %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}

func pngSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestCommands(t *testing.T) {
	dir, merged := torchJob(t)
	out := runOK(t, "info", dir)
	for _, want := range []string{"4 rank(s)", "Top GPU work", "ProfilerStep#<n>"} {
		if !strings.Contains(out, want) {
			t.Errorf("info: no %q in\n%s", want, out)
		}
	}
	out = runOK(t, "find", "-name", "^ProfilerStep", "-ranks", "1", merged)
	if !strings.Contains(out, "4 match(es)") {
		t.Errorf("find: want the 4 steps of rank 1:\n%s", out)
	}
	// The window find prints frames the step.
	m := regexp.MustCompile(`(\S+ms|\S+s):(\S+)\s+ProfilerStep`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("find: no window in\n%s", out)
	}
	runOK(t, "gaps", "-min", "1us", dir)

	png1 := filepath.Join(t.TempDir(), "a.png")
	out = runOK(t, "-ranks", "0-1", "-window", m[1]+":"+m[2], "-width", "1000", "-scale", "1", "-o", png1, merged)
	if !strings.Contains(out, "2 rank(s), layout rows") {
		t.Errorf("render: %s", out)
	}
	if w, h := pngSize(t, png1); w != 1000 || h < 300 {
		t.Errorf("render: %d×%d px", w, h)
	}

	// -around frames the nth step of a rank; summary, light, 2×.
	png2 := filepath.Join(t.TempDir(), "b.png")
	out = runOK(t, "-around", "^ProfilerStep", "-nth", "2", "-at-rank", "2", "-layout", "summary", "-theme", "light",
		"-highlight", "nccl", "-mark", "40ms", "-width", "800", "-o", png2, dir)
	if !strings.Contains(out, "layout summary") {
		t.Errorf("render: %s", out)
	}
	if w, _ := pngSize(t, png2); w != 1600 {
		t.Errorf("render at 2×: %d px wide", w)
	}

	// A small -max-height leaves rows out, and auto turns to the summary.
	out = runOK(t, "-max-height", "500", "-scale", "1", "-o", filepath.Join(t.TempDir(), "c.png"), dir)
	if !strings.Contains(out, "layout summary") {
		t.Errorf("auto: %s", out)
	}
}

func TestErrors(t *testing.T) {
	dir, _ := torchJob(t)
	for _, args := range [][]string{
		{},
		{"-ranks", "9", dir},
		{"-window", "2s:1s", dir},
		{"-around", "^no such thing$", dir},
		{"-rows", "gpu,disk", dir},
		{"-layout", "grid", dir},
		{"-theme", "pink", dir},
		{"find", dir},
		{filepath.Join(dir, "missing.json")},
	} {
		var out, errb bytes.Buffer
		if err := run(args, &out, &errb); err == nil {
			t.Errorf("traceshot %v: no error", args)
		}
	}
}

// testRank builds a rank whose GPU runs kernels, given as {start, end,
// stream} in ns, all named name, its first event placed at offset on the
// session's axis (an offset of the first kernel's start keeps the times).
func testRank(num int, offset int64, name string, kernels [][3]int64) *rank {
	b := trace.NewBuilder(fmt.Sprintf("rank%d.sqlite", num))
	for _, k := range kernels {
		b.AddKernel(trace.Event{Start: k[0], End: k[1], Name: b.Intern(name)}, trace.KernelInfo{Stream: int32(k[2])})
	}
	tr := b.Finish()
	return &rank{path: tr.Path, label: fmt.Sprintf("rank%d", num), num: num, tr: tr, offset: offset}
}

// Collectives match one to one across ranks, by their ends; the spread of
// their starts is the imbalance; each rank's first collective (which says
// when its capture began, not how balanced it is) is left out.
func TestFindCollectives(t *testing.T) {
	const ar = "ncclDevKernel_AllReduce_Sum_f32_RING_LL"
	ms := int64(1_000_000)
	// Rank 0: a warm-up collective, then two at once on two streams, the
	// one ending at 20 ms and the one at 20.05 ms, then one at 40 ms.
	r0 := testRank(0, 2*ms, ar, [][3]int64{{2 * ms, 3 * ms, 7}, {10 * ms, 20 * ms, 7}, {11 * ms, 20*ms + 50_000, 9}, {30 * ms, 40 * ms, 7}})
	// Rank 1 comes 4 ms late to the first pair, 1 ms to the last.
	r1 := testRank(1, 2*ms, ar, [][3]int64{{2 * ms, 3 * ms, 7}, {14 * ms, 20 * ms, 7}, {15 * ms, 20*ms + 50_000, 9}, {31 * ms, 40 * ms, 7}})
	ss := &session{ranks: []*rank{r0, r1}, span: 41 * ms}
	cs := findCollectives(ss, 0, ss.span, nil, 2)
	if len(cs) != 3 {
		t.Fatalf("%d collectives, want 3 (the warm-up left out)", len(cs))
	}
	for i, want := range []struct{ start, spread int64 }{{10 * ms, 4 * ms}, {11 * ms, 4 * ms}, {30 * ms, ms}} {
		c := cs[i]
		if c.start != want.start || c.spread != want.spread || c.arr[c.last].r != r1 || c.arr[c.first].r != r0 {
			t.Errorf("collective %d: start %d spread %d last rank %d; want start %d spread %d last rank 1", i, c.start, c.spread, c.arr[c.last].r.num, want.start, want.spread)
		}
	}
	// The two that end together took a kernel each of rank 1.
	if cs[0].arr[1].start == cs[1].arr[1].start {
		t.Error("two collectives matched the same kernel")
	}
	// The window: around the arrivals of a long collective; the whole one if asked.
	c := cs[2] // 1 ms spread, 10 ms long
	if a, b := c.window(false); a > 30*ms || b < 31*ms || b > 33*ms {
		t.Errorf("window %d–%d: want the arrivals, 30–31 ms", a, b)
	}
	if _, b := c.window(true); b < 40*ms {
		t.Errorf("whole window ends at %d, before the collective", b)
	}
	if got := arrivalInfo(c, r1); !strings.Contains(got, "last") {
		t.Errorf("arrivalInfo: %q", got)
	}
	// Ranks that do not share a collective match nothing.
	r2 := testRank(2, 2*ms, ar, [][3]int64{{2 * ms, 3 * ms, 7}, {50 * ms, 60 * ms, 7}})
	if cs := findCollectives(&session{ranks: []*rank{r0, r2}, span: 61 * ms}, 0, 61*ms, nil, 2); len(cs) != 0 {
		t.Errorf("%d collectives matched between ranks that share none", len(cs))
	}
}

func TestContextWindow(t *testing.T) {
	for _, c := range []struct{ t0, t1, span, a, b int64 }{
		{100, 110, 1000, 45, 165}, // 12× around the middle
		{0, 10, 1000, 0, 120},     // against the start
		{990, 1000, 1000, 880, 1000},
		{0, 500, 1000, 0, 1000}, // longer than the trace
	} {
		if w := contextWindow(c.t0, c.t1, c.span, 12); w != [2]int64{c.a, c.b} {
			t.Errorf("contextWindow(%d, %d): %v, want [%d %d]", c.t0, c.t1, w, c.a, c.b)
		}
	}
	if got := fmtTick(12_250_000_000, 50_000_000); got != "12.25 s" {
		t.Errorf("fmtTick: %q", got)
	}
}

func TestImbalanceCommand(t *testing.T) {
	dir, _ := torchJob(t)
	out := runOK(t, "imbalance", "-limit", "3", dir)
	if !strings.Contains(out, "collective(s) matched") || !strings.Contains(out, "By rank") {
		t.Fatalf("imbalance:\n%s", out)
	}
	png1 := filepath.Join(t.TempDir(), "z.png")
	out = runOK(t, "-imbalance", "1", "-scale", "1", "-o", png1, dir)
	if !strings.Contains(out, "started it") {
		t.Errorf("render -imbalance: %s", out)
	}
	if w, h := pngSize(t, png1); w != 1600 || h < 400 {
		t.Errorf("render -imbalance: %d×%d px", w, h)
	}
	// A zoomed window has a context strip; -context off leaves it out.
	h := func(args ...string) int {
		p := filepath.Join(t.TempDir(), "c.png")
		runOK(t, append(append([]string{"-scale", "1", "-legend", "0", "-ranks", "0", "-o", p}, args...), dir)...)
		_, h := pngSize(t, p)
		return h
	}
	if on, off := h("-window", "50ms+2ms"), h("-window", "50ms+2ms", "-context", "off"); on <= off {
		t.Errorf("context strip: %d px tall with it, %d without", on, off)
	}
	var o, e bytes.Buffer
	if err := run([]string{"-imbalance", "999", dir}, &o, &e); err == nil {
		t.Error("-imbalance 999: no error")
	}
}
