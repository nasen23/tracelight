// Package theme is the one source of the viewer's look, for both of its
// renderers: the desktop app draws with these values (package viewer), and
// the server sends them to the browser client as theme.js, generated here,
// so the two cannot drift apart. See DESIGN.md.
//
// There are two palettes, Dark and Light; each app picks one from the
// system's appearance, or from the user's choice (Mode).
package theme

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/trace"
)

// Palette is the colors of one appearance, as #rrggbb.
type Palette struct {
	Name string
	Dark bool

	Bg       string // the timeline's background
	Panel    string // the sidebar, the toolbar, the panel
	PanelAlt string // cards, group headings
	RowAlt   string // every other row of the timeline
	Border   string
	Text     string
	Muted    string
	Faint    string

	Accent        string
	AccentHi      string
	AccentPressed string
	AccentText    string // text on the accent

	SurfaceHover   string
	SurfacePressed string
	Scrollbar      string

	Grid        string // the ruler's lines across the rows
	Ruler       string
	Selection   string // the outline of the selected event
	Tooltip     string // tooltips' background (their text is Text)
	Inverse     string // the ruler's time label at the pointer
	InverseText string
	OnEvent     string // labels on events
	Range       string
	Warn        string
	Shadow      string // shadows of tooltips and dialogs, #rrggbbaa

	SwatchStream, SwatchRuntime, SwatchNVTX string
	SwatchCPU, SwatchPython                 string // a framework's operators, Python calls

	MemcpyHtoD, MemcpyDtoH, MemcpyDtoD, MemcpyPtoP, MemcpyOther, Memset string

	// tones are the lightness and chroma (OKLCH) of events by kind; their
	// hue comes from their name.
	tones map[string]tone
}

type tone struct{ l, c float32 }

// Dark is the palette of a dark appearance.
var Dark = &Palette{
	Name: "dark", Dark: true,
	Bg: "#0d1016", Panel: "#131720", PanelAlt: "#171c27", RowAlt: "#111520", Border: "#252b38",
	Text: "#e6e9f0", Muted: "#8a93a8", Faint: "#5b647a",
	Accent: "#76b900", AccentHi: "#8fd014", AccentPressed: "#649e00", AccentText: "#0b0e14", // a nod to NVIDIA's green
	SurfaceHover: "#1f2633", SurfacePressed: "#262e3d", Scrollbar: "#3a4254",
	Grid: "#1c2230", Ruler: "#10141c", Selection: "#ffffff", Tooltip: "#1e2431",
	Inverse: "#e6e9f0", InverseText: "#0b0e14", OnEvent: "#0b0e14",
	Range: "#5aa9ff", Warn: "#f0b35a", Shadow: "#00000080",
	SwatchStream: "#76b900", SwatchRuntime: "#5aa9ff", SwatchNVTX: "#e8b04b", SwatchCPU: "#f97066", SwatchPython: "#a78bfa",
	MemcpyHtoD: "#3ecf8e", MemcpyDtoH: "#c084fc", MemcpyDtoD: "#f59e0b", MemcpyPtoP: "#f472b6", MemcpyOther: "#94a3b8", Memset: "#67e8f9",
	tones: map[string]tone{
		// Kernels in three shades (Shade): neighbors differ in lightness,
		// not only in hue, and stand apart as in nsys.
		"kernel":   {0.72, 0.16},
		"kernel.1": {0.60, 0.15},
		"kernel.2": {0.83, 0.12},
		"runtime":  {0.66, 0.08},
		"nvtx":     {0.78, 0.10},
		"cpu":      {0.74, 0.11},
		"python":   {0.80, 0.07},
		"other":    {0.70, 0.10},
	},
}

