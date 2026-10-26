// Package viewer is the trace viewer's native UI, drawn by MyGo's ui
// package: a toolbar, a sidebar with the file's details and its rows, the
// timeline, and a panel with the selected event's details and summaries.
package viewer

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/remote"
	"tracelight/internal/theme"
	"tracelight/internal/trace"
	"tracelight/internal/version"
)

// App is the state of a window showing one trace.
type App struct {
	tr  *trace.Trace
	pal *palette
	tl  timeline

	hidden    map[int]bool
	collapsed map[string]bool
	selected  *trace.Ref
	// highlight is a name whose events stand out (from the summary), or "".
	highlight string

	sidebarW   float32
	timelinePc float32
	panelTab   int

	// src is where the events come from: the trace in memory, a server,
	// or a session of several (source.go).
	src source
	// sumRange is the measured range the summaries are of, once the drag
	// that measures it ends.
	sumRange struct {
		ok       bool
		from, to int64 // relative to the trace's start
	}
	fileSize int64

	summaryList ui.ListState

	// ctl is what outlasts the trace shown: the connection to a server,
	// the path box, recent paths and servers (control.go).
	ctl *control

	// noAnim turns the zoom animation off: for screenshots and tests.
	noAnim bool
	// idle are the GPUs without activity, hidden unless showIdle.
	idle     []int32
	showIdle bool

	// nav moves the view while keys are held (nav.go).
	nav nav

	// mode is the appearance the user chose; OnThemeMode, if set, is told
	// of a new choice, to keep it.
	mode        theme.Mode
	OnThemeMode func(theme.Mode)
	// navSpeed multiplies the held-key rates (theme.NavSpeeds; 0 is 1);
	// OnNavSpeed, if set, is told of a new choice, to keep it.
	navSpeed   float64
	OnNavSpeed func(float64)

	// perf times the timeline's paints; showPerf shows them (P).
	perf     perf
	showPerf bool

	// mods are the modifier keys held as of the last frame.
	mods ui.Modifiers

	// loading is the file being loaded, loadErr why it failed.
	loading string
	loadErr error
	// picker lists a server's traces (remote.go).
	picker *picker

	// The tab this is the state of (tabs.go): its id; gen, its latest open
	// (what comes back for an older one is dropped); inputs, what it was
	// opened with, as typed; title, the window's while it shows.
	tabID  int
	gen    int
	inputs string
	title  string
	// remotePath is the path on the server of the single trace streamed
	// from it (for Download for offline), downloading while it downloads.
	remotePath  string
	downloading bool
	// linkNext is the view and alignment of the link the next open from the
	// server came from (#v=, #align=), applied once it shows.
	linkNext *remote.Link
	// lazy is the link of a tab restored, not opened yet (it opens when
	// shown).
	lazy *remote.Link
}

// New makes the state for showing tr, or a welcome screen if tr is nil.
func New(tr *trace.Trace) *App {
	s := &App{
		hidden:     map[int]bool{},
		collapsed:  map[string]bool{},
		sidebarW:   270,
		timelinePc: 70,
		ctl:        newControl(),
	}
	s.tabID = s.ctl.tabs[0].id
	if tr != nil {
		s.SetTrace(tr)
	}
	return s
}

// SetTrace shows tr, forgetting the trace shown before.
func (s *App) SetTrace(tr *trace.Trace) {
	var size int64
	if fi, err := os.Stat(tr.Path); err == nil {
		size = fi.Size()
	}
	s.show(tr, newLocalSource(tr), size)
}

// show shows the rows of tr, drawn from src.
func (s *App) show(tr *trace.Trace, src source, size int64) {
	old := *s
	*s = App{
		tr:        tr,
		src:       src,
		fileSize:  size,
		pal:       newPalette(),
		hidden:    map[int]bool{},
		collapsed: map[string]bool{},
		tabID:     old.tabID,
		gen:       old.gen,
		inputs:    old.inputs,
		title:     old.title,
	}
	s.keepGlobals(&old)
	s.tl.dirty = true
	s.idle = tr.IdleDevices()
}

// SetLoading shows that path is loading, or, with err, that it failed.
func (s *App) SetLoading(path string, err error) {
	s.loading, s.loadErr = path, err
}

// SetThemeMode sets the appearance: the system's, light or dark.
func (s *App) SetThemeMode(m theme.Mode) { s.mode = m }

// ThemeMode is the appearance the user chose.
func (s *App) ThemeMode() theme.Mode {
	if s.mode == "" {
		return theme.ModeSystem
	}
	return s.mode
}

// SetNavSpeed sets the navigation speed: a multiplier of the held-key rates,
// one of theme.NavSpeeds'.
func (s *App) SetNavSpeed(v float64) { s.navSpeed = theme.ParseNavSpeed(v) }

// NavSpeed is the navigation speed chosen.
func (s *App) NavSpeed() float64 {
	if s.navSpeed == 0 {
		return 1
	}
	return s.navSpeed
}

// ShowPerf shows or hides the overlay of paint times (P).
func (s *App) ShowPerf(on bool) { s.showPerf = on }

// SetAnimations turns the smooth zoom on or off.
func (s *App) SetAnimations(on bool) { s.noAnim = !on }

