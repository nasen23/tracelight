package viewer

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/torchgen"
	"tracelight/internal/trace"
)

// A PyTorch profile's ranks (torchgen's) open as a session from their
// folder, lined up on their collective kernels; a forward kernel's details
// show the Python stack where it was launched, the innermost frame first,
// and a click on a frame selects that call; Top CPU ops sums the
// operators.
func TestTorchProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	if _, err := torchgen.WriteDir(dir, torchgen.Options{Ranks: 3, Steps: 3, Layers: 2}, false); err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	s.SetAnimations(false)
	s.SetCompact("off")
	tt := ui.NewTester(s.View, w, h)
	s.OpenPath(dir)
	waitFor(t, tt, "the ranks", func() bool { ss, ok := s.src.(*sessionSource); return ok && len(ss.parts) == 3 && s.loading == "" })
	ss := s.src.(*sessionSource)
	for i, p := range ss.parts {
		if p.label != []string{"rank0", "rank1", "rank2"}[i] || (i > 0 && (p.align.Basis != "collectives" || p.align.Spread > 1000)) {
			t.Errorf("rank %d: %q aligned on %s, spread %v ns", i, p.label, p.align.Basis, p.align.Spread)
		}
	}
	// rank1's forward attention kernel.
	p := ss.parts[1]
	ref, ok := torchKernel(p.tr, "flash_fwd", 2)
	if !ok {
		t.Fatal("no forward kernel")
	}
	ref.Track += int32(p.first)
	s.Select(ref)
	tt.Frame()
	tt.Frame()
	for _, want := range []string{"Python stack where it was launched · innermost first", "scaled_dot_product_attention", "torch/nn/functional.py:5865", "model.py:39", "train_step", "Top CPU ops"} {
		if !tt.HasText(want) {
			t.Errorf("no %q in the details: %q", want, tt.Texts())
		}
	}
	texts := tt.Texts()
	if i, j := slices.Index(texts, "scaled_dot_product_attention"), slices.Index(texts, "train_step"); i < 0 || j < i {
		t.Errorf("not the innermost frame first: %d, %d", i, j)
	}
	if err := tt.Click("Python frame model.py(39): forward"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	sel := *s.selected
	local := trace.Ref{Track: sel.Track - int32(p.first), Lane: sel.Lane, Index: sel.Index}
	if s.tr.Tracks[sel.Track].Kind != trace.TrackPython || p.tr.Name(p.tr.Event(local)) != "model.py(39): forward" {
		t.Errorf("the frame's click selected %+v (%s)", sel, p.tr.Name(p.tr.Event(local)))
	}
	// A backward kernel: autograd's thread ran it, no Python.
	bref, _ := torchKernel(p.tr, "flash_bwd", 1)
	bref.Track += int32(p.first)
	s.Select(bref)
	tt.Frame()
	if !tt.HasText("No Python stack: launched from pt_autograd_0 (41226) · CUDA API, which ran no Python then (autograd's thread runs the backward pass without one).") {
		t.Errorf("backward kernel: %q", tt.Texts())
	}
	// Top CPU ops: the operators, of the trace in focus.
	if err := tt.Click("Top CPU ops"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "the operators' summary", func() bool { tt.Frame(); return tt.HasText("aten::mm") && tt.HasText("aten::addmm") })
	if strings.Contains(strings.Join(tt.Texts(), "|"), "cudaLaunchKernel") {
		t.Error("CUDA API calls among the operators")
	}
}

// A file of several ranks merged opens, from its path, as a session of its
// ranks, named so, lined up on their collectives; by their clocks, on the
// file's clock they share (not each at its start).
func TestTorchMergedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.json.gz")
	if err := torchgen.WriteMerged(path, torchgen.Options{Ranks: 3, Steps: 3, Layers: 2, NoStack: true}, true); err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	s.OpenLocal(path)
	waitFor(t, tt, "the ranks", func() bool { ss, ok := s.src.(*sessionSource); return ok && len(ss.parts) == 3 && s.loading == "" })
	ss := s.src.(*sessionSource)
	for i, p := range ss.parts {
		if p.label != "rank"+string(rune('0'+i)) || (i > 0 && (p.align.Basis != "collectives" || p.align.Spread > 1000)) {
			t.Errorf("part %d: %q aligned on %s (spread %v)", i, p.label, p.align.Basis, p.align.Spread)
		}
	}
	// By clocks: the file's epoch times, the same clock for all (a file
	// of times from boot would line up on the file's clock, "monotonic").
	s.SetAlign("abs")
	for i, p := range ss.parts {
		if p.align.Basis != ss.parts[0].align.Basis || p.align.Basis == "start" {
			t.Errorf("by clocks, part %d on %q", i, p.align.Basis)
		}
	}
	// The operators of a kernel, in its details.
	ref, _ := torchKernel(ss.parts[1].tr, "flash_fwd", 1)
	ref.Track += int32(ss.parts[1].first)
	s.SetAlign("collectives")
	s.Select(ref)
	tt.Frame()
	tt.Frame()
	if !tt.HasText("Operator stack where it was launched · innermost first") || !tt.HasText("aten::_flash_attention_forward") {
		t.Errorf("no operator stack: %q", tt.Texts())
	}
}