// Light is the palette of a light appearance: designed for it, not
// inverted. Events are a little darker and more saturated than in the dark,
// to stand out against white and keep dark labels readable; the accent is a
// deeper green that holds contrast on white.
var Light = &Palette{
	Name: "light", Dark: false,
	Bg: "#f6f7f9", Panel: "#ffffff", PanelAlt: "#f0f2f5", RowAlt: "#eef1f5", Border: "#dde1e8",
	Text: "#1a1f2b", Muted: "#566074", Faint: "#8a93a5",
	Accent: "#4c8a00", AccentHi: "#5a9e00", AccentPressed: "#3f7400", AccentText: "#ffffff",
	SurfaceHover: "#e9ecf1", SurfacePressed: "#dfe3ea", Scrollbar: "#c3c9d4",
	Grid: "#e3e7ed", Ruler: "#eceff3", Selection: "#1a1f2b", Tooltip: "#ffffff",
	Inverse: "#1a1f2b", InverseText: "#ffffff", OnEvent: "#10131a",
	Range: "#2f7de1", Warn: "#a8650a", Shadow: "#1a1f2b30",
	SwatchStream: "#4c8a00", SwatchRuntime: "#2f7de1", SwatchNVTX: "#c58b12", SwatchCPU: "#d94a3f", SwatchPython: "#7c5cd6",
	MemcpyHtoD: "#1fae6c", MemcpyDtoH: "#9b5de5", MemcpyDtoD: "#df8a00", MemcpyPtoP: "#dc4f97", MemcpyOther: "#7b879c", Memset: "#1fb0cc",
	tones: map[string]tone{
		"kernel":   {0.70, 0.16},
		"kernel.1": {0.58, 0.15},
		"kernel.2": {0.81, 0.13},
		"runtime":  {0.67, 0.09},
		"nvtx":     {0.79, 0.11},
		"cpu":      {0.72, 0.12},
		"python":   {0.81, 0.08},
		"other":    {0.69, 0.11},
	},
}

// Palettes lists the palettes by name.
var Palettes = map[string]*Palette{"dark": Dark, "light": Light}

// Mode is which appearance the user chose.
type Mode string

const (
	ModeSystem Mode = "system" // follow the system's appearance
	ModeLight  Mode = "light"
	ModeDark   Mode = "dark"
)

// ParseMode reads a Mode, ModeSystem for anything else.
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModeLight, ModeDark:
		return Mode(s)
	}
	return ModeSystem
}

// For returns the palette to use in mode m when the system is dark or not.
func For(m Mode, systemDark bool) *Palette {
	switch m {
	case ModeLight:
		return Light
	case ModeDark:
		return Dark
	}
	if systemDark {
		return Dark
	}
	return Light
}

// Metrics of the timeline, in DIPs (CSS pixels in browsers).
const (
	RulerH = 30
	LabelW = 236
	// LabelMinGutter and LabelMaxGutter bound the label gutter's width as
	// its edge is dragged.
	LabelMinGutter = 120
	LabelMaxGutter = 640
	LaneH          = 20
	TrackPad       = 4
	// DenseLaneH and DensePad are the lanes of the rows of short and many
	// events, GPU streams and CUDA API calls, and the space above and below
	// them: denser than the other rows' (LaneH, TrackPad), whose nested
	// ranges have names to read. There are many such rows, mostly sparse.
	DenseLaneH = 16
	DensePad   = 1
	// LabelMinW is the narrowest an event's box gets a name, its first
	// letters if no more fits (before 0.5.3, 33).
	LabelMinW = 18
	GroupH    = 26
	ToggleH   = 22
	// MinScale is the deepest zoom, in nanoseconds a pixel.
	MinScale = 0.02
	// ZoomPerNotch is how much a notch of a mouse wheel zooms.
	ZoomPerNotch = 1.35
)

// The rank overview (in code, "compact"), for many traces (ranks) side by
// side: each trace is a block of a busy/idle strip and a summary lane per
// kind of row it has (CompactLanes), every row of that kind drawn into it,
// the trace's name in the label gutter, kept in view as its block scrolls.
// Its lanes are as tall as the full view's, with the same event names in
// them: 0.3.1 drew 12-pixel lanes without names, 9.5-pixel labels and
// 5-pixel gaps to fit 16 ranks on one screen, which read as far too dense
// (on a laptop most of all); now 8 ranks fit with the default lanes (GPU
// and NVTX) on a 14-inch laptop, and more scroll. Since 0.3.7 a summary
// lane is a few sub-lanes (trace.Trace.Overview): work that runs at the
// same time (kernels of several streams, nested NVTX ranges) side by side
// instead of over each other; "overlaid" draws each kind in one again.
const (
	// CompactLaneH is the height of a summary lane's sub-lane: the full
	// view's lane.
	CompactLaneH = LaneH
	// CompactKindGap is the space between a trace's summary lanes.
	CompactKindGap = 4
	// CompactStripH is the height of a trace's busy/idle strip: the share
	// of each pixel column its GPUs are busy, idle gaps tinted.
	CompactStripH = 8
	// CompactGap is the space between traces, a separator line in it.
	CompactGap = 12
	// CompactAuto: the overview turns on by itself with more traces open
	// than this (the user's choice overrides it).
	CompactAuto = 4
	// CompactIdle is the busy share under which a column counts as idle.
	CompactIdle = 0.05
)