// Select selects the event at r, as a click on it does.
func (s *App) Select(r trace.Ref) { s.selectRef(r) }

// ZoomTo shows the times a to b, in nanoseconds from the trace's start.
func (s *App) ZoomTo(a, b float64) {
	s.tl.fitted = true
	s.zoomTo(a, b, false)
}

// Extent is the time span of what is shown, in nanoseconds from its start
// (0, 0 for nothing).
func (s *App) Extent() (a, b float64) {
	if s.tr == nil {
		return 0, 0
	}
	return 0, float64(s.tr.Duration())
}

// Shown is the time span the timeline shows, in nanoseconds from the
// trace's start; false if nothing is shown yet.
func (s *App) Shown() (a, b float64, ok bool) {
	if s.tr == nil || s.tl.size.W == 0 {
		return 0, 0, false
	}
	return s.tl.tgt.start, s.tl.tgt.start + s.dataW()*s.tl.tgt.scale, true
}

// Hover places the pointer at x, y of the timeline, for screenshots.
func (s *App) Hover(x, y float32) {
	s.tl.mouseX, s.tl.mouseY, s.tl.hovering = x, y, true
}

// ScrollTo scrolls the rows to y DIPs from the top, as far as they go.
func (s *App) ScrollTo(y float32) {
	s.tl.scrollY = y
	s.clampScroll()
}

// Trace is the trace shown.
func (s *App) Trace() *trace.Trace { return s.tr }

// View builds the window.
func (s *App) View(c *ui.Context) {
	// The system's appearance (MyGo's default theme follows it), or the
	// user's choice.
	applyPalette(theme.For(s.mode, c.Theme().Dark))
	c.SetTheme(widgetTheme(c.Theme()))
	s.mods = c.Modifiers()
	now := c.Now()
	if s.stepNav(now.Sub(s.tl.lastFrame).Seconds()) {
		c.AnimationFrame()
	}
	if s.tl.animating {
		if s.step(c.Now()) {
			c.AnimationFrame()
		}
	} else {
		s.tl.lastFrame = c.Now()
	}
	s.drain()
	s.shortcuts(c)
	if !s.ctl.showConnect {
		s.tabShortcuts(c)
	}
	root := ui.Column(c).Fill().Background(colBg).TextColor(colText)
	if files := root.DroppedFiles(); len(files) > 0 && !s.ctl.web {
		for _, f := range files {
			s.OpenLocal(f)
		}
	}
	s.connectDialog(c)
	if s.tr == nil || s.loading != "" || (s.picker != nil && s.picker.shown) {
		root.Children(func() {
			s.tabBar(c)
			s.welcome(c)
		})
		return
	}
	root.Children(func() {
		s.toolbar(c)
		s.tabBar(c)
		ui.Split(c, &s.sidebarW, func() { s.sidebar(c) }, func() {
			ui.SplitVerticalPercent(c, &s.timelinePc, func() { s.timelineView(c) }, func() { s.panel(c) }).Fill()
		}).Grow(1)
	})
}

func (s *App) toolbar(c *ui.Context) {
	// Wherever the focus is: Esc clears the selection and the range, Enter
	// (or Z) zooms to the range.
	if c.Shortcut(0, ui.KeyEscape) {
		s.selected = nil
		s.clearRange()
	}
	if c.Shortcut(0, ui.KeyEnter) || c.Shortcut(0, ui.KeyZ) {
		s.zoomToRange()
	}
	ui.Row(c).Height(46).PaddingX(14).Gap(10).Background(colPanel).
		BorderWidth(0, 0, 1, 0).BorderColor(colBorder).Children(func() {
		// The mark: a little timeline.
		ui.Box(c).Size(22, 22).Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(r, colAccent.Alpha(0.15), 6)
			p.Fill(ui.Rect{X: r.X + 4, Y: r.Y + 5, W: 9, H: 3}, colAccent, 1.5)
			p.Fill(ui.Rect{X: r.X + 8, Y: r.Y + 10, W: 10, H: 3}, colAccentHi, 1.5)
			p.Fill(ui.Rect{X: r.X + 5, Y: r.Y + 15, W: 6, H: 3}, colAccent, 1.5)
		})
		ui.Text(c, "Tracelight").FontSize(14).Bold()
		s.pathBox(c, 420)
		s.connChip(c)
		if s.remotePath != "" && !s.ctl.web {
			label := "Download for offline"
			if s.downloading {
				label = "Downloading…"
			}
			if ui.Button(c, label).Disabled(s.downloading).
				Tooltip("Fetch the whole trace from the server, to keep showing it without the server").Clicked() {
				s.DownloadOffline()
			}
		}
		ui.Spacer(c)
		ui.Text(c, "Ctrl/⌘+wheel zoom · drag pan · hold W/S/A/D · F fit").
			FontSize(11.5).TextColor(colFaint).SingleLine().Shrink(1)
		if ui.Button(c, "−").Width(32).Tooltip("Zoom out (S)").Clicked() {
			s.zoomAt(1.6, s.dataW()/2)
		}
		if ui.Button(c, "+").Width(32).Tooltip("Zoom in (W)").Clicked() {
			s.zoomAt(1/1.6, s.dataW()/2)
		}
		s.themeControl(c)
		if s.tl.hasRange && s.tl.rangeA != s.tl.rangeB && ui.Button(c, "Zoom to range").Tooltip("Zoom to the measured range (Enter or Z)").Clicked() {
			s.zoomToRange()
		}
		if ui.PrimaryButton(c, "Fit").Tooltip("Show the whole trace (0)").Clicked() {
			s.fit(true)
		}
		if ui.Button(c, "Open…").Tooltip("Open another trace (Ctrl/⌘+O)").Clicked() {
			s.browse()
		}
	})
}

