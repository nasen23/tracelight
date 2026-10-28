package main

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/server"
	"tracelight/internal/viewer"
)

// benchRemote is `tracelight bench -remote`: it serves the trace's folder
// in-process, every request delayed by a simulated round trip, and times
// the window streaming it as when connected to a server: to its rows, to
// the first pixels of events, to every tile of the view; then a scripted
// zoom and pan at 60 frames a second while tiles stream, timing each frame
// and how long each new view takes to be drawn at its full detail. It
// prints aggregates only.
func benchRemote(path, rtt string, w, h int, overview bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		fail(err)
	}
	lo, hi, err := parseRTT(rtt)
	if err != nil {
		fail(err)
	}
	srv, err := server.New(server.Config{Root: filepath.Dir(abs)})
	if err != nil {
		fail(err)
	}
	var requests atomic.Int64
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(1, 2))
	ts := httptest.NewServer(http.HandlerFunc(func(wr http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		d := lo
		if hi > lo {
			mu.Lock()
			d += time.Duration(rng.Int64N(int64(hi - lo)))
			mu.Unlock()
		}
		time.Sleep(d)
		srv.ServeHTTP(wr, r)
	}))
	defer ts.Close()
	fmt.Printf("remote: simulated round trip %v–%v per request, window %dx%d\n", lo, hi, w, h)

	for _, run := range []string{"cold server (parses the trace)", "warm server (trace in memory)"} {
		app := viewer.New(nil)
		app.SetAnimations(false)
		if overview {
			app.SetSettings(viewer.Settings{Compact: "on"})
		}
		woke := make(chan struct{}, 1)
		app.SetWake(func() {
			select {
			case woke <- struct{}{}:
			default:
			}
		})
		tt := ui.NewTester(app.View, w, h)
		tt.Frame()
		start := time.Now()
		app.Connect(ts.URL, "", "")
		frameUntil(tt, woke, 30*time.Second, func() bool { _, ok := app.Connected(); return ok })
		connected := time.Since(start)
		start = time.Now()
		app.OpenPathAll(abs)
		var rows, first, full time.Duration
		frameUntil(tt, woke, 60*time.Second, func() bool {
			if app.Trace() == nil || app.Loading() {
				return false
			}
			if rows == 0 {
				rows = time.Since(start)
			}
			blocks, complete, loading := app.StreamStats()
			if blocks > 0 && first == 0 {
				first = time.Since(start)
			}
			if complete && loading == 0 && blocks > 0 {
				full = time.Since(start)
				return true
			}
			return false
		})
		tr := app.Trace()
		fmt.Printf("\n%s: %s events in %d rows\n", run, fmtInt(tr.EventCount()), len(tr.Tracks))
		fmt.Printf("  connect (info)                  %8s\n", ms(connected))
		fmt.Printf("  open to the rows (meta)         %8s\n", ms(rows))
		fmt.Printf("  open to first pixels of events  %8s\n", ms(first))
		fmt.Printf("  open to every tile of the view  %8s\n", ms(full))
		if run[0] == 'c' {
			continue
		}

		// The script, as scripts/overviewbench.mjs's: zoom in by 4× steps,
		// panning half a view and jumping to places not seen yet at each
		// level, then zoom out from deep at another place; each view framed
		// at 60 a second until every tile slot is of its level.
		dur := float64(tr.Duration())
		mid := dur / 2
		type view struct{ a, b float64 }
		var views []view
		k := 0
		for span := dur; span > dur/300; span /= 4 {
			views = append(views, view{mid - span/2, mid + span/2}, view{mid, mid + span}, view{mid - span, mid})
			far := dur * (0.15 + 0.1*float64(k))
			views = append(views, view{far, far + span}, view{far + 3*span, far + 4*span})
			k++
		}
		for span, p := dur/256, dur*0.8; span < dur; span *= 4 {
			views = append(views, view{p - span/2, p + span/2})
		}
		var frames, refill []time.Duration
		blankFrames, standinFrames, timeouts := 0, 0, 0
		requests.Store(0)
		for _, v := range views {
			app.ZoomTo(v.a, v.b)
			stepStart := time.Now()
			for {
				t0 := time.Now()
				tt.Frame()
				frames = append(frames, time.Since(t0))
				exact, coarse, blank := app.StreamSlots()
				if blank > 0 {
					blankFrames++
				}
				if coarse > 0 {
					standinFrames++
				}
				if exact > 0 && coarse == 0 && blank == 0 {
					refill = append(refill, time.Since(stepStart))
					break
				}
				if time.Since(stepStart) > 10*time.Second {
					timeouts++
					break
				}
				time.Sleep(max(0, 16*time.Millisecond-time.Since(t0)))
			}
		}
		fmt.Printf("\nviews: %d (4× zoom steps from %s down and back, pans and jumps), %d frames at 60 a second\n", len(views), time.Duration(dur), len(frames))
		printDist("  to full refill   ", refill)
		fmt.Printf("  views not full after 10 s: %d\n", timeouts)
		fmt.Printf("  frames with blank slots: %d (%.1f%%), with stand-in tiles: %d (%.1f%%)\n", blankFrames, 100*float64(blankFrames)/float64(len(frames)), standinFrames, 100*float64(standinFrames)/float64(len(frames)))
		printDist("  build+paint      ", frames)
		fmt.Printf("  requests during the script: %d\n", requests.Load())
	}
}

// frameUntil frames the window as things arrive until cond holds.
func frameUntil(tt *ui.Tester, woke chan struct{}, limit time.Duration, cond func() bool) {
	deadline := time.Now().Add(limit)
	for {
		tt.Frame()
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			fail(fmt.Errorf("timed out"))
		}
		select {
		case <-woke:
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func parseRTT(s string) (lo, hi time.Duration, err error) {
	a, b, ok := strings.Cut(s, "-")
	if lo, err = time.ParseDuration(a); err != nil {
		return
	}
	hi = lo
	if ok {
		hi, err = time.ParseDuration(b)
	}
	if hi < lo || lo < 0 {
		err = fmt.Errorf("bad -rtt %q", s)
	}
	return
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
