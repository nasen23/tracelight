package viewer

import (
	"image/png"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/server"
	"tracelight/internal/torchgen"
	"tracelight/internal/trace"
)

// TestDesktopScreenshots renders the connect dialog and the path box, with
// the synthetic job of samples/ranks (go run ./cmd/gensample -ranks 8),
// into $TRACELIGHT_SHOTS. It is skipped without it.
func TestDesktopScreenshots(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	ranks, _ := filepath.Abs("../../samples/ranks")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	if _, err := os.Stat(ranks); err != nil {
		t.Skip("no samples/ranks: go run ./cmd/gensample -ranks 8")
	}
	save := func(tt *ui.Tester, name string) {
		f, err := os.Create(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, tt.Image())
		f.Close()
	}
	const W, H = 1600, 1000

	// The connect dialog, connected to a server streaming the job.
	// A short path for the pictures.
	base := "/tmp/nsa-shots/data"
	os.RemoveAll(base)
	defer os.RemoveAll("/tmp/nsa-shots")
	runs := filepath.Join(base, "runs")
	os.MkdirAll(runs, 0o755)
	os.MkdirAll(filepath.Join(runs, "job7"), 0o755)
	files, _ := filepath.Glob(filepath.Join(ranks, "*.sqlite"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		os.WriteFile(filepath.Join(runs, "job7", filepath.Base(f)), data, 0o644)
	}
	srv, err := server.New(server.Config{Root: runs, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:8737")
	if err != nil {
		ln, _ = net.Listen("tcp", "127.0.0.1:0")
	}
	go http.Serve(ln, srv)
	defer ln.Close()
	url := "http://" + ln.Addr().String()

	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	s.SetSettings(Settings{Servers: []string{"http://gpu-server.example.com:8737/", "http://train-cluster.example.org:8737/"},
		RecentServerPaths: map[string][]string{"http://gpu-server.example.com:8737/": {"/data/runs/job6", "/data/runs/job5/rank0.nsys-rep"}}})
	tt := ui.NewTester(s.View, W, H)
	s.Connect(url, "", filepath.Join(runs, "job7"))
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	s.ZoomTo(200_000_000, 245_000_000)
	waitFor(t, tt, "the tiles", func() bool { _, done, n := s.StreamStats(); return done && n == 0 })
	for _, tk := range s.tr.Tracks {
		if tk.Kind != trace.TrackStream {
			s.collapsed[tk.Group] = true
		}
	}
	s.tl.dirty = true
	s.openConnect()
	s.ctl.urlText = url
	tt.Frame()
	time.Sleep(50 * time.Millisecond)
	waitFor(t, tt, "the tiles again", func() bool { _, done, n := s.StreamStats(); return done && n == 0 })
	save(tt, "desktop-connect.png")

	// The path box, local, its suggestions open on the job's folder.
	s2 := New(nil)
	s2.SetAnimations(false)
	s2.SetThemeMode("dark")
	s2.SetSettings(Settings{RecentPaths: []string{filepath.Join(runs, "job7"), "/data/runs/job6/", "/data/runs/job5/rank0.nsys-rep"}})
	tt2 := ui.NewTester(s2.View, W, H)
	s2.OpenPath(filepath.Join(runs, "job7"))
	waitFor(t, tt2, "the local session", func() bool { return s2.tr != nil && s2.loading == "" })
	s2.ZoomTo(200_000_000, 245_000_000)
	for _, tk := range s2.tr.Tracks {
		if tk.Kind != trace.TrackStream {
			s2.collapsed[tk.Group] = true
		}
	}
	s2.tl.dirty = true
	s2.ctl.pathText = ""
	s2.ctl.focusPath = true
	tt2.Frame()
	tt2.Frame()
	tt2.Type(filepath.Join(runs, "job7") + "/")
	tt2.Frame()
	tt2.Frame()
	save(tt2, "desktop-path-input.png")
}

// shotPath is samples/name under a short, neutral path for the pictures,
// /tmp/runs/name (a link to it), which shows in the path box instead of
// where the repository is; the test is skipped without the sample.
func shotPath(t *testing.T, name string) string {
	t.Helper()
	abs, _ := filepath.Abs("../../samples/" + name)
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("no samples/%s", name)
	}
	link := filepath.Join("/tmp/runs", name)
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Remove(link)
	if err := os.Symlink(abs, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// TestOverviewScreenshots renders the rank overview of samples/ranks (8
// ranks: go run ./cmd/gensample -ranks 8) and samples/ranks16 (go run
// ./cmd/gensample -ranks 16 -o samples/ranks16), as they open, into
// $TRACELIGHT_SHOTS.
func TestOverviewScreenshots(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	for _, c := range []struct {
		dir, file string
		t0, t1    float64
	}{
		// Two training steps, wide enough for the kernels' names.
		{"ranks", "desktop-multirank-8.png", 400_000_000, 440_000_000},
		{"ranks16", "desktop-compact-multirank.png", 400_000_000, 440_000_000},
		// Closer: the all-reduces beside the compute kernels, nested NVTX.
		{"ranks", "desktop-overview-parallel.png", 412_000_000, 420_000_000},
		// The data loader's ranges in a band of their own, below the
		// training loop's.
		{"ranks", "desktop-overview-nvtx-bands.png", 404_000_000, 414_000_000},
	} {
		dir := shotPath(t, c.dir)
		s := New(nil)
		s.SetAnimations(false)
		s.SetThemeMode("dark")
		tt := ui.NewTester(s.View, 1600, 1000)
		s.OpenPath(dir)
		waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
		if !s.compactOn() {
			t.Fatalf("%s did not open in the rank overview", c.dir)
		}
		s.ZoomTo(c.t0, c.t1)
		tt.Frame()
		tt.Frame()
		f, err := os.Create(filepath.Join(out, c.file))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, tt.Image())
		f.Close()
	}
}

// TestTabsScreenshot renders three tabs, the synthetic trace
// (samples/sample.sqlite: go run ./cmd/gensample), its 8 ranks
// (samples/ranks) and 16 ranks (samples/ranks16), the 8 ranks shown, into
// $TRACELIGHT_SHOTS.
func TestTabsScreenshot(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	var paths []string
	for _, p := range []string{"sample.sqlite", "ranks", "ranks16"} {
		paths = append(paths, shotPath(t, p))
	}
	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	tt := ui.NewTester(s.View, 1600, 1000)
	for i, p := range paths {
		if i > 0 {
			s.NewTab()
		}
		s.OpenPath(p)
		waitFor(t, tt, p, func() bool { tt.Frame(); return s.tr != nil && s.loading == "" })
		s.ZoomTo(400_000_000, 440_000_000)
		tt.Frame()
	}
	s.ShowTab(1)
	tt.Frame()
	tt.Frame()
	f, err := os.Create(filepath.Join(out, "desktop-tabs.png"))
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, tt.Image())
	f.Close()
}

// TestTorchScreenshots renders a synthetic PyTorch profile (torchgen: a
// rank of a data-parallel job, with Python stacks), a forward pass's
// attention kernel selected, its Python stack beside its details, into
// $TRACELIGHT_SHOTS.
func TestTorchScreenshots(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	// A short path for the picture.
	dir := filepath.Join(os.TempDir(), "runs", "llama-ddp")
	defer os.RemoveAll(filepath.Dir(dir))
	paths, err := torchgen.WriteDir(dir, torchgen.Options{Ranks: 4}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	s.SetCompact("off")
	s.timelinePc = 58
	tt := ui.NewTester(s.View, 1600, 1000)
	s.OpenLocal(paths[1])
	waitFor(t, tt, "the profile", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" })
	ref, ok := torchKernel(s.tr, "flash_fwd", 9)
	if !ok {
		t.Fatal("no forward attention kernel")
	}
	e := s.tr.Event(ref)
	s.ZoomTo(float64(e.Start-s.tr.Start)-5_000_000, float64(e.End-s.tr.Start)+2_500_000)
	s.Select(ref)
	tt.Frame()
	tt.Frame()
	f, err := os.Create(filepath.Join(out, "desktop-torch.png"))
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, tt.Image())
	f.Close()
}

// torchKernel is the nth kernel whose name has part, on the first stream.
func torchKernel(tr *trace.Trace, part string, n int) (trace.Ref, bool) {
	for ti, tk := range tr.Tracks {
		if tk.Kind != trace.TrackStream {
			continue
		}
		for i := range tk.Lanes[0].Events {
			if strings.Contains(tr.Name(&tk.Lanes[0].Events[i]), part) {
				if n--; n == 0 {
					return trace.Ref{Track: int32(ti), Index: int32(i)}, true
				}
			}
		}
		break
	}
	return trace.Ref{}, false
}

// TestTorchMergedScreenshot renders a synthetic file of four ranks merged
// (torchgen.WriteMerged, profiled without with_stack), opened as its
// ranks, a backward kernel of rank 2 selected with its operator stack,
// into $TRACELIGHT_SHOTS.
func TestTorchMergedScreenshot(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	dir := filepath.Join(os.TempDir(), "runs")
	os.MkdirAll(dir, 0o755)
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ddp-job.json.gz")
	if err := torchgen.WriteMerged(path, torchgen.Options{Ranks: 4, NoStack: true}, true); err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	s.SetCompact("on")
	s.timelinePc = 60
	tt := ui.NewTester(s.View, 1600, 1000)
	s.OpenLocal(path)
	waitFor(t, tt, "the ranks", func() bool {
		tt.Frame()
		ss, ok := s.src.(*sessionSource)
		return ok && len(ss.parts) == 4 && s.loading == ""
	})
	ss := s.src.(*sessionSource)
	p := ss.parts[2]
	ref, ok := torchKernel(p.tr, "flash_bwd", 3)
	if !ok {
		t.Fatal("no backward kernel")
	}
	e := p.tr.Event(ref)
	ref.Track += int32(p.first)
	at := float64(e.Start - p.tr.Start + p.offset)
	s.ZoomTo(at-14_000_000, at+10_000_000)
	s.Select(ref)
	tt.Frame()
	tt.Frame()
	f, err := os.Create(filepath.Join(out, "desktop-torch-merged.png"))
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, tt.Image())
	f.Close()
}

// TestDensityScreenshot renders the synthetic job of samples/ranks (8
// ranks: go run ./cmd/gensample -ranks 8) in full density, as it opens at
// 1600×1000, its streams and CUDA API rows dense, into
// $TRACELIGHT_SHOTS/desktop-density.png.
func TestDensityScreenshot(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	ranks, _ := filepath.Abs("../../samples/ranks")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	if _, err := os.Stat(ranks); err != nil {
		t.Skip("no samples/ranks: go run ./cmd/gensample -ranks 8")
	}
	// A short path for the picture.
	dir := filepath.Join(os.TempDir(), "runs", "job7")
	os.MkdirAll(dir, 0o755)
	defer os.RemoveAll(filepath.Dir(dir))
	files, _ := filepath.Glob(filepath.Join(ranks, "*.sqlite"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		os.WriteFile(filepath.Join(dir, filepath.Base(f)), data, 0o644)
	}
	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	s.SetCompact("off")
	tt := ui.NewTester(s.View, 1600, 1000)
	s.OpenLocal(dir)
	waitFor(t, tt, "the ranks", func() bool {
		tt.Frame()
		ss, ok := s.src.(*sessionSource)
		return ok && len(ss.parts) == 8 && s.loading == ""
	})
	s.ZoomTo(200_000_000, 245_000_000)
	tt.Frame()
	tt.Frame()
	f, err := os.Create(filepath.Join(out, "desktop-density.png"))
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, tt.Image())
	f.Close()
}

// TestTracesScreenshot renders the synthetic job of samples/ranks (8
// ranks: go run ./cmd/gensample -ranks 8) with 3 of its traces shown, the
// sidebar's TRACES list saying which, and the server's browser with two
// traces picked, into $TRACELIGHT_SHOTS/desktop-traces.png and
// desktop-pick.png.
func TestTracesScreenshot(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	ranks, _ := filepath.Abs("../../samples/ranks")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	if _, err := os.Stat(ranks); err != nil {
		t.Skip("no samples/ranks: go run ./cmd/gensample -ranks 8")
	}
	save := func(tt *ui.Tester, name string) {
		f, err := os.Create(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, tt.Image())
		f.Close()
	}
	// A short path for the pictures.
	runs := filepath.Join(os.TempDir(), "runs")
	dir := filepath.Join(runs, "job7")
	os.MkdirAll(dir, 0o755)
	defer os.RemoveAll(runs)
	files, _ := filepath.Glob(filepath.Join(ranks, "*.sqlite"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		os.WriteFile(filepath.Join(dir, filepath.Base(f)), data, 0o644)
	}
	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	s.SetCompact("off")
	tt := ui.NewTester(s.View, 1600, 1000)
	s.OpenLocal(dir)
	waitFor(t, tt, "the ranks", func() bool {
		tt.Frame()
		ss, ok := s.src.(*sessionSource)
		return ok && len(ss.parts) == 8 && s.loading == ""
	})
	s.applyTraceFilter([]string{"rank0", "rank3", "rank5"}, nil)
	s.ZoomTo(200_000_000, 245_000_000)
	tt.Frame()
	tt.Frame()
	save(tt, "desktop-traces.png")

	// The server's browser, two traces picked.
	srv, err := server.New(server.Config{Root: runs, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:8737")
	if err != nil {
		ln, _ = net.Listen("tcp", "127.0.0.1:0")
	}
	go http.Serve(ln, srv)
	defer ln.Close()
	r := New(nil)
	r.SetAnimations(false)
	r.SetThemeMode("dark")
	tr := ui.NewTester(r.View, 1600, 1000)
	r.Connect("http://"+ln.Addr().String(), "", "")
	waitFor(t, tr, "the server", func() bool {
		return r.picker != nil && r.picker.shown && !r.picker.listing && len(r.picker.entries) > 0
	})
	r.listRemote("job7")
	waitFor(t, tr, "the folder", func() bool { return !r.picker.listing && len(r.picker.entries) == 8 })
	for _, n := range []string{"Pick rank0.sqlite", "Pick rank5.sqlite"} {
		if err := tr.Click(n); err != nil {
			t.Fatal(err)
		}
		tr.Frame()
	}
	tr.Frame()
	save(tr, "desktop-pick.png")
}

// TestKernelsScreenshot renders samples/sample.sqlite (go run
// ./cmd/gensample) zoomed into a few milliseconds of kernels, with a
// kernel and its launch selected (the flow line between them), into
// $TRACELIGHT_SHOTS/desktop-kernels.png: kernels in three shades with a
// seam between them, named down to 18-pixel boxes.
func TestKernelsScreenshot(t *testing.T) {
	out := os.Getenv("TRACELIGHT_SHOTS")
	path, _ := filepath.Abs("../../samples/sample.sqlite")
	if out == "" {
		t.Skip("TRACELIGHT_SHOTS not set")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no samples/sample.sqlite: go run ./cmd/gensample")
	}
	dir := filepath.Join(os.TempDir(), "runs")
	os.MkdirAll(dir, 0o755)
	defer os.RemoveAll(dir)
	data, _ := os.ReadFile(path)
	short := filepath.Join(dir, "train.sqlite")
	os.WriteFile(short, data, 0o644)
	s := New(nil)
	s.SetAnimations(false)
	s.SetThemeMode("dark")
	tt := ui.NewTester(s.View, 1600, 1000)
	s.OpenLocal(short)
	waitFor(t, tt, "the trace", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" })
	a, b := kernelsWindow()
	s.ZoomTo(a, b)
	// A kernel in the middle selected: the flow line to its launch.
	for i, e := range s.tr.Tracks[0].Lanes[0].Events {
		if float64(e.Start-s.tr.Start) >= (a+b)/2 {
			s.Select(trace.Ref{Track: 0, Lane: 0, Index: int32(i)})
			break
		}
	}
	tt.Frame()
	tt.Frame()
	f, err := os.Create(filepath.Join(out, "desktop-kernels.png"))
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, tt.Image())
	f.Close()
}

// kernelsWindow is the window of samples/sample.sqlite the kernel
// screenshots show (desktop and web alike).
func kernelsWindow() (float64, float64) { return 372_400_000, 374_800_000 }