func (s *App) sidebar(c *ui.Context) {
	tr := s.tr
	ui.Scroll(c).Fill().Background(colPanel).BorderWidth(0, 1, 0, 0).BorderColor(colBorder).
		Padding(14, 12).Gap(14).Children(func() {
		section(c, "TRACE")
		card(c, func() {
			kv(c, "File", filepath.Base(tr.Path))
			if s.fileSize > 0 {
				kv(c, "Size", fmtBytes(s.fileSize))
			}
			kv(c, "Duration", fmtDur(float64(tr.Duration())))
			kv(c, "Events", fmtCount(tr.EventCount()))
			kv(c, "Rows", fmtCount(len(tr.Tracks)))
		})
		// A session's traces, near the top: which show.
		if ss := s.session(); ss != nil {
			section(c, "TRACES")
			s.tracesControl(c, ss)
		}
		section(c, "EVENTS")
		card(c, func() {
			for _, k := range []trace.Kind{trace.KindKernel, trace.KindMemcpy, trace.KindMemset, trace.KindRuntime, trace.KindNVTX, trace.KindNVTXMark, trace.KindCPUOp, trace.KindPython} {
				if n := tr.Counts[k]; n > 0 {
					kv(c, k.String(), fmtCount(n))
				}
			}
		})
		if len(tr.Devices) > 0 {
			section(c, "GPUS")
			card(c, func() {
				ids := make([]int, 0, len(tr.Devices))
				for id := range tr.Devices {
					ids = append(ids, int(id))
				}
				slices.Sort(ids)
				for _, id := range ids {
					if !slices.Contains(s.idle, int32(id)) {
						kv(c, fmt.Sprintf("GPU %d", id), tr.Devices[int32(id)])
					}
				}
				if len(s.idle) == 0 {
					return
				}
				show := s.showIdle
				label := fmt.Sprintf("Show %d idle GPUs", len(s.idle))
				if len(s.idle) == 1 {
					label = "Show 1 idle GPU"
				}
				ui.Row(c).Gap(8).MarginY(2).Children(func() {
					ui.Text(c, label).FontSize(12).TextColor(colMuted).Grow(1)
					if ui.Switch(c.Key("idle"), &show).Label(label).Changed() {
						s.setShowIdle(show)
					}
				})
				if s.showIdle {
					for _, id := range s.idle {
						ui.Row(c).Gap(8).Children(func() {
							ui.Text(c, fmt.Sprintf("GPU %d", id)).FontSize(12).TextColor(colFaint)
							ui.Spacer(c)
							ui.Text(c, tr.Devices[id]+" · idle").FontSize(12).TextColor(colFaint).SingleLine().Shrink(1)
						})
					}
				}
			})
		}
		if len(tr.Missing) > 0 || len(tr.Warnings) > 0 {
			section(c, "NOTES")
			card(c, func() {
				for _, m := range tr.Missing {
					ui.Text(c, "No "+m).FontSize(11.5).TextColor(colFaint)
				}
				for _, w := range tr.Warnings {
					ui.Text(c, w).FontSize(11.5).TextColor(colWarn)
				}
			})
		}
		if _, ok := s.src.(*sessionSource); ok {
			section(c, "ALIGN")
			ui.Column(c).Gap(6).Children(func() { s.alignControl(c) })
		}
		section(c, "DENSITY")
		ui.Column(c).Gap(6).Children(func() { s.compactControl(c) })
		section(c, "NAVIGATION SPEED")
		// Not in a card: the four choices need the sidebar's whole width.
		ui.Column(c).Gap(6).Children(func() {
			s.navSpeedControl(c)
			ui.Text(c, "How fast held W/A/S/D move the view").FontSize(11.5).TextColor(colFaint)
		})
		section(c, "ROWS")
		ui.Column(c).Gap(2).Children(func() {
			group := ""
			for i, t := range tr.Tracks {
				if t.Group != group {
					group = t.Group
					g := group
					open := !s.collapsed[g]
					hdr := ui.Row(c.Key("g:"+g)).Gap(6).Padding(5, 4).Radius(5).Children(func() {
						ui.Text(c, map[bool]string{true: "▾", false: "▸"}[open]).FontSize(11).TextColor(colMuted).Width(10)
						ui.Text(c, g).FontSize(12).Bold().SingleLine().Grow(1)
					})
					if hdr.Hovered() {
						hdr.Background(colPanelAlt)
					}
					if hdr.Clicked() {
						s.collapsed[g] = open
						s.tl.dirty = true
					}
				}
				if s.collapsed[group] {
					continue
				}
				shown := !s.hidden[i]
				ui.Row(c.Key(i)).Gap(6).PaddingStart(14).Children(func() {
					if ui.Checkbox(c.Key("cb"), &shown, "").Label("Show " + t.Name).Changed() {
						s.hidden[i] = !shown
						s.tl.dirty = true
					}
					name := ui.Text(c, t.Name).FontSize(11.5).SingleLine().Grow(1).Shrink(1)
					if !shown {
						name.TextColor(colFaint)
					}
					ui.Text(c, compact(t.Count)).FontSize(10.5).TextColor(colFaint)
				})
			}
		})
	})
}

