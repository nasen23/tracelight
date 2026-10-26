# Design

## One renderer or two?

Tracelight draws its timeline twice: the desktop app with MyGo's native,
GPU-drawn `ui` package (`internal/viewer`), and the browser client of
`tracelight serve` with a 2D canvas (`internal/server/web`).

**Decision (October 2026): keep both renderers, and make everything they
share come from one place.** The model, the level-of-detail blocks and the
look are single-sourced in Go. The renderers are thin, and a test checks
that they agree.

### Options weighed

| | Desktop quality | Desktop speed | Browser | Maintenance |
|---|---|---|---|---|
| **A. Two renderers, shared core** (chosen) | native: GPU (Metal, OpenGL) or MyGo's CPU fallback, system fonts, accessibility, no webview | 1.6 ms p50, 3.5 ms p95 per frame on 1.2M events, in memory | 50 KB of JS, streams 0.5–100 KB tiles, first paint at once | two drawing layers (~990 Go and ~1,100 JS lines); kept in step by `internal/theme` and a conformance test |
| B. Desktop shows the web UI in MyGo's webview | a page in WebKitGTK, WKWebView or WebView2 | every frame through local HTTP and JSON tiles; the canvas client measured 2.1 ms p50, **8.8 ms p95** on the same trace | the same client | one drawing layer, but the desktop depends on a system webview: WebKitGTK is not even installed on the Linux test machine, where the native app runs today (falling back to the CPU renderer under Xvfb). It adds the webview's processes and memory and its cold start |
| C. One Go renderer, compiled to WebAssembly for browsers (MyGo's `ui` runs in browsers with WebGL 2) | unchanged | unchanged, unless the viewer is made to stream data | **12.2 MB of WASM (3.3 MB gzipped)**, measured for the viewer, which also needed its SQLite reader built out (modernc's SQLite does not build for `js/wasm`). The viewer holds the whole model in memory, so the browser would download the full snapshot (10.6 MB gzipped for the 1.2M-event trace) and keep a ~90 MB Go heap, giving up tile streaming. Keeping streaming would mean rebuilding the desktop timeline around asynchronous, missing data | one renderer, but the riskiest change for the desktop, which must stay smooth |

### Why A

- **Quality and speed where they matter.** The desktop is the app for big
  traces: it renders from memory with no serialization, on the GPU, with
  frames well under 16 ms. B would put a browser engine and a network
  protocol under every frame; C would either bloat the browser client or
  rework the desktop's data path.
- **The browser client stays small.** It exists for zero-install access to
  a remote machine. Tiles keep it fast over a network and its first paint
  immediate, which a 3 MB WASM download plus a 10 MB snapshot would not.
- **Duplication is contained to drawing.** Parsing, the model, LOD,
  summaries and correlation exist once, in Go: the server computes the
  browser's tiles with the same `Lane.LOD` the desktop paints with. What
  remained duplicated, and had already started to drift, was
  presentation: the palette, the metrics, number formatting, and the
  colors of events (the JS clamped OKLCH where MyGo gamut-maps it, so some
  hues differed).

### What keeps the two in step

- `internal/theme` is the one source of the look: both palettes (dark and
  light), the metrics, the held-key navigation rates, kind ids, and the
  color of every event. The desktop draws with it; the server
  generates `theme.js` from it, including each tone's color for all 360
  hues, computed by MyGo's own OKLCH-to-sRGB mapping, so browser colors
  equal the desktop's exactly.
- `web/shared.js` holds the client's pure logic (formatting, ruler ticks,
  name hashing, event colors) with no DOM. `TestBrowserClientMatchesDesktop`
  (in `internal/viewer`) runs it in Node and compares it with the desktop's
  Go for durations, times, ticks, sizes, 1,498 name-and-kind colors in each
  palette, and a 240-frame held-key navigation session (the same easing
  model drives both apps' continuous W/A/S/D).
  Mutation checks: breaking a unit string or the hue lookup fails it.
- `trace.OpenWith` is the only way files become traces, for both (see the
  server section of the README), and the server's tests keep it free of
  format code.

### When to revisit

Choose C (one Go renderer) if MyGo's browser target becomes cheap enough to
download (or is needed anyway), and the viewer gains a tile-backed data
source that the desktop can use without a frame-time cost: then the JS
client can go. Choose B only if the desktop needs web-only features that
outweigh the webview dependency.

### Measured after the change

`tracelight bench` on a real-world export of 1.2M events (not distributed), medians of
three runs, against [BENCHMARKS.md](BENCHMARKS.md):

| | BENCHMARKS.md | Now |
|---|---:|---:|
| Load | 408 ms | 425 ms |
| Open to first pixels | 525 ms | 533 ms |
| Frames, build+paint p50 / p95 / p99 | 1.6 / 3.5 / 5.2 ms | 1.7 / 3.5 / 5.4 ms |
| Frames over 16 ms (build+paint) | 0 | 0 |
| Peak RSS | 275 MB | 282 MB |
| Live Go heap | 91 MB | 91 MB |

The differences are within run-to-run noise on this shared machine.
