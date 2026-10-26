# Patches to MyGo

`cmd/genwebui` builds the web client (`cmd/webui`) against a copy of the
MyGo module with these files replaced by ours (a `go.mod` of its own,
`go build -modfile`; `-overlay` cannot replace files of the module cache).
They apply to the web client only; the desktop app builds with MyGo as it
is.

| File of MyGo v0.5.5 | Ours | Why |
|---|---|---|
| `internal/web/surface.go` | `internal/web/surface.go.overlay` | Without WebGL 2, frames drawn in memory are shown by what changed only (`PresentDamage`, which MyGo's renderer already reports), converted to the canvas's RGBA two pixels a word. The original converted (byte by byte) and copied the whole frame every frame: 57% of a frame's time while the timeline panned. |
| `internal/raster/raster.go` | `internal/raster/raster.go.overlay` | A plain rectangle (no corners, no border, one color) under plain clips is filled without working out its shape and its clips' on each row (`fillRect`). A timeline draws thousands of rectangles a pixel or two wide each frame; in WebAssembly, those per-row sums (and `max` of floats, a function call there) cost more than the pixels. |

`genwebui` checks that the module is the version these were made from and
that each original is unchanged (its SHA-256), and that the patched
renderer draws the very pixels MyGo's does (`cmd/genwebui/rastercheck`:
scenes of thousands of rectangles, translucent, at fractional edges,
under clips, some rounded). It refuses to build otherwise, so a MyGo upgrade means porting the patch, or dropping it if
MyGo does the same. MyGo is MIT licensed; see `LICENSE`.
