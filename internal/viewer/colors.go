package viewer

import (
	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// The colors of the active palette (applyPalette), from package theme,
// which the browser client shares.
var (
	colBg, colPanel, colPanelAlt, colRowAlt, colBorder       ui.Color
	colText, colMuted, colFaint, colAccent, colAccentHi      ui.Color
	colGrid, colRuler, colSelection, colTooltip, colOnEvent  ui.Color
	colInverse, colInverseText, colRange, colWarn, colShadow ui.Color
	colSwatchStream, colSwatchRuntime, colSwatchNVTX         ui.Color
	colSwatchCPU, colSwatchPython                            ui.Color

	// active is the palette the colors are of.
	active *theme.Palette
)

func init() { applyPalette(theme.Dark) }

// applyPalette makes p the palette the app draws with. It reports whether
// that changed anything.
func applyPalette(p *theme.Palette) bool {
	if p == active {
		return false
	}
	active = p
	h := ui.Hex
	colBg, colPanel, colPanelAlt, colRowAlt, colBorder = h(p.Bg), h(p.Panel), h(p.PanelAlt), h(p.RowAlt), h(p.Border)
	colText, colMuted, colFaint, colAccent, colAccentHi = h(p.Text), h(p.Muted), h(p.Faint), h(p.Accent), h(p.AccentHi)
	colGrid, colRuler, colSelection, colTooltip, colOnEvent = h(p.Grid), h(p.Ruler), h(p.Selection), h(p.Tooltip), h(p.OnEvent)
	colInverse, colInverseText, colRange, colWarn, colShadow = h(p.Inverse), h(p.InverseText), h(p.Range), h(p.Warn), h(p.Shadow)
	colSwatchStream, colSwatchRuntime, colSwatchNVTX = h(p.SwatchStream), h(p.SwatchRuntime), h(p.SwatchNVTX)
	colSwatchCPU, colSwatchPython = h(p.SwatchCPU), h(p.SwatchPython)
	return true
}

// widgetTheme is the theme the widgets use, in the active palette.
func widgetTheme(base *ui.Theme) *ui.Theme {
	p := active
	t := *base
	t.Dark = p.Dark
	t.Background = colBg
	t.Surface = colPanelAlt
	t.SurfaceHover = ui.Hex(p.SurfaceHover)
	t.SurfacePressed = ui.Hex(p.SurfacePressed)
	t.Border = colBorder
	t.Text = colText
	t.TextMuted = colMuted
	t.Accent = colAccent
	t.AccentHover = colAccentHi
	t.AccentPressed = ui.Hex(p.AccentPressed)
	t.AccentText = ui.Hex(p.AccentText)
	t.Selection = colAccent.Alpha(0.35)
	t.Focus = colAccent.Alpha(0.7)
	t.Inverse = colTooltip
	t.InverseText = colText
	t.Scrollbar = ui.Hex(p.Scrollbar)
	t.Radius = 6
	t.FontSize = 13
	return &t
}

// palette caches a color per name and kind, in the active palette.
type palette struct {
	from   *theme.Palette // the palette the colors are of
	colors map[paletteKey]ui.Color
}

type paletteKey struct {
	name string
	kind trace.Kind
}

func newPalette() *palette {
	return &palette{colors: map[paletteKey]ui.Color{}}
}

// of returns the color of events named name of kind k: a hue from the
// name's hash, at a lightness and chroma that tell kinds apart.
func (p *palette) of(name string, k trace.Kind) ui.Color {
	if p.from != active { // the appearance changed
		clear(p.colors)
		p.from = active
	}
	key := paletteKey{name, k}
	if c, ok := p.colors[key]; ok {
		return c
	}
	if len(p.colors) > 50_000 {
		clear(p.colors)
	}
	c := active.EventColor(name, k)
	p.colors[key] = c
	return c
}