func section(c *ui.Context, title string) {
	ui.Text(c, title).FontSize(10.5).Bold().TextColor(colFaint).LetterSpacing(0.8)
}

func card(c *ui.Context, children func()) {
	ui.Column(c).Gap(6).Padding(10, 12).Radius(8).Background(colPanelAlt).
		Border(1, colBorder).Children(children)
}

func kv(c *ui.Context, k, v string) {
	ui.Row(c).Gap(8).Children(func() {
		ui.Text(c, k).FontSize(12).TextColor(colMuted)
		ui.Spacer(c)
		ui.Text(c, v).FontSize(12).SingleLine().Shrink(1)
	})
}

func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return trimFloat(float64(n)/1e6, 1) + "M"
	case n >= 10_000:
		return trimFloat(float64(n)/1e3, 1) + "k"
	}
	return fmtCount(n)
}

func (s *App) timelineView(c *ui.Context) {
	canvas := ui.Box(c.Key("timeline")).Fill().Clip().Focusable().FocusRing(false).AutoFocus().
		Label("Timeline").Cursor(s.cursor()).
		HandleInput(s.input).
		Draw(s.paint)
	if !canvas.Hovered() && !s.tl.dragging {
		s.tl.hovering = false
	}
	// Keys let go of while the timeline had no keyboard (another control,
	// another window) never come up here: stop.
	if !canvas.Focused() {
		s.nav.stop()
	}
}

func (s *App) cursor() ui.Cursor {
	if s.tl.dragging && s.tl.dragMoved && !s.tl.rangeDrag {
		return ui.CursorGrabbing
	}
	if s.tl.rangeDrag || s.tl.gutterDrag || (s.tl.hovering && !s.tl.dragging && s.onGutterEdge(s.tl.mouseX)) {
		return ui.CursorResizeColumn
	}
	return ui.CursorDefault
}

// panel is the bottom panel: the selection's details and the summaries.
func (s *App) panel(c *ui.Context) {
	ui.Column(c).Fill().Background(colPanel).BorderWidth(1, 0, 0, 0).BorderColor(colBorder).Children(func() {
		ui.Row(c).PaddingX(10).PaddingY(6).Gap(10).Children(func() {
			tabs := []string{"Details", "Top kernels", "Top CUDA API", "Top NVTX ranges"}
			if s.tr.Counts[trace.KindCPUOp] > 0 { // a PyTorch profile's operators
				tabs = append(tabs, "Top CPU ops")
			}
			s.panelTab = min(s.panelTab, len(tabs)-1)
			ui.Tabs(c, &s.panelTab, tabs...)
			ui.Spacer(c)
			if s.highlight != "" {
				ui.Text(c, "Highlighting "+ellipsize(s.highlight, 50)).FontSize(11.5).TextColor(colMuted)
				if ui.Button(c, "Clear").Clicked() {
					s.highlight = ""
				}
			}
		})
		switch s.panelTab {
		case 0:
			s.details(c)
		case 1:
			s.summary(c, trace.KindKernel, "GPU time")
		case 2:
			s.summary(c, trace.KindRuntime, "CPU time in CUDA API")
		case 3:
			s.summary(c, trace.KindNVTX, "time in ranges")
		case 4:
			s.summary(c, trace.KindCPUOp, "CPU time in operators")
		}
	})
}

// selectedInfo is the selected event's details, once they are at hand.
func (s *App) selectedInfo() (*eventInfo, bool) {
	if s.selected == nil {
		return nil, false
	}
	return s.src.event(*s.selected)
}

func (s *App) details(c *ui.Context) {
	ui.Scroll(c).Grow(1).Padding(8, 16, 14, 16).Gap(8).Children(func() {
		if s.selected == nil {
			ui.Text(c, "Click an event on the timeline to see its details. Double-click zooms to it.").TextColor(colMuted)
			return
		}
		ref := *s.selected
		e, ok := s.src.event(ref)
		if !ok {
			ui.Row(c).Gap(8).Children(func() {
				ui.Spinner(c)
				ui.Text(c, "Loading the event's details…").TextColor(colMuted)
			})
			return
		}
		tk := s.tr.Tracks[ref.Track]
		col := s.pal.of(e.name, e.kind)
		// The name, then the actions, which wrap under a long name.
		ui.Row(c).Gap(10).Wrap().Children(func() {
			ui.Row(c).Gap(10).Grow(1).Shrink(1).Basis(0).MinWidth(320).Children(func() {
				ui.Box(c).Size(4, 18).Radius(2).Background(col)
				ui.Text(c, e.name).FontSize(15).Bold().MaxLines(2).Shrink(1).Selectable()
				ui.Badge(c, e.kind.String())
			})
			ui.Row(c).Gap(10).Children(func() { s.detailActions(c, e) })
		})
		// With a Python or an operator stack, they go beside the rest.
		if e.python || len(e.ops) > 0 {
			ui.Row(c).Gap(24).AlignItems(ui.Start).Children(func() {
				ui.Column(c).Gap(8).Grow(1).Basis(0).Children(func() { s.detailFields(c, e, ref, tk, 2) })
				ui.Column(c).Gap(4).Grow(1.15).Basis(0).Children(func() {
					if e.python {
						s.pythonStack(c, e)
					}
					if len(e.ops) > 0 {
						s.operatorStack(c, e)
					}
				})
			})
			return
		}
		s.detailFields(c, e, ref, tk, 4)
	})
}

