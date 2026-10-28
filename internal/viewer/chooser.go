package viewer

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// Too many traces at once: a folder of 64 ranks, a glob, a pattern or a
// merged file that would load more than theme.ChooseOver traces, or more
// than theme.ChooseBytes of files, first asks which to open: the tab lists
// them, a box each, theme.ChoosePick ticked, with quick picks (the first,
// some spread across the job, all, none), and opens those ticked, or all
// if asked. Files each named (picked in a browser, or ticked here) never
// ask: they were chosen. As the web client's (chooseHTML).

// choice is a trace the chooser lists.
type choice struct {
	path, name string
	size       int64 // its file's (a merged file's for each of its ranks)
}

// chooser is a tab asking which of its traces to open.
type chooser struct {
	input  string // what was asked to open, as typed
	remote bool   // on the server connected to
	files  []choice
	ticked []bool
	last   int // the box last clicked: Shift-click ticks the run up to another
}

// shouldChoose reports whether opening count traces, of bytes in all,
// named by inputs paths first asks which. The same function as
// shared.js's.
func shouldChoose(count int, bytes int64, inputs int) bool {
	return count > inputs && count > 1 && (count > theme.ChooseOver || bytes > theme.ChooseBytes)
}

// spreadPick picks k of n evenly, the first and the last among them. The
// same function as shared.js's.
func spreadPick(n, k int) []int {
	if k >= n {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	if k <= 1 {
		if k == 1 {
			return []int{0}
		}
		return nil
	}
	var out []int
	for i := range k {
		j := int(float64(i*(n-1))/float64(k-1) + 0.5)
		if len(out) == 0 || out[len(out)-1] != j {
			out = append(out, j)
		}
	}
	return out
}

// totalBytes is the size of the files of choices, each file once (the
// ranks of a merged file are one file).
func totalBytes(files []choice) int64 {
	seen := map[string]bool{}
	var n int64
	for _, f := range files {
		file, _, _ := trace.SplitMember(f.path)
		if !seen[file] {
			seen[file] = true
			n += f.size
		}
	}
	return n
}

// choose shows the chooser in the tab instead of loading.
func (s *App) choose(ch *chooser) {
	ch.ticked, ch.last = make([]bool, len(ch.files)), -1
	for i := range min(theme.ChoosePick, len(ch.files)) {
		ch.ticked[i] = true
	}
	s.SetLoading("", nil)
	s.loadItems = nil
	s.choosing = ch
	s.setTitle(ch.input)
}

// openChosen opens the traces ticked in the chooser without asking again.
func (s *App) openChosen(paths []string) {
	ch := s.choosing
	s.choosing = nil
	s.chooseOff = true
	input := strings.Join(paths, " ")
	s.ctl.pathText = input
	if ch.remote {
		s.openRemote(paths, input)
	} else {
		s.openLocal(paths, input)
	}
}

// Choosing says whether the tab shown asks which traces to open: how many
// it lists and how many are ticked. For tests driving the App UI.
func (s *App) Choosing() (n, ticked int) {
	if s.choosing == nil {
		return 0, 0
	}
	for _, on := range s.choosing.ticked {
		if on {
			ticked++
		}
	}
	return len(s.choosing.files), ticked
}

// OpenTicked opens the traces ticked in the chooser shown, as its button
// does. For tests driving the App UI.
func (s *App) OpenTicked() {
	if ch := s.choosing; ch != nil {
		var paths []string
		for i, f := range ch.files {
			if ch.ticked[i] {
				paths = append(paths, f.path)
			}
		}
		s.openChosen(paths)
	}
}

// OpenPathAll opens input as OpenPath does, all its traces however many
// (for benchmarks and scripts, which ask no one).
func (s *App) OpenPathAll(input string) {
	s.chooseOff = true
	s.OpenPath(input)
}

// openAll opens every trace of the chooser's input without asking again.
func (s *App) openAll() {
	input := s.choosing.input
	s.choosing = nil
	s.chooseOff = true
	s.OpenPath(input)
}

// chooserView is the tab asking which traces to open.
func (s *App) chooserView(c *ui.Context) {
	ch := s.choosing
	n := len(ch.files)
	var picked []string
	var pickedFiles []choice
	for i, f := range ch.files {
		if ch.ticked[i] {
			picked = append(picked, f.path)
			pickedFiles = append(pickedFiles, f)
		}
	}
	name := filepath.Base(strings.TrimRight(strings.Fields(ch.input + " ?")[0], "/\\"))
	if len(strings.Fields(ch.input)) > 1 {
		name = fmt.Sprintf("%d paths", len(strings.Fields(ch.input)))
	}
	set := func(on func(i int) bool) {
		for i := range ch.ticked {
			ch.ticked[i] = on(i)
		}
	}
	ui.Column(c.Key("chooser")).Width(640).Gap(10).Padding(16).Radius(10).Background(colPanel).Border(1, colBorder).Children(func() {
		ui.Row(c).Gap(10).Children(func() {
			ui.Text(c, fmt.Sprintf("%d traces in %s", n, name)).FontSize(15).Bold().SingleLine().Shrink(1)
			ui.Text(c, fmtBytes(totalBytes(ch.files))).FontSize(12.5).TextColor(colFaint)
		})
		ui.Text(c, fmt.Sprintf("That's a lot to open at once (more than %d traces, or %s): tick the ones to open, or open them all.",
			theme.ChooseOver, fmtBytes(theme.ChooseBytes))).FontSize(12.5).TextColor(colMuted)
		ui.Row(c).Gap(6).Children(func() {
			k := theme.ChoosePick
			if ui.Button(c, fmt.Sprintf("First %d", k)).Tooltip("The first traces, in order").Clicked() {
				set(func(i int) bool { return i < k })
			}
			if ui.Button(c, fmt.Sprintf("Spread %d", k)).Tooltip("Traces spread across the job: the first, the last and between").Clicked() {
				set(func(int) bool { return false })
				for _, i := range spreadPick(n, k) {
					ch.ticked[i] = true
				}
			}
			if ui.Button(c, "All").Clicked() {
				set(func(int) bool { return true })
			}
			if ui.Button(c, "None").Clicked() {
				set(func(int) bool { return false })
			}
			ui.Spacer(c)
			ui.Text(c, fmt.Sprintf("%d ticked · %s", len(picked), fmtBytes(totalBytes(pickedFiles)))).FontSize(12.5).TextColor(colFaint)
		})
		ui.Scroll(c).Height(min(float32(n)*26+12, 340)).Padding(6).Gap(1).Border(1, colBorder).Radius(6).Children(func() {
			for i, f := range ch.files {
				ui.Row(c.Key(i)).Gap(8).Height(24).PaddingX(4).Children(func() {
					if ui.Checkbox(c.Key("tick"), &ch.ticked[i], "").Label("Open " + f.name).Changed() {
						if c.Modifiers()&ui.Shift != 0 && ch.last >= 0 {
							for j := min(i, ch.last); j <= max(i, ch.last); j++ {
								ch.ticked[j] = ch.ticked[i]
							}
						}
						ch.last = i
					}
					ui.Text(c, f.name).FontSize(12.5).SingleLine().Grow(1).Shrink(1)
					ui.Text(c, fmtBytes(f.size)).FontSize(11.5).TextColor(colFaint).Width(80).TextAlign(ui.End)
				})
			}
		})
		ui.Row(c).Gap(8).Children(func() {
			label := fmt.Sprintf("Open %d ticked", len(picked))
			if ui.PrimaryButton(c, label).Disabled(len(picked) == 0).Tooltip("Side by side, on one time axis").Clicked() {
				s.post(func(s *App) { s.openChosen(picked) })
			}
			if ui.Button(c, fmt.Sprintf("Open all %d", n)).Tooltip("Every one: it takes a while, and memory").Clicked() {
				s.post(func(s *App) { s.openAll() })
			}
			ui.Spacer(c)
			if ui.Button(c, "Cancel").Clicked() {
				s.post(func(s *App) { s.choosing = nil; s.setTitle("") })
			}
		})
	})
}
