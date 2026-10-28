package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/viewer"
)

// benchSession is `tracelight bench folder`: it opens the folder's traces
// side by side, as the window's path box does, and replays zooms by 4×
// steps from the whole session down to 2 µs around its middle and back,
// with half-view pans and a scroll at each level, in the rank overview and
// in the full view, timing each frame. It prints aggregates only.
func benchSession(dir string, raster bool, w, h int) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		fail(err)
	}
	for _, mode := range []string{"on", "off"} {
		app := viewer.New(nil)
		app.SetAnimations(false)
		app.SetSettings(viewer.Settings{Compact: mode})
		woke := make(chan struct{}, 1)
		app.SetWake(func() {
			select {
			case woke <- struct{}{}:
			default:
			}
		})
		tt := ui.NewTester(app.View, w, h)
		start := time.Now()
		app.OpenPathAll(abs)
		frameUntil(tt, woke, 2*time.Minute, func() bool { return app.Trace() != nil && !app.Loading() })
		loaded := time.Since(start)
		tr := app.Trace()
		dur := float64(tr.Duration())
		mid := dur / 2
		type view struct{ a, b float64 }
		var views []view
		for span := dur; span > 2000; span /= 4 {
			views = append(views, view{mid - span/2, mid + span/2}, view{mid, mid + span}, view{mid - span, mid})
		}
		for span := 2000.0 * 16; span < dur; span *= 16 {
			views = append(views, view{mid - span/2, mid + span/2})
		}
		// The first frame (text shaping, the first layout), apart.
		t0 := time.Now()
		tt.Frame()
		first := time.Since(t0)
		views = append(views, views...) // twice, for more frames
		var build, full []time.Duration
		blocks := 0
		for i, v := range views {
			app.ZoomTo(v.a, v.b)
			app.ScrollTo(float32(i%3) * 120)
			t0 := time.Now()
			tt.Frame()
			build = append(build, time.Since(t0))
			if raster {
				tt.Image()
				full = append(full, time.Since(t0))
			}
			b, _, _ := app.StreamStats()
			blocks += b
		}
		name := "full    "
		if mode == "on" {
			name = "overview"
		}
		fmt.Printf("%s: %s events, %d rows, opened in %s, first frame %s; %d frames (zoom 4× steps from %s to 2 µs and back, pans, scrolls, twice), %d blocks a frame\n",
			name, fmtInt(tr.EventCount()), len(tr.Tracks), ms(loaded), ms(first), len(views), time.Duration(dur), blocks/len(views))
		printDist("  build+paint      ", build)
		if raster {
			printDist("  + CPU raster     ", full)
		}
	}
}
