package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tracelight/internal/torchgen"
)

// stackNames are a stack's functions, outermost first.
func stackNames(st []StackFrame) []string {
	var out []string
	for _, f := range st {
		out = append(out, f.Name)
	}
	return out
}

// A PyTorch profile (torchgen's): its GPU work on its streams, launched by
// CUDA API calls of the right threads (correlated), operators, Python
// calls and ProfilerStep annotations on the threads; the rank and the wall
// clock from the file; the Python stack of a forward kernel (where it was
// launched), none for a backward kernel (autograd's thread runs no
// Python), and a Python call's own ending in it. Gzipped, or with times
// from baseTimeNanoseconds, it reads the same.
func TestLoadTorch(t *testing.T) {
	dir := t.TempDir()
	paths, err := torchgen.WriteDir(filepath.Join(dir, "plain"), torchgen.Options{Ranks: 2, Steps: 3, Layers: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	if f := Detect(paths[1]); f != FormatTorch {
		t.Fatalf("Detect: %q", f)
	}
	tr, err := Open(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	// What the file holds, counted from its JSON.
	raw, _ := os.ReadFile(paths[1])
	var doc struct {
		TraceEvents []struct {
			Ph, Cat string
		} `json:"traceEvents"`
	}
	json.Unmarshal(raw, &doc)
	want := map[string]int{}
	for _, e := range doc.TraceEvents {
		if e.Ph == "X" {
			want[e.Cat]++
		}
	}
	for cat, k := range map[string]Kind{"kernel": KindKernel, "gpu_memcpy": KindMemcpy, "cuda_runtime": KindRuntime, "cpu_op": KindCPUOp,
		"python_function": KindPython, "user_annotation": KindNVTX} {
		if tr.Counts[k] != want[cat] || want[cat] == 0 {
			t.Errorf("%s: %d events, the file has %d", k, tr.Counts[k], want[cat])
		}
	}
	if tr.Rank != 1 || tr.SessionStartUTC < 1_792_483_000_000_000_000 || tr.Devices[0] != "NVIDIA H100 80GB HBM3" {
		t.Errorf("rank %d, session start %d, devices %v", tr.Rank, tr.SessionStartUTC, tr.Devices)
	}
	kinds := map[TrackKind]int{}
	var names []string
	for _, tk := range tr.Tracks {
		kinds[tk.Kind]++
		names = append(names, tk.Name)
	}
	if kinds[TrackStream] != 2 || kinds[TrackRuntime] != 2 || kinds[TrackCPU] != 2 || kinds[TrackPython] != 1 || kinds[TrackNVTX] != 1 {
		t.Errorf("tracks %v: %q", kinds, names)
	}
	if !slices.Contains(names, "python3 (41217) · Python") || !slices.Contains(names, "pt_autograd_0 (41226) · CPU ops") || !slices.Contains(names, "python3 (41217) · Annotations") {
		t.Errorf("track names %q", names)
	}
	if len(tr.Warnings) != 0 {
		t.Errorf("warnings %q", tr.Warnings)
	}

	// Every kernel has its launch; the stacks.
	var fwd, bwd Ref
	for ti, tk := range tr.Tracks {
		if tk.Kind != TrackStream {
			continue
		}
		for i := range tk.Lanes[0].Events {
			r := Ref{Track: int32(ti), Index: int32(i)}
			if _, ok := tr.Correlated(r); !ok {
				t.Fatalf("kernel %s has no launch", tr.Name(tr.Event(r)))
			}
			name := tr.Name(tr.Event(r))
			if strings.Contains(name, "flash_fwd") && fwd == (Ref{}) {
				fwd = r
			}
			if strings.Contains(name, "flash_bwd") && bwd == (Ref{}) {
				bwd = r
			}
		}
	}
	st := stackNames(tr.PythonStack(fwd))
	wantTail := []string{"model.py(96): forward", "torch/nn/modules/module.py(1739): _wrapped_call_impl", "torch/nn/modules/module.py(1750): _call_impl",
		"model.py(61): forward", "torch/nn/modules/module.py(1739): _wrapped_call_impl", "torch/nn/modules/module.py(1750): _call_impl",
		"model.py(39): forward", "torch/nn/functional.py(5865): scaled_dot_product_attention"}
	if len(st) < len(wantTail) || st[0] != "runpy.py(198): _run_module_as_main" || !slices.Equal(st[len(st)-len(wantTail):], wantTail) {
		t.Errorf("forward kernel's stack:\n  %s", strings.Join(st, "\n  "))
	}
	if st := tr.PythonStack(bwd); len(st) != 0 {
		t.Errorf("backward kernel's stack: %q", stackNames(st))
	}
	frames := tr.PythonStack(fwd)
	last := frames[len(frames)-1]
	own := tr.PythonStack(last.Ref)
	if !slices.Equal(stackNames(own), stackNames(frames)) {
		t.Errorf("the frame's own stack %q, want %q", stackNames(own), stackNames(frames))
	}

	// Gzipped, and with times from baseTimeNanoseconds: the same trace.
	for _, o := range []struct {
		name string
		opt  torchgen.Options
		gz   bool
	}{{"gz", torchgen.Options{Ranks: 2, Steps: 3, Layers: 2}, true}, {"relative", torchgen.Options{Ranks: 2, Steps: 3, Layers: 2, Relative: true}, false}} {
		ps, err := torchgen.WriteDir(filepath.Join(dir, o.name), o.opt, o.gz)
		if err != nil {
			t.Fatal(err)
		}
		other, err := Open(ps[1])
		if err != nil {
			t.Fatal(err)
		}
		if other.SessionStartUTC+other.Start != tr.SessionStartUTC+tr.Start || other.Duration() != tr.Duration() || other.EventCount() != tr.EventCount() {
			t.Errorf("%s: start %d+%d, %d events over %d; plain %d+%d, %d over %d", o.name, other.SessionStartUTC, other.Start, other.EventCount(), other.Duration(),
				tr.SessionStartUTC, tr.Start, tr.EventCount(), tr.Duration())
		}
	}
	// Without with_stack: no Python calls, and the warning says how.
	ps, _ := torchgen.WriteDir(filepath.Join(dir, "nostack"), torchgen.Options{Steps: 2, Layers: 1, NoStack: true}, false)
	ns, err := Open(ps[0])
	if err != nil {
		t.Fatal(err)
	}
	if ns.Counts[KindPython] != 0 || !slices.ContainsFunc(ns.Warnings, func(w string) bool { return strings.Contains(w, "with_stack=True") }) {
		t.Errorf("without stacks: %d Python calls, warnings %q", ns.Counts[KindPython], ns.Warnings)
	}
}

// The ranks' collective kernels end at the same moment (torchgen's job):
// their ends differ from rank to rank by the same offset (the clocks'),
// which aligning on them recovers.
func TestTorchCollectives(t *testing.T) {
	paths, err := torchgen.WriteDir(t.TempDir(), torchgen.Options{Ranks: 3, Steps: 3, Layers: 3}, false)
	if err != nil {
		t.Fatal(err)
	}
	var ends [][]int64
	for _, p := range paths {
		tr, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		c := tr.Collectives()["allreduce"]
		abs := make([]int64, len(c))
		for i, e := range c {
			abs[i] = tr.SessionStartUTC + tr.Start + e
		}
		ends = append(ends, abs)
	}
	for r := 1; r < len(ends); r++ {
		if len(ends[r]) != len(ends[0]) || len(ends[0]) != 9 {
			t.Fatalf("rank %d: %d all-reduces, rank 0 %d", r, len(ends[r]), len(ends[0]))
		}
		off := ends[r][0] - ends[0][0]
		for i := range ends[r] {
			if d := ends[r][i] - ends[0][i] - off; d < -1000 || d > 1000 {
				t.Errorf("rank %d, all-reduce %d: off by %d ns more than the first", r, i, d)
			}
		}
	}
}

// Times in microseconds convert to nanoseconds exactly, at epoch times too.
func TestUsToNs(t *testing.T) {
	for in, want := range map[string]int64{"1792483200000000.123": 1792483200000000123, "12": 12000, "0.5": 500, "1.2345": 1234, "-3.25": -3250, "1.5e3": 1500000} {
		if got, ok := usToNs(json.Number(in)); !ok || got != want {
			t.Errorf("usToNs(%s) = %d, want %d", in, got, want)
		}
	}
}

// Files named .json are traces only if they are Chrome traces; any
// Chrome trace reads: spans of "B" and "E", processes and threads named by
// strings, instants as marks.
func TestChromeTrace(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"lr": 0.1, "events": []}`), 0o644)
	if f := Detect(cfg); f != "" {
		t.Errorf("a JSON config read as %q", f)
	}
	p := filepath.Join(dir, "app.json")
	os.WriteFile(p, []byte(`{"traceEvents": [
		{"ph": "M", "name": "thread_name", "pid": "renderer", "tid": 2, "args": {"name": "main"}},
		{"ph": "B", "name": "load", "pid": "renderer", "tid": 2, "ts": 100},
		{"ph": "B", "name": "parse", "pid": "renderer", "tid": 2, "ts": 120},
		{"ph": "E", "pid": "renderer", "tid": 2, "ts": 180},
		{"ph": "E", "pid": "renderer", "tid": 2, "ts": 300},
		{"ph": "X", "cat": "gc", "name": "minor gc", "pid": "renderer", "tid": 3, "ts": 150, "dur": 10.5},
		{"ph": "i", "cat": "mark", "name": "ready", "pid": "renderer", "tid": 2, "ts": 310},
		{"ph": "C", "name": "heap", "pid": "renderer", "ts": 200, "args": {"used": 12}}
	]}`), 0o644)
	tr, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if g := tr.Tracks[0].Group; g != "Process 1048576 · renderer" {
		t.Errorf("group %q", g)
	}
	var got []string
	for _, tk := range tr.Tracks {
		for li, l := range tk.Lanes {
			for _, e := range l.Events {
				got = append(got, tk.Name+"|"+tr.Name(&e)+"|"+e.Kind.String()+"|"+string(rune('0'+li))+"|"+itoa(e.End-e.Start))
			}
		}
	}
	slices.Sort(got)
	want := []string{"Thread 3 · CPU ops|minor gc|CPU op|0|10500", "main (2) · Annotations|ready|NVTX mark|0|0", "main (2) · CPU ops|load|CPU op|0|200000", "main (2) · CPU ops|parse|CPU op|1|60000"}
	if !slices.Equal(got, want) {
		t.Errorf("events:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The scanner cuts out the events exactly, however the input is chunked:
// strings with escaped quotes and backslashes, braces in strings, nested
// values; with a buffer of 16 bytes every boundary falls somewhere odd.
func TestJSONScanner(t *testing.T) {
	var vals []string
	for i := range 200 {
		name := strings.Repeat(`\\`, i%5) + `q\"{` + strings.Repeat("x", i%37) + `}]\"` + strings.Repeat(`\\`, i%3) + `é`
		vals = append(vals, `{"ph": "X", "name": "`+name+`", "args": {"a": [1, {"b": "]}"}], "c": null}, "ts": `+itoa(int64(i))+`.5}`)
	}
	doc := `{"traceEvents": [` + strings.Join(vals, ",\n ") + `], "x": 1}`
	if !json.Valid([]byte(doc)) {
		t.Fatal("the test's JSON is not valid")
	}
	for _, size := range []int{16, 17, 64, 4096} {
		sc := &jsonScanner{br: bufio.NewReaderSize(strings.NewReader(doc), size)}
		if c, _ := sc.next(); c != '{' {
			t.Fatal("no object")
		}
		c, _ := sc.next()
		key, _ := sc.value(c, nil)
		sc.next() // :
		if c, _ := sc.next(); string(key) != `"traceEvents"` || c != '[' {
			t.Fatalf("key %s", key)
		}
		var got []string
		for {
			c, err := sc.next()
			if err != nil || c == ']' {
				break
			}
			if c == ',' {
				continue
			}
			raw, err := sc.value(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, string(raw))
		}
		if !slices.Equal(got, vals) {
			for i := range min(len(got), len(vals)) {
				if got[i] != vals[i] {
					t.Fatalf("buffer %d: value %d\n  got  %s\n  want %s", size, i, got[i], vals[i])
				}
			}
			t.Fatalf("buffer %d: %d values, want %d", size, len(got), len(vals))
		}
	}
}

// A gzipped trace of several chunks of what is decompressed ahead
// (asyncReader's 1 MiB) reads as the same trace uncompressed does.
func TestTorchGzipLarge(t *testing.T) {
	dir := t.TempDir()
	o := torchgen.Options{Steps: 24, Layers: 6}
	plain, _ := torchgen.WriteDir(filepath.Join(dir, "plain"), o, false)
	gz, _ := torchgen.WriteDir(filepath.Join(dir, "gz"), o, true)
	if fi, _ := os.Stat(plain[0]); fi.Size() < 4<<20 {
		t.Fatalf("only %d bytes uncompressed", fi.Size())
	}
	a, err := Open(plain[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(gz[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.EventCount() != b.EventCount() || a.Duration() != b.Duration() || len(a.Names) != len(b.Names) {
		t.Errorf("gzipped: %d events over %d, %d names; plain %d over %d, %d", b.EventCount(), b.Duration(), len(b.Names), a.EventCount(), a.Duration(), len(a.Names))
	}
}

// A file of several ranks merged (torchgen.WriteMerged: processes named
// "rank<N>:<pid>", the ranks one after another, the first event not the
// earliest, no distributedInfo) reads as a trace a rank: each the rank's
// own (the same events as the rank written alone), on the file's clock;
// no time is before the trace's start, and no stream's work is spread over
// more lanes than it overlaps. The file itself opens as its first rank,
// saying so.
func TestTorchMerged(t *testing.T) {
	dir := t.TempDir()
	o := torchgen.Options{Ranks: 3, Steps: 3, Layers: 2, NoStack: true}
	merged := filepath.Join(dir, "job.json.gz")
	if err := torchgen.WriteMerged(merged, o, true); err != nil {
		t.Fatal(err)
	}
	alone, _ := torchgen.WriteDir(filepath.Join(dir, "alone"), o, false)
	members := Members(merged)
	if len(members) != 3 || members[1] != merged+"#rank1" || Detect(members[2]) != FormatTorch {
		t.Fatalf("members %q", members)
	}
	var monos []int64
	for r, m := range members {
		tr, err := Open(m)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := Open(alone[r])
		if err != nil {
			t.Fatal(err)
		}
		if tr.Rank != r || tr.Clock != merged || tr.Path != m || tr.EventCount() != ref.EventCount() || tr.Counts[KindKernel] != ref.Counts[KindKernel] {
			t.Errorf("rank %d: rank %d, clock %q, %d events (%d kernels); alone %d (%d)", r, tr.Rank, tr.Clock, tr.EventCount(), tr.Counts[KindKernel], ref.EventCount(), ref.Counts[KindKernel])
		}
		monos = append(monos, tr.SessionStartMono)
		for _, tk := range tr.Tracks {
			for _, l := range tk.Lanes {
				if len(l.Events) > 0 && l.Events[0].Start < 0 {
					t.Errorf("rank %d: %s starts at %d", r, tk.Name, l.Events[0].Start)
				}
			}
			if tk.Kind == TrackStream && len(tk.Lanes) > 2 {
				t.Errorf("rank %d: %s over %d lanes", r, tk.Name, len(tk.Lanes))
			}
		}
		if info := (OpenOptions{}).ReadInfo(m); info.Rank != r || info.Clock != merged || info.SessionStartMono != tr.SessionStartMono {
			t.Errorf("ReadInfo(%s) = %+v", m, info)
		}
	}
	if monos[0] == 0 || monos[0] != monos[1] || monos[1] != monos[2] {
		t.Errorf("the ranks' clocks %v", monos)
	}
	whole, err := Open(merged)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Rank != 0 || !slices.ContainsFunc(whole.Warnings, func(w string) bool { return strings.Contains(w, "holds 3 ranks merged") }) {
		t.Errorf("the file: rank %d, warnings %q", whole.Rank, whole.Warnings)
	}
}

// Events before 0 (a trace's times are not always after it) pack into as
// many lanes as overlap, as later ones do.
func TestPackNegative(t *testing.T) {
	var ev []Event
	for i := range int64(100) {
		ev = append(ev, Event{Start: -1000 + i*10, End: -1000 + i*10 + 15}) // two overlap at a time
	}
	if lanes := Pack(ev); len(lanes) != 2 {
		t.Errorf("%d lanes, want 2", len(lanes))
	}
}

// The operators an event ran under: for a forward kernel, those around the
// CUDA call that launched it (the step's annotation outermost); for a
// backward kernel, autograd's on its thread; an operator's own, ending in
// it.
func TestOperatorStack(t *testing.T) {
	paths, _ := torchgen.WriteDir(t.TempDir(), torchgen.Options{Steps: 2, Layers: 2}, false)
	tr, err := Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	find := func(part string) Ref {
		for ti, tk := range tr.Tracks {
			if tk.Kind != TrackStream {
				continue
			}
			for i := range tk.Lanes[0].Events {
				if strings.Contains(tr.Name(&tk.Lanes[0].Events[i]), part) {
					return Ref{Track: int32(ti), Index: int32(i)}
				}
			}
		}
		t.Fatalf("no kernel %s", part)
		return Ref{}
	}
	fwd := stackNames(tr.OperatorStack(find("flash_fwd")))
	if !slices.Equal(fwd, []string{"ProfilerStep#2", "aten::scaled_dot_product_attention", "aten::_scaled_dot_product_flash_attention", "aten::_flash_attention_forward"}) {
		t.Errorf("forward kernel's operators %q", fwd)
	}
	bwdOps := tr.OperatorStack(find("flash_bwd"))
	bwd := stackNames(bwdOps)
	if !slices.Equal(bwd, []string{"autograd::engine::evaluate_function: ScaledDotProductFlashAttentionBackward0", "ScaledDotProductFlashAttentionBackward0", "aten::_flash_attention_backward"}) {
		t.Errorf("backward kernel's operators %q", bwd)
	}
	own := stackNames(tr.OperatorStack(bwdOps[1].Ref))
	if !slices.Equal(own, bwd[:2]) {
		t.Errorf("an operator's own stack %q, want %q", own, bwd[:2])
	}
}

// A thread's rows read as its stack, outermost first: its ranges (NVTX,
// annotations), its Python calls, its operators, and at the bottom the CUDA
// API calls, the lowest level.
func TestThreadOrder(t *testing.T) {
	paths, err := torchgen.WriteDir(t.TempDir(), torchgen.Options{Ranks: 1, Steps: 2, Layers: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := LoadTorch(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []TrackKind{TrackNVTX, TrackPython, TrackCPU, TrackRuntime}
	byThread := map[int64][]TrackKind{}
	for _, tk := range tr.Tracks {
		if tk.Kind != TrackStream {
			byThread[tk.Thread] = append(byThread[tk.Thread], tk.Kind)
		}
	}
	full := 0
	for th, kinds := range byThread {
		last := -1
		for _, k := range kinds {
			i := slices.Index(want, k)
			if i <= last {
				t.Errorf("thread %d: rows %v, not in the order %v", th, kinds, want)
				break
			}
			last = i
		}
		if len(kinds) == len(want) {
			full++
		}
	}
	if full == 0 {
		t.Errorf("no thread with all four kinds of rows: %v", byThread)
	}
}
