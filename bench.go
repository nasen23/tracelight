package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/trace"
	"tracelight/internal/viewer"
)

// benchMain is `tracelight bench [flags] file`: it loads the trace and
// replays a scripted sequence of zooms and pans headlessly, timing each
// phase and frame.
func benchMain(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	size := fs.String("size", "1600x1000", "window size, WxH")
	raster := fs.Bool("raster", true, "also time CPU rasterization of each frame (what a window without a GPU pays)")
	loadProfile := fs.String("loadprofile", "", "write a CPU profile of loading to this file")
	firstProfile := fs.String("firstprofile", "", "write a CPU profile of the first frame to this file")
	frameProfile := fs.String("frameprofile", "", "write a CPU profile of the scripted frames to this file")
	allocProfile := fs.String("allocprofile", "", "write an allocation profile of the scripted frames to this file")
	remoteMode := fs.Bool("remote", false, "stream the trace from a server, as the window does when connected: serve its folder in-process and time opening and the frames")
	overview := fs.Bool("overview", false, "with -remote and a folder, show its traces in the rank overview whatever their number")
	rtt := fs.String("rtt", "30ms-80ms", "with -remote, the round-trip time each request is delayed by: a duration, or a range lo-hi drawn from uniformly")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: tracelight bench [flags] trace.sqlite|folder-of-traces\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	var w, h int
	if _, err := fmt.Sscanf(*size, "%dx%d", &w, &h); err != nil {
		fmt.Fprintln(os.Stderr, "bad -size")
		os.Exit(2)
	}
	if *remoteMode {
		benchRemote(fs.Arg(0), *rtt, w, h, *overview)
		return
	}
	if fi, err := os.Stat(fs.Arg(0)); err == nil && fi.IsDir() {
		benchSession(fs.Arg(0), *raster, w, h)
		return
	}
	heap := startHeapSampler()

	// The window draws its loading screen while the trace loads, which
	// warms the UI's text and fonts; do the same here, and time it.
	var warm time.Duration
	warmed := make(chan struct{})
	go func() {
		defer close(warmed)
		start := time.Now()
		ui.Render(viewer.New(nil).View, w, h, 1)
		warm = time.Since(start)
	}()

	stopProfile := profile(*loadProfile)
	start := time.Now()
	tr, err := trace.Open(fs.Arg(0))
	stopProfile()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	loadTime := time.Since(start)

	<-warmed
	start = time.Now()
	app := viewer.New(tr)
	app.SetAnimations(false)
	newApp := time.Since(start)
	stopProfile = profile(*firstProfile)
	start = time.Now()
	tt := ui.NewTester(app.View, w, h)
	tt.Frame()
	firstBuild := time.Since(start)
	stopProfile()
	start = time.Now()
	tt.Image()
	firstRaster := time.Since(start)

	fmt.Printf("trace: %s\n", tr.Path)
	fmt.Printf("events: %s in %d rows over %s\n", fmtInt(tr.EventCount()), len(tr.Tracks), time.Duration(tr.Duration()))
	for _, k := range []trace.Kind{trace.KindKernel, trace.KindMemcpy, trace.KindMemset, trace.KindRuntime, trace.KindSync, trace.KindNVTX, trace.KindNVTXMark} {
		fmt.Printf("  %-11s %10s\n", k, fmtInt(tr.Counts[k]))
	}
	fmt.Printf("\nload: %v total\n", ms(loadTime))
	fmt.Printf("  sqlite read + decode  %8s  (%d connections in parallel; tables overlap)\n", ms(tr.Load.Read-tr.Load.Merge), tr.Load.Workers)
	for _, t := range tr.Load.Tables {
		chunks := ""
		if t.Chunks > 1 {
			chunks = fmt.Sprintf(", %d chunks", t.Chunks)
		}
		fmt.Printf("    %-32s %8s  %9s rows%s\n", t.Table, ms(t.Time), fmtInt(t.Rows), chunks)
	}
	fmt.Printf("  merge shards          %8s\n", ms(tr.Load.Merge))
	fmt.Printf("  model build (pack)    %8s\n", ms(tr.Load.Build))
	fmt.Printf("  viewer state (stats)  %8s\n", ms(newApp))
	fmt.Printf("  first frame           %8s build+paint, %s CPU raster (UI warmed during load; cold, the first frame took %s)\n", ms(firstBuild), ms(firstRaster), ms(warm))
	fmt.Printf("  open to first pixels  %8s\n", ms(loadTime+newApp+firstBuild+firstRaster))

	// The script: from the whole trace down to a couple of microseconds
	// around the busiest stretch of kernels, panning at each level, and back.
	steps := script(tr, app)
	var build, full []time.Duration
	var mallocs uint64
	if *allocProfile != "" {
		runtime.MemProfileRate = 4096
	}
	stopProfile = profile(*frameProfile)
	var allocBytes uint64
	var ms0, ms1 runtime.MemStats
	for _, step := range steps {
		step.apply()
		runtime.ReadMemStats(&ms0)
		start := time.Now()
		tt.Frame()
		b := time.Since(start)
		runtime.ReadMemStats(&ms1)
		mallocs += ms1.Mallocs - ms0.Mallocs
		allocBytes += ms1.TotalAlloc - ms0.TotalAlloc
		build = append(build, b)
		if *raster {
			tt.Image() // copies the frame: not counted as the app's allocations
			full = append(full, time.Since(start))
		}
	}
	stopProfile()
	if *allocProfile != "" {
		if f, err := os.Create(*allocProfile); err == nil {
			pprof.Lookup("allocs").WriteTo(f, 0)
			f.Close()
		}
	}

	fmt.Printf("\nframes: %d scripted (zoom from %s down to %s and back, pans and scrolls at each level, pointer over the timeline)\n",
		len(steps), time.Duration(tr.Duration()), time.Duration(minSpan))
	printDist("  build+paint      ", build)
	if *raster {
		printDist("  + CPU raster     ", full)
	}
	fmt.Printf("  allocations      %.0f per frame, %.1f KB per frame (build+paint)\n",
		float64(mallocs)/float64(len(steps)), float64(allocBytes)/1024/float64(len(steps)))

	runtime.GC()
	var live runtime.MemStats
	runtime.ReadMemStats(&live)
	fmt.Printf("\nmemory:\n")
	fmt.Printf("  peak RSS         %8.1f MB\n", float64(peakRSS())/1e6)
	fmt.Printf("  peak Go heap     %8.1f MB (sampled every 2 ms)\n", float64(heap.stop())/1e6)
	fmt.Printf("  live Go heap     %8.1f MB after GC, trace and view loaded\n", float64(live.HeapAlloc)/1e6)
	runtime.KeepAlive(tr)
}

