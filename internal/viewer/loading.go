package viewer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/remote"
	"tracelight/internal/trace"
)

// Loading several traces (the ranks of a job) takes a while: the loading
// screen says how far each has got and how far they all are, as the
// readers report it (trace.Progress), on this machine or on a server
// (remote.Client.Progress, asked every pollEvery).

// loadItem is a trace of what is loading, as the loading screen lists it.
// The UI's goroutine makes the list; loads write the progress.
type loadItem struct {
	name string
	size int64 // its file's, to weigh it in the whole; 0 if unknown
	prog *trace.Progress
	// listing marks an input that a server is still turning into traces
	// (a folder, a glob, a merged file): its progress shows only once
	// the server reads it.
	listing bool
	// received marks a trace a server has loaded, its rows on their way
	// (pollProgress); nil for a trace loading here.
	received *atomic.Bool
}

// pollEvery is how often a server is asked how far its loads are.
const pollEvery = 250 * time.Millisecond

// localItems are the loading screen's lines for files on this machine.
func localItems(files []string) []loadItem {
	items := make([]loadItem, len(files))
	for i, f := range files {
		file, _, _ := trace.SplitMember(f)
		items[i] = loadItem{name: loadName(f), prog: &trace.Progress{}}
		if fi, err := os.Stat(file); err == nil {
			items[i].size = fi.Size()
		}
	}
	return items
}

// loadName names a file on the loading screen: its last element ("rank8"
// for a rank of a merged file).
func loadName(p string) string {
	if _, rank, ok := trace.SplitMember(p); ok {
		return fmt.Sprintf("rank%d", rank)
	}
	return filepath.Base(strings.TrimRight(p, "/\\"))
}

// overall is how far the loads are together, from 0 to 1, each weighed by
// its file's size (equally where sizes are unknown), and how many are done
// or failed.
func overall(items []loadItem) (frac float64, done, failed int) {
	var total, got float64
	var known int64
	for _, it := range items {
		known += it.size
	}
	for _, it := range items {
		w := 1.0
		if known > 0 {
			w = float64(it.size) / float64(known)
		}
		total += w
		got += w * it.prog.Fraction()
		switch it.prog.Phase() {
		case trace.PhaseDone:
			done++
		case trace.PhaseFailed:
			failed++
		}
	}
	if total > 0 {
		frac = got / total
	}
	return frac, done, failed
}

// LoadProgress says what the loading screen shows: how many traces it
// lists, how many of them are done, and how far they all are (0 to 1). For
// tests driving the App UI.
func (s *App) LoadProgress() (n, done int, frac float64) {
	frac, done, _ = overall(s.loadItems)
	return len(s.loadItems), done, frac
}

// maxLoadRows is how many traces the loading screen lists; more say how
// many more.
const maxLoadRows = 16

// loadingView is the welcome screen's part while traces load: a bar of
// how far they all are, and a line a trace.
func (s *App) loadingView(c *ui.Context) {
	items := s.loadItems
	what := "Loading " + filepath.Base(s.loading) + "…"
	if len(items) > 1 {
		what = fmt.Sprintf("Loading %d traces…", len(items))
	}
	frac, done, failed := overall(items)
	ui.Row(c).Gap(10).Children(func() {
		ui.Spinner(c)
		ui.Text(c, what).TextColor(colMuted)
		if len(items) > 0 {
			status := fmt.Sprintf("%d%%", int(frac*100))
			if len(items) > 1 {
				status = fmt.Sprintf("%d of %d done · %s", done, len(items), status)
			}
			if failed > 0 {
				status += fmt.Sprintf(" · %d failed", failed)
			}
			ui.Text(c, status).FontSize(12.5).TextColor(colFaint)
		}
	})
	if len(items) == 0 {
		return
	}
	progressBar(c, 520, 6, frac, colAccent)
	if len(items) == 1 && !items[0].listing {
		ui.Text(c, phaseText(items[0])).FontSize(12).TextColor(colFaint)
		return
	}
	ui.Column(c).Gap(3).Children(func() {
		for i, it := range items {
			if i == maxLoadRows {
				ui.Text(c, fmt.Sprintf("and %d more", len(items)-maxLoadRows)).FontSize(12).TextColor(colFaint)
				break
			}
			ui.Row(c.Key(i)).Gap(10).Children(func() {
				ui.Text(c, it.name).FontSize(12).TextColor(colMuted).SingleLine().Width(220)
				col := colAccent
				if it.prog.Phase() == trace.PhaseFailed {
					col = colWarn
				}
				f := it.prog.Fraction()
				if it.listing && f == 0 {
					f = -1 // nothing to say yet
				}
				progressBar(c, 180, 5, f, col)
				ui.Text(c, phaseText(it)).FontSize(11.5).TextColor(colFaint).SingleLine().Width(110)
			})
		}
	})
}

// phaseText says where a trace's load is: "reading 45%", "queued", "done".
func phaseText(it loadItem) string {
	p := it.prog
	ph := p.Phase()
	switch {
	case it.listing && ph <= trace.PhaseOpening:
		return "listing…"
	case ph == trace.PhaseReading:
		return fmt.Sprintf("reading %d%%", int(p.Fraction()*100))
	case ph == trace.PhaseFailed:
		if err := p.Err(); err != nil {
			return "failed: " + err.Error()
		}
	case ph == trace.PhaseConverting:
		return "converting (nsys)"
	case ph == trace.PhaseBuilding && it.received != nil && it.received.Load():
		return "receiving"
	}
	return ph.String()
}

// progressBar draws a bar w by h, frac of it filled (less than 0: a
// track only).
func progressBar(c *ui.Context, w, h float32, frac float64, col ui.Color) {
	ui.Box(c).Size(w, h).Draw(func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, colBorder, h/2)
		if frac > 0 {
			p.Fill(ui.Rect{X: r.X, Y: r.Y, W: max(r.W*float32(min(frac, 1)), h), H: r.H}, col, h/2)
		}
	})
}

// pollProgress asks the server how far its loads of items (at paths) are
// until stop is closed, writing what it says into their progress. Loads
// it has finished (Finish) it asks no more about.
func pollProgress(client *remote.Client, paths []string, items []loadItem, stop <-chan struct{}) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		var ask []string
		var at []int
		for i, it := range items {
			if ph := it.prog.Phase(); ph != trace.PhaseDone && ph != trace.PhaseFailed {
				ask = append(ask, paths[i])
				at = append(at, i)
			}
		}
		if len(ask) == 0 {
			return
		}
		got, err := client.Progress(ask)
		if err != nil {
			continue // an older server, or the network: the bars wait
		}
		for k, lp := range got {
			ph := trace.ParsePhase(lp.Phase)
			if ph == trace.PhaseDone || ph == trace.PhaseFailed {
				// Loaded there, its rows on their way: the client's own
				// request says when it is done.
				ph = trace.PhaseBuilding
				if r := items[at[k]].received; r != nil {
					r.Store(true)
				}
			}
			if items[at[k]].listing && ph == trace.PhaseQueued {
				continue
			}
			items[at[k]].prog.SetFraction(ph, min(lp.Fraction, 0.99))
		}
	}
}

// redrawEvery is how often the loading screen is built again, to show the
// progress as it goes.
const redrawEvery = 150 * time.Millisecond

// redrawWhile asks the window for a frame every redrawEvery until stop is
// closed: the loading screen reads its loads' progress as it is built.
func (c *control) redrawWhile(stop <-chan struct{}) {
	t := time.NewTicker(redrawEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.wakeUp()
		}
	}
}
