// Command genicon renders the app icon from its SVG sources into the PNGs
// and the macOS icon set the app ships, with MyGo's own SVG renderer: no
// rsvg, Inkscape or iconutil needed.
//
//	go run ./cmd/genicon          # from the repository root
//
// It writes, in assets/:
//
//	icon-1024.png             the icon MyGo builds the app's AppIcon.icns from (mygo.json)
//	tracelight.iconset/       every size of a macOS icon set, for iconutil
//	tracelight.icns           the same as an .icns
//
// and, in internal/server/web/ (embedded in the server), the web client's
// favicon.svg (the simplified drawing), favicon-32.png, and
// apple-touch-icon.png: full bleed, as iOS rounds the corners itself.
//
// Sizes of 64 pixels and under come from icon-small.svg, a simplified
// drawing that stays legible at 16 and 32 points. The .icns leaves out the
// icp4 and icp5 entries (16 and 32 pixels), as MyGo's does: macOS draws
// PNGs in them as noise, and scales those sizes from ic11 and ic12.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"

	"github.com/egoist/mygo/ui"
)

func main() {
	dir := flag.String("dir", "assets", "directory of icon.svg and icon-small.svg, and of the output")
	web := flag.String("web", filepath.Join("internal", "server", "web"), "the web client's directory, for its favicons")
	flag.Parse()
	largeSrc := read(filepath.Join(*dir, "icon.svg"))
	smallSrc := read(filepath.Join(*dir, "icon-small.svg"))
	large, small := mustSVG(largeSrc), mustSVG(smallSrc)
	render := func(px int) []byte {
		src := large
		if px <= 64 {
			src = small
		}
		return encode(renderSVG(src, px))
	}

	write(filepath.Join(*dir, "icon-1024.png"), render(1024))

	// The web client's icons.
	write(filepath.Join(*web, "favicon.svg"), smallSrc)
	write(filepath.Join(*web, "favicon-32.png"), render(32))
	// Full bleed for iOS: the rounded square's inside, edge to edge.
	bleed := bytes.Replace(largeSrc, []byte(`viewBox="0 0 1024 1024"`), []byte(`viewBox="100 100 824 824"`), 1)
	bleed = bytes.Replace(bleed, []byte("</defs>"), []byte(`</defs><rect x="0" y="0" width="1024" height="1024" fill="url(#body)"/>`), 1)
	write(filepath.Join(*web, "apple-touch-icon.png"), encode(renderSVG(mustSVG(bleed), 180)))

	// The icon set: icon_<n>x<n>[@2x].png for n of 16 to 512.
	set := filepath.Join(*dir, "tracelight.iconset")
	if err := os.MkdirAll(set, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, n := range []int{16, 32, 128, 256, 512} {
		write(filepath.Join(set, fmt.Sprintf("icon_%dx%d.png", n, n)), render(n))
		write(filepath.Join(set, fmt.Sprintf("icon_%dx%d@2x.png", n, n)), render(2*n))
	}

	// The .icns: a header, then each entry's type, length and PNG.
	entries := []struct {
		kind string
		px   int
	}{
		{"ic11", 32}, {"ic12", 64}, {"ic07", 128}, {"ic13", 256}, {"ic08", 256},
		{"ic14", 512}, {"ic09", 512}, {"ic10", 1024},
	}
	var body bytes.Buffer
	for _, e := range entries {
		p := render(e.px)
		body.WriteString(e.kind)
		binary.Write(&body, binary.BigEndian, uint32(len(p)+8))
		body.Write(p)
	}
	var icns bytes.Buffer
	icns.WriteString("icns")
	binary.Write(&icns, binary.BigEndian, uint32(body.Len()+8))
	icns.Write(body.Bytes())
	write(filepath.Join(*dir, "tracelight.icns"), icns.Bytes())
}

func read(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	return data
}

func mustSVG(data []byte) *ui.SVG {
	s, err := ui.ParseSVG(data)
	if err != nil {
		log.Fatal(err)
	}
	return s
}

// renderSVG draws s into a transparent square of px pixels.
func renderSVG(s *ui.SVG, px int) image.Image {
	return ui.Render(func(c *ui.Context) {
		t := *c.Theme()
		t.Background = ui.Color{} // transparent around the rounded square
		c.SetTheme(&t)
		ui.Box(c).Fill().Draw(func(p *ui.Painter, r ui.Rect) {
			p.Image(s, r, ui.Contain)
		})
	}, px, px, 1)
}

func encode(img image.Image) []byte {
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
}