// detailFields shows an event's fields in cols columns, its demangled
// name, and the event its correlation id links it to.
func (s *App) detailFields(c *ui.Context, e *eventInfo, ref trace.Ref, tk *trace.Track, cols int) {
	dur := e.end - e.start
	ui.Grid(c).Columns(cols).GapX(28).GapY(6).Children(func() {
		field(c, "Row", e.group+" › "+e.track)
		field(c, "Duration", fmtDur(float64(dur)))
		field(c, "Start", fmtTime(float64(e.start), 0.1))
		field(c, "End", fmtTime(float64(e.end), 0.1))
		if k := e.kernel; k != nil {
			field(c, "Grid", fmt.Sprintf("%d × %d × %d", k.GridX, k.GridY, k.GridZ))
			field(c, "Block", fmt.Sprintf("%d × %d × %d", k.BlockX, k.BlockY, k.BlockZ))
			field(c, "Registers / thread", fmt.Sprint(k.Registers))
			field(c, "Shared memory", fmtBytes(int64(k.StaticShared))+" static · "+fmtBytes(int64(k.DynamicShared))+" dynamic")
		}
		if m := e.memop; m != nil {
			field(c, "Size", fmtBytes(m.Bytes))
			if dur > 0 {
				field(c, "Throughput", trimFloat(float64(m.Bytes)/float64(dur), 2)+" GB/s")
			}
			if e.kind == trace.KindMemset {
				field(c, "Value", fmt.Sprint(m.Value))
			}
		}
		if e.corr != 0 {
			field(c, "Correlation ID", fmt.Sprint(e.corr))
		}
		if l := tk.Lanes; len(l) > 1 && tk.Kind == trace.TrackNVTX {
			field(c, "Depth", fmt.Sprint(ref.Lane))
		}
	})
	if e.full != "" && e.full != e.name {
		ui.Text(c, "Demangled name").FontSize(11).TextColor(colFaint)
		ui.Text(c, e.full).FontSize(11.5).Font("monospace").TextColor(colMuted).MaxLines(3).Selectable()
	}
	if o := e.linked; o != nil {
		label := "Launched by"
		lat := e.start - o.start
		if e.kind == trace.KindRuntime {
			label = "Launched"
			lat = o.start - e.start
		}
		ui.Row(c).Gap(8).Padding(8, 10).Radius(8).Background(colPanelAlt).Border(1, colBorder).Children(func() {
			ui.Text(c, label).FontSize(12).TextColor(colFaint)
			ui.Text(c, ellipsize(o.name, 80)).FontSize(12).Bold().Shrink(1).SingleLine()
			ui.Text(c, "on "+o.group+" › "+o.track).FontSize(12).TextColor(colMuted).SingleLine().Shrink(1)
			ui.Text(c, "· launch to start "+fmtDur(float64(lat))).FontSize(12).TextColor(colMuted)
			ui.Spacer(c)
			if ui.Button(c, "Go to").Clicked() {
				s.goToLinked()
			}
		})
	}
}

// detailActions are the selected event's buttons: zoom to it, highlight
// its name, and go to the event its correlation id links it to (up here,
// where it shows without scrolling the details).
func (s *App) detailActions(c *ui.Context, e *eventInfo) {
	if ui.Button(c, "Zoom to event").Tooltip("F").Clicked() {
		s.zoomTo(float64(e.start), float64(e.end), true)
	}
	if ui.Button(c, "Highlight name").Clicked() {
		s.highlight = e.name
	}
	if e.linked != nil && ui.PrimaryButton(c, goToLabel(e)).Tooltip("Select "+ellipsize(e.linked.name, 60)+" and show it with this one (G)").Clicked() {
		s.goToLinked()
	}
}

