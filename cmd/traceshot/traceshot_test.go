package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"tracelight/internal/torchgen"
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
