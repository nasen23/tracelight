// Command rastercheck draws scenes of many rectangles (fractional edges,
// translucent colors, clips, and some rounded or bordered ones) with
// MyGo's software renderer, and prints a hash of their pixels: genwebui
// runs it with MyGo as it is and with our patches (third_party/mygo), and
// builds only if both draw the same pixels.
package main

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"

	"github.com/egoist/mygo/ui"
)

func main() {
	h := sha256.New()
	for seed := range uint64(6) {
		rng := rand.New(rand.NewPCG(seed, 7))
		view := func(c *ui.Context) {
			ui.Box(c).Fill().Background(ui.RGB(20, 24, 32)).Draw(func(p *ui.Painter, r ui.Rect) {
				f := func() float32 { return rng.Float32() }
				for i := range 1500 {
					x, y := f()*r.W, f()*r.H
					rect := ui.Rect{X: x, Y: y, W: 0.3 + f()*f()*120, H: 0.3 + f()*30}
					if i%3 == 0 { // whole pixels, as the timeline's columns
						rect = ui.Rect{X: float32(int(x)), Y: float32(int(y)), W: float32(1 + rng.IntN(4)), H: 17}
					}
					col := ui.RGB(uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256))).Alpha(0.15 + 0.85*f())
					switch {
					case i%97 == 0:
						p.Clip(ui.Rect{X: x, Y: y, W: 50 + f()*300, H: 20 + f()*100}, 0, func() {
							p.Fill(ui.Rect{X: x - 20, Y: y - 5, W: 200, H: 60}, col, 0)
						})
					case i%89 == 0:
						p.Clip(ui.Rect{X: x, Y: y, W: 150, H: 80}, 12, func() {
							p.Fill(ui.Rect{X: x - 10, Y: y - 10, W: 120, H: 120}, col, 0)
						})
					case i%53 == 0:
						p.Fill(rect, col, 4)
					default:
						p.Fill(rect, col, 0)
					}
				}
			})
		}
		tt := ui.NewTester(view, 800, 500)
		tt.Frame()
		img := tt.Image()
		h.Write(img.Pix)
	}
	fmt.Printf("%x\n", h.Sum(nil))
}