// pythonStack lists the Python stack an event ran under, the innermost
// frame first, as a debugger's call stack does: each frame's function and
// where it is, the program's own frames stronger than the libraries'; a
// click selects the frame's call. For GPU work, it is the stack of the
// CUDA API call that launched it.
func (s *App) pythonStack(c *ui.Context, e *eventInfo) {
	title := "Python stack"
	if e.linked != nil && e.kind != trace.KindRuntime && len(e.stack) > 0 {
		title += " where it was launched"
	}
	title += " · innermost first"
	ui.Text(c, title).FontSize(11).TextColor(colFaint).MarginY(4)
	if len(e.stack) == 0 {
		why := "No Python calls on this thread around it."
		if o := e.linked; o != nil && e.kind != trace.KindRuntime {
			why = "No Python stack: launched from " + o.track + ", which ran no Python then (autograd's thread runs the backward pass without one)."
		} else if e.linked == nil && (e.kind == trace.KindKernel || e.kind == trace.KindMemcpy || e.kind == trace.KindMemset) {
			why = "No Python stack: the CUDA call that launched it is not in the trace."
		}
		ui.Text(c, why).FontSize(12).TextColor(colMuted).MaxWidth(720)
		return
	}
	ui.Column(c).Gap(1).Padding(6, 8).Radius(8).Background(colPanelAlt).Border(1, colBorder).Children(func() {
		for i := len(e.stack) - 1; i >= 0; i-- {
			f := e.stack[i]
			fn, where := splitFrame(f.Name)
			lib := isLibraryFrame(where)
			row := ui.Row(c.Key(i)).Gap(10).Padding(3, 6).Radius(4).Focusable().Label("Python frame " + f.Name).
				Tooltip("Select this call (" + f.Name + ")")
			if row.Hovered() || row.Focused() {
				row.Background(colPanel)
			}
			if row.Clicked() {
				s.selectRef(f.Ref)
				s.revealTrack(int(f.Ref.Track))
			}
			row.Children(func() {
				name := ui.Text(c, fn).FontSize(12).Font("monospace").SingleLine().Shrink(1)
				loc := ui.Text(c, where).FontSize(11.5).TextColor(colFaint).SingleLine().Shrink(2)
				if lib {
					name.TextColor(colMuted)
				} else {
					name.Bold()
					loc.TextColor(colMuted)
				}
			})
		}
	})
}

// operatorStack lists the operators and annotations an event ran under on
// its thread, the innermost first (for GPU work, those of the CUDA call
// that launched it); a click selects one.
func (s *App) operatorStack(c *ui.Context, e *eventInfo) {
	title := "Operator stack"
	if e.linked != nil && e.kind != trace.KindRuntime && e.kind != trace.KindCPUOp {
		title += " where it was launched"
	}
	ui.Text(c, title+" · innermost first").FontSize(11).TextColor(colFaint).MarginY(4)
	ui.Column(c).Gap(1).Padding(6, 8).Radius(8).Background(colPanelAlt).Border(1, colBorder).Children(func() {
		for i := len(e.ops) - 1; i >= 0; i-- {
			f := e.ops[i]
			row := ui.Row(c.Key("op"+strconv.Itoa(i))).Gap(10).Padding(3, 6).Radius(4).Focusable().Label("Operator " + f.Name).Tooltip("Select it")
			if row.Hovered() || row.Focused() {
				row.Background(colPanel)
			}
			if row.Clicked() {
				s.selectRef(f.Ref)
				s.revealTrack(int(f.Ref.Track))
			}
			row.Children(func() {
				ui.Text(c, f.Name).FontSize(12).Font("monospace").SingleLine().Shrink(1)
			})
		}
	})
}

// splitFrame splits a frame as PyTorch's profiler names it ("model.py(52):
// forward") into the function and where it is ("model.py:52").
func splitFrame(name string) (fn, where string) {
	loc, f, ok := strings.Cut(name, "): ")
	if !ok {
		return name, ""
	}
	if i := strings.LastIndexByte(loc, '('); i >= 0 {
		loc = loc[:i] + ":" + loc[i+1:]
	}
	return f, loc
}

// isLibraryFrame reports whether a frame is in a library (PyTorch, Python's
// own, an installed package) rather than the program.
func isLibraryFrame(where string) bool {
	return where == "" || strings.HasPrefix(where, "torch/") || strings.Contains(where, "/torch/") || strings.Contains(where, "site-packages") ||
		strings.Contains(where, "dist-packages") || strings.HasPrefix(where, "runpy.py") || strings.HasPrefix(where, "<")
}

func field(c *ui.Context, k, v string) {
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, k).FontSize(11).TextColor(colFaint)
		ui.Text(c, v).FontSize(12.5).Selectable()
	})
}