// CompactLane is a summary lane of the compact view: the rows of one kind
// ("stream", "api", "nvtx", as rows are of a kind) drawn as one lane.
type CompactLane struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// CompactLanes are the overview's summary lanes, in order: the GPU's, then
// a thread's as in the full view, its stack from the outermost (NVTX) to
// the CUDA API calls at the bottom.
var CompactLanes = []CompactLane{{"gpu", "GPU", "stream"}, {"nvtx", "NVTX", "nvtx"}, {"python", "Python", "python"},
	{"cpu", "CPU ops", "cpu"}, {"api", "CUDA API", "api"}}

// CompactDefault are the summary lanes shown until the user picks: the GPU
// work and the NVTX phases, what lines ranks up; the CUDA API lane mostly
// repeats the GPU lane's launches and the strip's waits.
var CompactDefault = []string{"gpu", "nvtx"}

// Tabs: each keeps a timeline open (a trace, or a session of several),
// switched between without loading again; the tab bar sits under the
// toolbar.
const (
	// TabBarH is the tab bar's height.
	TabBarH = 34
	// TabMaxW and TabMinW bound a tab's width: tabs share the bar, as
	// wide as TabMaxW while they fit.
	TabMaxW = 220
	TabMinW = 96
	// MaxTabs is how many tabs may be open at once.
	MaxTabs = 24
	// HiddenTabTiles is how many tiles a streamed tab keeps (the most
	// recently drawn, across its traces) while another is shown: its view
	// draws at once when it is shown again, and many tabs stay light.
	HiddenTabTiles = 200
)

// Navigation with held keys (W/A/S/D): rates per second, reached and left
// with an exponential ease of time constant NavEase seconds.
const (
	// NavPan is how many view widths a second A and D pan.
	NavPan = 2.5
	// NavZoom is the natural logarithm of how many times a second W and S
	// zoom (12×).
	NavZoom = 2.4849066497880004 // math.Log(12)
	// NavEase is the time constant, in seconds, of the ease in and out.
	NavEase = 0.06
	// NavShift multiplies the rates while Shift is held.
	NavShift = 3
)

// NavSpeed is a choice of the navigation speed setting: a multiplier of
// the rates above.
type NavSpeed struct {
	Name  string  `json:"name"`
	Times float64 `json:"times"`
}

// NavSpeeds are the choices of the navigation speed setting, slowest first.
var NavSpeeds = []NavSpeed{{"Slow", 0.5}, {"Normal", 1}, {"Fast", 2}, {"Very fast", 3}}

// ParseNavSpeed is the navigation speed a saved setting names: one of
// NavSpeeds' multipliers, or 1 (Normal) for anything else.
func ParseNavSpeed(v float64) float64 {
	for _, s := range NavSpeeds {
		if s.Times == v {
			return v
		}
	}
	return 1
}

func toneOf(k trace.Kind) string {
	switch k {
	case trace.KindKernel:
		return "kernel"
	case trace.KindRuntime:
		return "runtime"
	case trace.KindNVTX, trace.KindNVTXMark:
		return "nvtx"
	case trace.KindCPUOp:
		return "cpu"
	case trace.KindPython:
		return "python"
	}
	return "other"
}

// Hue is the hue, from 0 to 359, of the events named name: from the
// FNV-1a hash of the name, so a name has the same color everywhere.
func Hue(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % 360)
}

// Shade is which of a kind's shades (0, 1 or 2; kernels have three) the
// events named name are in: from the same hash as their hue, apart from it.
func Shade(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() / 360 % 3)
}

// toneName is the tone of events named name of kind k: a kernel's in one
// of three shades, so that kernels side by side differ in lightness too.
func toneName(name string, k trace.Kind) string {
	t := toneOf(k)
	if t == "kernel" {
		if s := Shade(name); s > 0 {
			t += "." + strconv.Itoa(s)
		}
	}
	return t
}

// memcpyColor is the color of a memcpy, by its direction.
func (p *Palette) memcpyColor(name string) string {
	switch {
	case strings.Contains(name, "HtoD"):
		return p.MemcpyHtoD
	case strings.Contains(name, "DtoH"):
		return p.MemcpyDtoH
	case strings.Contains(name, "DtoD"):
		return p.MemcpyDtoD
	case strings.Contains(name, "PtoP"):
		return p.MemcpyPtoP
	}
	return p.MemcpyOther
}

// hueColor is the sRGB color of a hue in a tone, as MyGo maps OKLCH into
// sRGB (reducing chroma for colors outside it, as CSS does).
func hueColor(t tone, hue int) ui.Color {
	return ui.Oklch(t.l, t.c, float32(hue)+0.5).SRGB()
}

// EventColor is the color of events named name of kind k.
func (p *Palette) EventColor(name string, k trace.Kind) ui.Color {
	switch k {
	case trace.KindMemcpy:
		return ui.Hex(p.memcpyColor(name))
	case trace.KindMemset:
		return ui.Hex(p.Memset)
	}
	return hueColor(p.tones[toneName(name, k)], Hue(name))
}