// profile starts a CPU profile into path, unless it is empty, and returns
// the function that stops it.
func profile(path string) func() {
	if path == "" {
		return func() {}
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d)/1e6) }

func printDist(label string, d []time.Duration) {
	s := slices.Clone(d)
	slices.Sort(s)
	pct := func(p float64) time.Duration { return s[min(len(s)-1, int(math.Ceil(p*float64(len(s))))-1)] }
	over := 0
	for _, v := range s {
		if v > 16*time.Millisecond {
			over++
		}
	}
	fmt.Printf("%s p50 %s  p95 %s  p99 %s  max %s  (%d over 16 ms)\n", label, ms(pct(0.5)), ms(pct(0.95)), ms(pct(0.99)), ms(s[len(s)-1]), over)
}

const minSpan = 2_000 // ns: the deepest zoom of the script

type step struct{ apply func() }

// script makes the frames of the benchmark.
func script(tr *trace.Trace, app *viewer.App) []step {
	dur := float64(tr.Duration())
	center := busiest(tr)
	var steps []step
	add := func(a, b float64) { steps = append(steps, step{func() { app.ZoomTo(a, b) }}) }
	var spans []float64
	for span := dur; span > minSpan; span /= 1.6 {
		spans = append(spans, span)
	}
	spans = append(spans, minSpan)
	level := func(span float64) {
		c := min(max(center, span/2), dur-span/2)
		add(c-span/2, c+span/2)
		for _, f := range []float64{0.3, 0.6, 0.3, 0, -0.3, -0.6, -0.3, 0} {
			add(c-span/2+f*span, c+span/2+f*span)
		}
		app.Hover(700, 160)
		for _, dy := range []float32{200, 400, 0} {
			steps = append(steps, step{func() { app.ScrollTo(dy) }})
		}
	}
	for _, s := range spans {
		level(s)
	}
	for i := len(spans) - 2; i >= 0; i-- {
		level(spans[i])
	}
	return steps
}

// busiest returns the relative time at the middle of the densest stretch of
// GPU work: where zooming in has the most to show.
func busiest(tr *trace.Trace) float64 {
	const buckets = 256
	var hist [buckets]int
	dur := max(tr.Duration(), 1)
	for _, tk := range tr.Tracks {
		if tk.Kind != trace.TrackStream {
			continue
		}
		for _, l := range tk.Lanes {
			for i := range l.Events {
				hist[min(buckets-1, int((l.Events[i].Start-tr.Start)*buckets/dur))]++
			}
		}
	}
	best := 0
	for i := range hist {
		if hist[i] > hist[best] {
			best = i
		}
	}
	return (float64(best) + 0.5) * float64(dur) / buckets
}

type heapSampler struct {
	peak atomic.Uint64
	done chan struct{}
	wg   sync.WaitGroup
}

// startHeapSampler samples the Go heap's size until stop, keeping the peak.
func startHeapSampler() *heapSampler {
	h := &heapSampler{done: make(chan struct{})}
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > h.peak.Load() {
				h.peak.Store(v)
			}
			select {
			case <-h.done:
				return
			case <-t.C:
			}
		}
	}()
	return h
}

func (h *heapSampler) stop() uint64 {
	close(h.done)
	h.wg.Wait()
	return h.peak.Load()
}