func ellipsize(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// summary lists names by their total time, with a bar of their share.
// stats is the top 100 names of a kind: over the measured range, each event
// by its overlap with it (trace.TopInRange), or over the whole trace; false
// while a server works them out.
func (s *App) stats(k trace.Kind) ([]trace.NameStat, bool) {
	return s.src.top(k, s.sumRange.ok, s.sumRange.from, s.sumRange.to)
}

// rangeEnded makes the range just measured the summaries' (none if empty).
func (s *App) rangeEnded() {
	tl := &s.tl
	a, b := min(tl.rangeA, tl.rangeB), max(tl.rangeA, tl.rangeB)
	s.sumRange.ok = tl.hasRange && b > a
	s.sumRange.from, s.sumRange.to = int64(math.Floor(a)), int64(math.Ceil(b))
}

// clearRange removes the measured range.
func (s *App) clearRange() {
	s.tl.hasRange = false
	s.sumRange.ok = false
}

// zoomToRange shows the measured range.
func (s *App) zoomToRange() {
	if tl := &s.tl; tl.hasRange && tl.rangeA != tl.rangeB {
		s.zoomTo(min(tl.rangeA, tl.rangeB), max(tl.rangeA, tl.rangeB), true)
	}
}

func (s *App) summary(c *ui.Context, kind trace.Kind, what string) {
	stats, ready := s.stats(kind)
	ui.Row(c).PaddingX(16).PaddingY(6).Gap(4).Children(func() {
		if s.sumRange.ok {
			a, b := float64(s.sumRange.from), float64(s.sumRange.to)
			ui.Text(c, "In range").FontSize(12).TextColor(colMuted)
			ui.Text(c, fmtTime(a, (b-a)/1000)+"–"+fmtTime(b, (b-a)/1000)).FontSize(12).Bold()
			ui.Text(c, "("+fmtDur(b-a)+") · events clipped to it").FontSize(12).TextColor(colMuted)
		} else {
			ui.Text(c, "Whole trace · Shift+drag on the timeline to sum a range").FontSize(12).TextColor(colMuted)
		}
		if ss, ok := s.src.(*sessionSource); ok {
			ui.Text(c, "· in "+ss.parts[ss.focus].label+", the trace in focus").FontSize(12).TextColor(colMuted)
		}
	})
	if !ready {
		ui.Row(c).Padding(16).Gap(8).Children(func() {
			ui.Spinner(c)
			ui.Text(c, "Summing on the server…").TextColor(colMuted)
		})
		return
	}
	if len(stats) == 0 {
		where := "trace"
		if s.sumRange.ok {
			where = "range"
		}
		ui.Column(c).Padding(16).Children(func() {
			ui.Text(c, "No events of this kind in the "+where+".").TextColor(colMuted)
		})
		return
	}
	cols := []float32{0, 70, 92, 84, 84, 84, 150}
	header := []string{"Name", "Count", "Total", "Avg", "Min", "Max", "Share of " + what}
	ui.Row(c).PaddingX(16).PaddingY(4).Gap(12).BorderWidth(0, 0, 1, 0).BorderColor(colBorder).Children(func() {
		for i, h := range header {
			t := ui.Text(c, h).FontSize(11).Bold().TextColor(colFaint).SingleLine()
			if i == 0 {
				t.Grow(1)
			} else {
				t.Width(cols[i]).TextAlign(ui.End)
			}
		}
	})
	ui.List(c.Key("summary:"+what), &s.summaryList, len(stats)).Grow(1).Rows(func(r ui.ListRow) {
		st := stats[r.Index]
		name := st.Name
		row := ui.Row(r.Context).PaddingX(16).PaddingY(4).Gap(12).Radius(4)
		active := s.highlight != "" && s.highlight == name
		if active {
			row.Background(colAccent.Alpha(0.14))
		} else if row.Hovered() {
			row.Background(colPanelAlt)
		}
		if row.Clicked() {
			if active {
				s.highlight = ""
			} else {
				s.highlight = name
			}
		}
		row.Children(func() {
			c := r.Context
			ui.Text(c, st.Name).FontSize(12).SingleLine().Grow(1).Shrink(1).Tooltip(st.Name)
			num := func(i int, v string) { ui.Text(c, v).FontSize(12).Width(cols[i]).TextAlign(ui.End).SingleLine() }
			num(1, fmtCount(st.Count))
			num(2, fmtDur(float64(st.Total)))
			num(3, fmtDur(float64(st.Avg())))
			num(4, fmtDur(float64(st.Min)))
			num(5, fmtDur(float64(st.Max)))
			share := st.Share
			ui.Row(c).Width(cols[6]).Gap(8).Children(func() {
				ui.Box(c).Height(6).Grow(1).Radius(3).Background(colPanelAlt).Draw(func(p *ui.Painter, b ui.Rect) {
					p.Fill(ui.Rect{X: b.X, Y: b.Y, W: max(b.W*float32(share), 1), H: b.H}, colAccent, 3)
				})
				ui.Text(c, trimFloat(100*share, 1)+"%").FontSize(11.5).TextColor(colMuted).Width(44).TextAlign(ui.End)
			})
		})
	})
}

// Title is the window's title.
func Title(path string) string {
	if path == "" {
		return "Tracelight"
	}
	return "Tracelight — " + strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// ShowcaseEvent picks an event worth selecting in a screenshot: the longest
// run of the kernel with the most total time, if the trace has kernels.
func ShowcaseEvent(tr *trace.Trace) (trace.Ref, bool) {
	top := tr.TopByTotal(1, trace.KindKernel)
	if len(top) == 0 {
		return trace.Ref{}, false
	}
	var best trace.Ref
	var bestDur int64 = -1
	mid := tr.Start + tr.Duration()/2
	for ti, tk := range tr.Tracks {
		for li, l := range tk.Lanes {
			// The first run of that kernel after the middle of the trace.
			i := l.FirstEnding(mid)
			for k := i; k < len(l.Events) && k < i+400; k++ {
				e := &l.Events[k]
				if e.Kind == trace.KindKernel && tr.Names[e.Name] == top[0].Name && e.Dur() > bestDur {
					best, bestDur = trace.Ref{Track: int32(ti), Lane: int32(li), Index: int32(k)}, e.Dur()
				}
			}
		}
	}
	return best, bestDur >= 0
}

// EventCenter returns where the event at r shows in the window, if it does.
func (s *App) EventCenter(r trace.Ref) (x, y float32, ok bool) {
	b, ok := s.eventBox(r)
	if !ok {
		return 0, 0, false
	}
	vis := ui.Rect{X: s.tl.size.X + s.labelW(), Y: s.tl.size.Y + rulerH, W: s.tl.size.W - s.labelW(), H: s.tl.size.H - rulerH}
	x0, x1 := max(b.X, vis.X), min(b.X+b.W, vis.X+vis.W)
	if x1 <= x0 || b.Y < vis.Y || b.Y > vis.Y+vis.H {
		return 0, 0, false
	}
	return (x0 + x1) / 2, b.Y + b.H/2, true
}

// welcome is the window without a trace: while one loads, after one failed
// to, or before one is chosen.
func (s *App) welcome(c *ui.Context) {
	ui.Column(c).Fill().Center().Gap(14).Padding(40).Children(func() {
		ui.Box(c).Size(56, 56).Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(r, colAccent.Alpha(0.14), 14)
			p.Fill(ui.Rect{X: r.X + 10, Y: r.Y + 14, W: 22, H: 7}, colAccent, 3.5)
			p.Fill(ui.Rect{X: r.X + 20, Y: r.Y + 25, W: 26, H: 7}, colAccentHi, 3.5)
			p.Fill(ui.Rect{X: r.X + 13, Y: r.Y + 36, W: 14, H: 7}, colAccent, 3.5)
		})
		ui.Row(c).Gap(8).Children(func() {
			ui.Text(c, "Tracelight").FontSize(24).Bold()
			ui.Text(c, version.Version).FontSize(13).TextColor(colFaint)
		})
		switch {
		case s.loading != "" && s.loadErr == nil:
			ui.Row(c).Gap(10).Children(func() {
				ui.Spinner(c)
				ui.Text(c, "Loading "+filepath.Base(s.loading)+"…").TextColor(colMuted)
			})
		case s.loadErr != nil:
			ui.Text(c, "Could not open "+filepath.Base(s.loading)).FontSize(15).Bold().TextColor(colWarn)
			ui.Text(c, s.loadErr.Error()).TextColor(colMuted).MaxWidth(640).TextAlign(ui.Center).Selectable()
		default:
			ui.Text(c, "An open viewer for NVIDIA Nsight Systems traces").TextColor(colMuted)
		}
		ui.Row(c.Key("path-row")).Gap(10).MarginY(6).Children(func() {
			s.pathBox(c, 560)
			s.connChip(c)
		})
		if s.picker != nil && (s.loading == "" || s.loadErr != nil || s.picker.shown) {
			s.remotePicker(c)
			return
		}
		if s.loading == "" || s.loadErr != nil {
			hint := "Type the path of a SQLite export (nsys export --type sqlite), an .nsys-rep report, or a folder of them (one per rank, side by side), or drop a file here."
			if s.ctl.web {
				hint = "Type the path of a trace on the server (a SQLite export or an .nsys-rep report), or of a folder of them (one per rank, side by side)."
			}
			ui.Text(c, hint).FontSize(12.5).TextColor(colFaint).MaxWidth(560).TextAlign(ui.Center)
			ui.Row(c).Gap(10).Children(func() {
				if s.ctl.browse != nil && ui.PrimaryButton(c, "Open trace…").Tooltip("Ctrl/⌘+O").Clicked() {
					s.ctl.browse()
				}
				if !s.ctl.web && ui.Button(c, "Connect to server…").Tooltip("Ctrl/⌘+K").Clicked() {
					s.openConnect()
				}
				if s.ctl.state == connConnected && ui.Button(c, "Browse the server…").Clicked() {
					s.ShowRemote()
				}
			})
		}
	})
}

var themeModes = []theme.Mode{theme.ModeSystem, theme.ModeLight, theme.ModeDark}

// themeControl chooses the appearance: the system's, light or dark.
func (s *App) themeControl(c *ui.Context) {
	i := 0
	for k, m := range themeModes {
		if m == s.ThemeMode() {
			i = k
		}
	}
	if ui.Segmented(c.Key("appearance"), &i, "System", "Light", "Dark").Label("Appearance").Changed() {
		s.mode = themeModes[i]
		if s.OnThemeMode != nil {
			s.OnThemeMode(s.mode)
		}
	}
}

// navSpeedControl chooses how fast W/A/S/D move the view.
func (s *App) navSpeedControl(c *ui.Context) {
	i, names := 1, make([]string, len(theme.NavSpeeds))
	for k, v := range theme.NavSpeeds {
		names[k] = v.Name
		if v.Times == s.NavSpeed() {
			i = k
		}
	}
	if ui.Segmented(c.Key("nav-speed"), &i, names...).Label("Navigation speed").Changed() {
		s.navSpeed = theme.NavSpeeds[i].Times
		if s.OnNavSpeed != nil {
			s.OnNavSpeed(s.navSpeed)
		}
	}
}