// colors is the palette for theme.js, by the names the browser uses.
func (p *Palette) colors() map[string]string {
	return map[string]string{
		"bg": p.Bg, "panel": p.Panel, "panelAlt": p.PanelAlt, "rowAlt": p.RowAlt, "border": p.Border,
		"text": p.Text, "muted": p.Muted, "faint": p.Faint,
		"accent": p.Accent, "accentHi": p.AccentHi, "accentPressed": p.AccentPressed, "accentText": p.AccentText,
		"hover": p.SurfaceHover, "pressed": p.SurfacePressed, "scrollbar": p.Scrollbar,
		"grid": p.Grid, "ruler": p.Ruler, "selection": p.Selection, "tooltip": p.Tooltip,
		"inverse": p.Inverse, "inverseText": p.InverseText, "onEvent": p.OnEvent,
		"range": p.Range, "warn": p.Warn, "shadow": p.Shadow,
		"swatchStream": p.SwatchStream, "swatchRuntime": p.SwatchRuntime, "swatchNVTX": p.SwatchNVTX,
		"swatchCPU": p.SwatchCPU, "swatchPython": p.SwatchPython,
		"memcpyHtoD": p.MemcpyHtoD, "memcpyDtoH": p.MemcpyDtoH, "memcpyDtoD": p.MemcpyDtoD,
		"memcpyPtoP": p.MemcpyPtoP, "memcpyOther": p.MemcpyOther, "memset": p.Memset,
	}
}

// hues is, for each tone, the sRGB color of every hue, flat: r, g, b.
func (p *Palette) hues() map[string][]int {
	out := map[string][]int{} // ints, not bytes, which JSON would write as base64
	for name, t := range p.tones {
		rgb := make([]int, 0, 360*3)
		for h := range 360 {
			c := hueColor(t, h)
			rgb = append(rgb, int(c.R), int(c.G), int(c.B))
		}
		out[name] = rgb
	}
	return out
}

// JS returns theme.js, which defines THEME for the browser client: both
// palettes (with, for each tone, the sRGB color of every hue, computed here
// so that the browser's colors are the desktop's to the bit), the metrics,
// the navigation rates and the kinds.
func JS() []byte {
	palettes := map[string]any{}
	for name, p := range Palettes {
		palettes[name] = map[string]any{"dark": p.Dark, "colors": p.colors(), "hues": p.hues()}
	}
	kinds := map[int]string{}
	kindTones := map[int]string{}
	for k := trace.KindKernel; k <= trace.LastKind; k++ {
		kinds[int(k)] = k.String()
		kindTones[int(k)] = toneOf(k)
	}
	v := map[string]any{
		"palettes": palettes,
		"metrics": map[string]float64{
			"rulerH": RulerH, "labelW": LabelW, "labelMinGutter": LabelMinGutter, "labelMaxGutter": LabelMaxGutter, "laneH": LaneH, "trackPad": TrackPad, "denseLaneH": DenseLaneH, "densePad": DensePad, "labelMinW": LabelMinW,
			"groupH": GroupH, "toggleH": ToggleH, "minScale": MinScale, "zoomPerNotch": ZoomPerNotch,
			"compactLaneH": CompactLaneH, "compactStripH": CompactStripH, "compactGap": CompactGap,
			"compactKindGap": CompactKindGap, "tabBarH": TabBarH, "tabMaxW": TabMaxW, "tabMinW": TabMinW, "maxTabs": MaxTabs, "hiddenTabTiles": HiddenTabTiles,
			"compactAuto": CompactAuto, "compactIdle": CompactIdle,
		},
		"compactLanes":   CompactLanes,
		"compactDefault": CompactDefault,
		"nav":            map[string]float64{"pan": NavPan, "zoom": NavZoom, "ease": NavEase, "shift": NavShift},
		"navSpeeds":      NavSpeeds,
		"kinds":          kinds,
		"kindTones":      kindTones,
		"kindIds": map[string]int{
			"kernel": int(trace.KindKernel), "memcpy": int(trace.KindMemcpy), "memset": int(trace.KindMemset),
			"api": int(trace.KindRuntime), "nvtx": int(trace.KindNVTX), "mark": int(trace.KindNVTXMark), "sync": int(trace.KindSync),
			"cpu": int(trace.KindCPUOp), "python": int(trace.KindPython),
		},
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return []byte(fmt.Sprintf("// Generated by internal/theme: the desktop app's look, for the browser.\n\"use strict\";\nconst THEME = %s;\n", b))
}
