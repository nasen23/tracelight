# Benchmarks

Measured on October 19, 2026, on an 8-core Intel Xeon VM (Linux, no GPU),
Go 1.27.2, `CGO_ENABLED=0`. Numbers are the median of three runs unless a
range is given; the machine is shared, so differences under about 10% are
noise.

```sh
go build -o tracelight .
./tracelight bench trace.sqlite                      # any nsys SQLite export
./tracelight bench -loadprofile load.pb -frameprofile frames.pb trace.sqlite   # pprof profiles
```

`bench` loads the trace, builds the viewer, renders the first frame
headlessly (MyGo's `ui.Tester`, on the CPU), then replays 852 scripted
frames: from the whole trace down to a 2 µs window around the densest
stretch of GPU work, in steps of 1.6×, with eight pans and three vertical
scrolls at each level and the pointer over the timeline (tooltip hit
testing included), and back out. "build+paint" is MyGo building, laying out
and painting the frame's display list; "+ CPU raster" adds rasterizing it
into pixels on the CPU, which a window without a GPU (as under Xvfb) pays
and a GPU-backed window does not.

## The traces

**Real trace** (a real-world export, not distributed): a 96 MB export
of a 27 s run.

| Kind | Events |
|---|---:|
| Kernels | 96,178 |
| Memcpy | 1,349 |
| Memset | 14,525 |
| CUDA API calls | 536,259 |
| Synchronizations | 95,320 (read since this round; before, not loaded) |
| NVTX ranges | 455,007 |
| **Total** | **1,198,638** (1,103,318 before, without the synchronizations) |

36 rows. The export lists 8 GPUs, of which one ran all the GPU work.

**Synthetic sample** (`go run ./cmd/gensample`): 198,920 events, 12 rows.

## Results: real trace

| | Before | After | |
|---|---:|---:|---|
| SQLite read + decode | 1960 ms | 260 ms | 7.5× |
| ↳ StringIds² | 308 ms | 3 ms | only the ids events use |
| ↳ NVTX_EVENTS (455k rows)² | 988 ms | 100 ms | 7 chunks in parallel |
| ↳ RUNTIME (536k rows)² | 577 ms | 117 ms | 9 chunks in parallel |
| Merge shards | — | 68 ms | new step |
| Model build (sort, pack into lanes) | 155 ms | 73 ms | and 95k more events |
| **Load total** | **2113 ms** | **408 ms** | **5.2×** |
| Viewer setup (summaries) | 263 ms | 18 ms | 15× |
| First frame (build+paint) | 123 ms | 92 ms | see below |
| **Open to first pixels** | **2489 ms** | **525 ms** | **4.7×** |
| Frames, build+paint p50 / p95 / p99 / max | 1.7 / 3.5 / 5.0 / 8.1 ms | 1.6 / 3.5 / 5.2 / 8.2 ms | over 16 ms: 0–1 of 852 before, 0 after |
| Frames, + CPU raster p50 / p95 / p99 / max | 5.0 / 7.5 / 9.4 / 13.5 ms | 5.0 / 7.3 / 9.4 / 14.7 ms | over 16 ms: 0–2 before, 0–1 after |
| Allocations per frame (build+paint) | ~36 KB¹ | 41 KB, 972 objects | |
| **Peak RSS** | **412 MB** (403–419) | **275 MB** (273–285) | −33% |
| Peak Go heap (sampled every 2 ms) | 275 MB | 199 MB | |
| Live Go heap, trace and view loaded | 101 MB | 91 MB | with 95k more events |

¹ The first version of `bench` reported 6.3 MB per frame; 92% of that was
`Tester.Image()` copying the 1600×1000 frame for the benchmark itself. The
allocation profile put the app's share at about 36 KB; `bench` now counts
build+paint only.

² Per-table times are from single runs; after the change, tables are read
at the same time, so they overlap and do not add up.

### In the real window

The same kind of sequence, driven through the real window under Xvfb with
`xdotool` (40 notches of Ctrl+wheel zoom in from the whole trace with a drag
pan every ten, keyboard pans, 40 notches back out), with MyGo's own frame
log (`MYGO_FRAME_STATS=all`). Without a GPU, MyGo draws in memory on the CPU
and presents through GTK:

| 949 frames | p50 | p95 | p99 | max | over 16 ms |
|---|---:|---:|---:|---:|---:|
| whole frame (build, layout, paint, present) | 1.9 ms | 3.3 ms | 5.1 ms | 107 ms | 2 |

The two frames over 16 ms: the first frame showing the trace (107 ms, below)
and one 17 ms frame on first reaching a new zoom depth. The paint-time
overlay (press **P** in the app) showed the timeline's own painting at
p50 0.32 ms, p95 0.42 ms.

## Results: synthetic sample

Mean of two runs.

| | Before | After |
|---|---:|---:|
| Load total | 369 ms | 81 ms |
| Open to first pixels | 496 ms | 165 ms |
| Frames, build+paint p50 / p95 / max | 1.5 / 2.6 / 8.0–9.4 ms | 1.5 / 2.6 / 7.6–7.8 ms |
| Peak RSS | 122 MB | 118 MB |

## What changed, and why

Profiling the load (`-loadprofile`) showed the time going to per-row
overhead rather than to SQLite's own work:

1. **No database/sql on the hot path** (`internal/trace/fastsql.go`). modernc's
   driver opens every connection with `SQLITE_OPEN_FULLMUTEX`, so each
   column read took and released a mutex: 23% of the load's CPU time.
   database/sql then boxed every value in an interface and converted it
   again in `Scan`. The loader now calls modernc's translation of the
   SQLite C API directly, read-only and `SQLITE_OPEN_NOMUTEX`, reading
   columns as plain integers and NVTX text as bytes it looks up without
   copying.
2. **Parallel reads.** Event tables are split into ranges of 64k rowids and
   read by up to 8 connections at once, each range into a shard of its own,
   merged in row order so that tracks stay sorted.
3. **Only the strings in use.** The loader read all 379k StringIds into a
   map (308 ms); now it collects the ids that events and thread names use
   and reads just those.
4. **Cheaper per-row bookkeeping.** The track of the previous row is
   cached (most rows go to the same one), counts go in an array instead of
   a map, and StringIds ids map to names through a slice.
5. **Faster packing.** Tracks are packed in parallel, largest first. A track
   that arrives sorted is not sorted again, and a sorted single-lane track
   keeps its slice as its lane. Tracks that need sorting (NVTX ranges,
   which exports list by end time, and API calls with their
   synchronizations) use a stable LSD radix sort of an index array by
   (start, end descending), several times faster than a comparison sort of
   the 32-byte events, and are copied once, into their lanes.
6. **Summaries in a few milliseconds.** `TopByTotal` read every track and
   built and sorted a stat for every name, and the real trace has about
   400k distinct NVTX names, most of them embedding a sequence number. It
   now reads only the tracks that can hold the kinds asked for, sums into a
   slice indexed by name, and keeps the top n with a heap.
7. **Correlation index off the load path.** The maps from correlation ids
   to API calls and GPU work (650k entries) are sorted slices built in the
   background after loading, and on demand if a selection comes first.
8. **Peak memory.** Each shard's first track is sized for its chunk instead
   of growing by doubling. The heap is collected once right after merging,
   when the shards' slices have just become garbage and before packing
   allocates lanes. Events hold no pointers, so this costs little. Without
   it, peak RSS ranged from 307 to 446 MB depending on when the collector
   ran.

The frames needed no work: at most one of 852 was over 16 ms before
(build+paint), none after. Level of
detail is computed per pixel column (`Lane.LOD`), so a frame's cost
follows the window's width, not the number of events; the timeline's own
painting is under half a millisecond of the 1.6 ms median.

## What is left

- **The first frame of a trace takes about 90 ms** (107 ms in the window
  under Xvfb). Profiles put most of it in MyGo's text layout on Linux:
  about 290 new strings (row names, ruler labels, the sidebar), with
  fontconfig lookups. Laying out the view's symbols while the trace loads
  did not help (the cost is per string, not per glyph). It happens once,
  after the loading screen. On macOS, MyGo's text goes through Core Text
  instead; not measured.
- **Peak heap is about twice the live heap** during the parallel read, as
  the default `GOGC=100` lets it grow. `GOGC=50` brought peak RSS to 292 MB
  before the explicit collection was added; it is not set by default.
- **CPU rasterization** is 3–4 ms of a frame on a 1600×1000 window without a
  GPU. With a GPU (Metal on macOS, OpenGL on Linux) that work moves off the
  CPU; not measured here, as this machine has none.

## Re-run after unifying the theme (DESIGN.md)

Same machine and command, medians of three runs: load 425 ms, open to first
pixels 533 ms, frames build+paint p50 1.7 / p95 3.5 / p99 5.4 ms (0 of 852
over 16 ms), + CPU raster p50 5.2 / p95 7.8 ms, 41 KB allocated per frame,
peak RSS 282 MB, live Go heap 91 MB. Within noise of the numbers above.

## Re-run after light/dark themes and continuous key navigation

The machine was busier this time (load average about 5 on 8 cores, from an
unrelated process), so the new build was compared with the one before it,
alternating, three runs each (build+paint, medians):

| | Before | After |
|---|---:|---:|
| Real trace, frames p50 / p95 / max | 2.2 / 4.4 / 8.6 ms | 2.2 / 4.7 / 9.4 ms |
| Real trace, load | 488 ms | 479 ms |
| Synthetic sample, frames p50 / p95 / max | 1.4 / 2.5 / 6.0 ms | 1.5 / 2.6 / 7.4 ms |
| Frames over 16 ms | 0 | 0 |
| Allocations per frame (real trace) | 971, 40.5 KB | 978, 40.8 KB |

No regression beyond noise; the synthetic sample matches the numbers above.

## Eight traces side by side (web client)

A session of 8 synthetic ranks (`go run ./cmd/gensample -ranks 8 -steps 100`:
740,544 events in all, 4.9–5.5 MB each), opened from a cold server through
the API as the web client does (the listing, then every trace's metadata at
once, then a screen of tiles per trace):

| | 8 ranks, 30 steps (183k events) | 8 ranks, 100 steps (741k events) |
|---|---:|---:|
| Session listing | 14 ms | 15 ms |
| All 8 parsed (4 at a time), wall | 61 ms | 167 ms |
| Parse per trace, min / median / max | 16 / 26 / 36 ms | 60 / 74 / 88 ms |
| First screen of tiles, all traces | 18 tiles, 12 ms | 16 tiles, 15 ms |

In headless Chrome (scripts/webcheck.mjs, the 741k-event session): draw p50
1.7 ms, p95 3.4 ms; tile round trip p50 5 ms, p95 21 ms. Server peak RSS
with all 8 loaded: 158 MB. Single-trace numbers are unchanged.

## Summaries over a measured range (server)

`/api/summary` on the real trace (1,198,638 events), through the server, 5
random ranges for each kind (kernel, CUDA API, NVTX) at each width:

| Range | Uncached median / max | Cached median |
|---|---:|---:|
| none (whole trace) | 0.7 / 10.3 ms* | 0.7 ms |
| 100% of the trace | 0.6 / 20.5 ms* | 0.6 ms |
| 50% | 5.6 / 13.9 ms | 0.7 ms |
| 10% | 4.3 / 13.7 ms | 0.9 ms |
| 1% | 3.4 / 4.5 ms | 0.7 ms |
| 0.1% | 2.9 / 4.2 ms | 0.7 ms |

(*Five identical requests per kind: only the first is uncached, which
the max shows.) A narrow range costs about 3 ms, mostly the per-name
accumulator and the response; the whole trace stays under ~20 ms.

## The desktop app streaming from a server

`tracelight bench -remote -rtt 30ms-80ms trace` serves the trace's folder
in-process, delays every request by a round trip drawn from 30–80 ms, and
drives the window headlessly as when connected (1600×1000). The real trace
(1,198,638 events, 36 rows):

| | Cold server (parses it) | Warm server |
|---|---:|---:|
| Connect (`/api/info`) | 90 ms | 80 ms |
| Open to the rows (`/api/session`, `/api/trace`) | 600 ms | 144 ms |
| Open to the first pixels of events | **631 ms** | **193 ms** |
| Open to every tile of the view | 689 ms | 228 ms |

Then 41 views (4× zoom steps from the whole 27 s down to 2 µs and back,
half-view pans at each level) at 60 frames a second while tiles stream:
132 frames, build+paint p50 1.0 ms, p95 2.8 ms, p99 3.6 ms, max 7.4 ms (0
over 16 ms); a new view is drawn at full detail after a median 17 ms (p95
85 ms), coarser tiles standing in meanwhile; 136 requests in all. The
synthetic sample (198,920 events): first pixels 290 ms cold, 179 ms warm;
frames p50 1.3 ms, p95 3.5 ms. Before, the desktop app downloaded the whole
parsed trace first (a 10.6 MB gzipped snapshot for the real trace), which
is now Download for offline.

## The compact view: 8 and 16 ranks side by side

Synthetic jobs from `go run ./cmd/gensample -ranks 8` (183,288 events, 56
rows) and `-ranks 16 -o samples/ranks16` (386,024 events, 112 rows), every
summary lane shown. Desktop: `tracelight bench samples/ranks16` opens the
folder as the path box does and replays 68 frames (4× zoom steps from the
whole session down to 2 µs and back, half-view pans, scrolls, twice),
1600×1000; build+paint, and with MyGo's CPU rasterizer (what a window
without a GPU pays):

| | 8 ranks compact | 8 ranks full | 16 ranks compact | 16 ranks full |
|---|---:|---:|---:|---:|
| Blocks drawn a frame | 1,371 | 434 | 2,048 | 367 |
| build+paint p50 / p95 | 2.8 / 5.0 ms | 2.2 / 3.8 ms | 3.4 / 8.6 ms | 2.8 / 4.6 ms |
| + CPU raster p50 / p95 | 6.2 / 9.9 ms | 5.7 / 7.9 ms | 7.6 / 13.3 ms | 7.0 / 10.8 ms |

The compact view draws every trace at once (the full view only the rows on
screen), hence more blocks; a profile of the 16-rank compact frames puts
most of the time in the CPU rasterizer, the LOD at about 5% and the busy
columns at about 3%. One frame of 68 went over 16 ms at 16 ranks (21.7 ms,
CPU raster, while zoomed out).

Web: `scripts/webbench.mjs` in headless Chrome, served locally, 600 frames
(`draw()`, 20 a view over 30 views of the same zoom script, every view's
tiles in), 16 ranks: compact p50 0.9 ms, p95 4.6 ms, max 8.3 ms; full p50
0.6 ms, p95 1.9 ms. 8 ranks: compact p50 0.7 ms, p95 2.7 ms; full p50
0.6 ms, p95 2.3 ms. Drawing the compact lanes' thousands of small blocks
one path per color (rather than a fill each), and the strip's colors made
once per palette, took the compact p95 from 30 ms to 4.6 ms. The webcheck
run on the same 16 ranks (interactions included) reports draw p50 2.7 ms,
p95 6.4 ms; on 8 ranks, p50 1.6 ms, p95 4.9 ms.

## The rank overview (0.3.2): 8 and 16 ranks

0.3.2 makes the multi-rank view readable: lanes as tall as the full
view's, with event names, GPU and NVTX lanes by default. Same synthetic jobs
and scripts as above (`tracelight bench samples/ranks16`: 68 frames,
1600×1000; desktop first, then the web client):

| | 8 ranks overview | 8 ranks full | 16 ranks overview | 16 ranks full |
|---|---:|---:|---:|---:|
| Desktop blocks drawn a frame | 1,078 | 434 | 1,507 | 367 |
| Desktop build+paint p50 / p95 | 2.9 / 7.3 ms | 2.0 / 3.8 ms | 4.1 / 9.2 ms | 2.3 / 4.2 ms |
| Desktop + CPU raster p50 / p95 | 6.1 / 12.2 ms | 5.2 / 8.5 ms | 7.3 / 12.8 ms | 5.6 / 8.3 ms |
| Web draw+raster p50 / p95 (max) | 1.9 / 8.9 (12.0) ms | 1.6 / 4.1 ms | 3.6 / 12.0 (16.2) ms | 1.6 / 3.8 ms |

Desktop: one frame of 68 over 16 ms at 16 ranks (20 ms build+paint). Web:
`scripts/webbench.mjs` now reads a pixel back after each frame, so that a
frame's time includes the canvas's rasterization (software, in headless
Chrome): drawn back to back without it, Chrome defers that work and pays
it every twenty frames or so, which made a ~115 ms "frame" appear in the
loop and the other frames look cheaper than they are (the numbers of the
0.3.1 section above are draw() alone). The overview queues its merged and
narrow blocks by color, draws the wide ones with their names on top, and
reuses its buffers from frame to frame.

## Multi-rank alignment and the overview's streaming (0.3.6)

Four real traces of a four-node training job (one H200 GPU each, 1.2–1.3M
events and about 24 s each, reports read natively; not distributed), and the
same four four times as 16 ranks.

**Alignment.** The 219 all-reduce kernels seen by all four ranks should end
together. How far apart their ends are under each alignment:

| | median | p90 | max |
|---|---:|---:|---:|
| Starts (what 0.3.5 did for reports without exports) | 487.8 ms | 487.8 ms | 487.9 ms |
| Clocks (the reports' wall clocks, now read) | 2.56 ms | 2.57 ms | 2.66 ms |
| Collectives (the new default) | 6.9 µs | 56.9 µs | 172.7 µs |

The hosts' clocks were 0.3 to 2.3 ms apart. Estimated with no clock at all,
the collectives give the same offsets to the nanosecond.

**Streaming.** `tracelight serve --latency 30ms-80ms` on the traces; the
web client measured by `scripts/overviewbench.mjs` (headless Chrome,
software rendering), the desktop app by `tracelight bench -remote
-overview -rtt 30ms-80ms`; both through 29 views (4× zoom steps down to
1/256 of the trace, half-view pans and jumps to places not seen yet, then
zooms out from deep at another place), each framed until every tile slot
of every trace drawn is of the view's level. Neither ever showed a blank
slot, before or after (coarser tiles stand in); what changed is how long
the coarser tiles stay, and the frames:

| | before | after |
|---|---:|---:|
| Web, 4 ranks, overview: full refill p50 / p95 | 138 / 266 ms | 81 / 115 ms |
| Web, 4 ranks: draw p95 | 10 ms | 7 ms |
| Web, 16 ranks: full refill p50 / p95 | 370 / 1,100 ms | 97 / 160 ms |
| Web, 16 ranks: draw p95 | 23 ms | 16 ms |
| Desktop, 4 ranks: full refill p50 / p95 | 76 / 114 ms | 85 / 103 ms |
| Desktop, 4 ranks: build+paint p95 (CPU raster) | 21.4 ms | 6.3 ms |
| Desktop, 4 ranks: requests | 505 | 94 |
| Desktop, 16 ranks: full refill p50 / p95 | 94 / 145 ms | 106 / 144 ms |
| Desktop, 16 ranks: build+paint p95 | 26.0 ms | 13.9 ms |
| Desktop, 16 ranks: requests | 2,020 | 205 |

What did it: tiles asked for in batches of several traces (`/api/tiles`,
16 a request, 8 at once) from what each frame wants (nothing stale queued;
traces off screen ask for nothing), a coarse tile first over any slot
with nothing to show, finer stand-ins two levels down for zooms out, the
overview's tiles holding only its lanes (about 60% smaller), a server-side
cache of encoded tiles, and summary lanes drawn as a column a pixel (the
strongest of every row there) instead of one rectangle a block: in the
browser as a picture one pixel high, stretched, where thousands of
rectangles went to the canvas before. The desktop app had refilled fast
by asking for 6 tiles at a time per trace (96 at once for 16), which the
batches now match with a tenth of the requests.

## Room for parallel work in the overview (0.3.7)

The overview's summary lanes became a few sub-lanes each (streams whose
work overlaps apart, NVTX by depth). On the synthetic 8 and 16 ranks
(`go run ./cmd/gensample -ranks 8` / `-ranks 16`), 1600×1000, before
and after:

| | 0.3.6 | 0.3.7 |
|---|---:|---:|
| Desktop, 8 ranks local: build+paint p95 / + CPU raster p95 | 5.4 / 9.5 ms | 4.6 / 8.7 ms |
| Desktop, 16 ranks local: build+paint p95 / + CPU raster p95 | 8.2 / 12.2 ms | 6.2 / 10.4 ms |
| Desktop, 8 ranks, server at 30–80 ms: full refill p50 / p95 | 79 / 120 ms | 84 / 118 ms |
| Desktop, 16 ranks, server at 30–80 ms: full refill p50 / p95 | 87 / 123 ms | 72 / 115 ms |
| Desktop, 16 ranks, server: requests | 191 | 109 |

Taller ranks put fewer of them on screen, so a frame draws fewer blocks
(16 ranks: 1,508 → 773 a frame) though each rank draws more lanes; with
only a few ranks, all on screen, a frame draws more lanes than before and
costs more (about 1.5× in the web client). Working out the sub-lanes takes
about 35 ms per trace of a million events, once, when the server (or the
desktop app) loads it.

## NVTX and CUDA API in bands (0.3.8)

0.3.7 packed every NVTX and CUDA API depth like a stream, so one thread's
ranges went in among another's and a few percent of them were drawn over
others. Each thread now gets a band of sub-lanes. On the synthetic ranks
the data loader's ranges move from the training loop's third depth to a
band of their own. Frame times stay level (desktop, local, build+paint
p95: 8 ranks 4.5 → 4.0 ms, 16 ranks 4.9 → 5.0 ms), and so does streaming
from a server at 30–80 ms.

## The web client in WebAssembly (0.4.0)

`scripts/clientbench.mjs` drives both web clients the same way in
headless Chrome (1600×1000, no GPU). It measures the start (navigation to
the first events drawn, and the bytes downloaded), the 29 views of
`overviewbench.mjs` (time until every tile slot drawn is of the view's
level), and 2 s of held D (panning). Frame work is the frame's
requestAnimationFrame callbacks, summed, in both clients. These are the 16
synthetic ranks in the rank overview, served with `--latency 30ms-80ms`:

| | Classic (JavaScript) | App UI, WebGL (SwiftShader) | App UI, no WebGL (CPU) |
|---|---:|---:|---:|
| Start to first events | 0.7 s | 5.1 s | 1.3 s |
| Downloaded | 0.24 MB | 5.2 MB | 5.2 MB |
| Full refill p50 / p95 | 82 / 132 ms | 1,851 / 2,282 ms | 161 / 236 ms |
| Held D: frames a second | 60 | 0.8 | 18.5 |
| Frame work p95 (panning) | 2.5 ms | 17.8 ms | 68 ms |

Headless Chrome has no GPU. Its WebGL runs on SwiftShader in the GPU
process, and the App UI's frames wait for it: most of the time is the
browser's own, not the app's. Without WebGL, MyGo draws each 1600×1000
frame on the CPU in WebAssembly, in about 50 ms. The App UI's refill is
bound by those frames, not by the network. With a hardware GPU the WebGL
path should do much better; its CPU-side work is about 15 ms a frame here.
That has not been measured on real GPUs yet, which is why the classic
client stays the default.

## Why the App UI was slow (0.4.1)

These are Chrome CPU profiles of the App UI without WebGL, panning (held D)
16 synthetic ranks zoomed to 5% of the trace, 1600×1000 (the first row
from the fitted view, profiled before any fix). In 0.4.0 a frame took
42–50 ms:

| | 0.4.0 | 0.4.1 |
|---|---:|---:|
| Showing the frame: RGBA conversion, copy, putImageData | 71% of frame time (profiled in the fitted view) | 3.2 ms |
| Rasterizing (MyGo's software renderer) | 23.8 ms | 12.5 ms |
| Painting the timeline into operations (ours) | 4.4 ms | 4.0 ms |
| Layout and the view | 2.8 ms | 2.5 ms |
| A frame, p50 | 42.9 ms | 23 ms |

1. **Showing the frame.** MyGo's browser backend converted every pixel
   of the whole window from premultiplied BGRA to straight RGBA, a byte
   at a time, then copied it all to JavaScript and put it on the canvas,
   every frame. Its renderer already reports what changed (the Linux
   backend uses that), so the patched backend converts, copies and puts
   only those rectangles, two pixels to a 64-bit word.
2. **Rasterizing.** The cost was per operation, not per pixel. Each row of
   each of the timeline's thousands of 1–2 pixel rectangles redid the
   rectangle's and its clips' shape math. That includes `max` on floats,
   which is a function call in WebAssembly. Plain rectangles under plain
   clips now do that once. The patched renderer is checked to draw the
   very same pixels.
3. **Tried and dropped:** drawing each summary lane's columns as a
   one-pixel-high bitmap stretched to the lane. That is one operation
   instead of thousands, but MyGo's CPU renderer samples images pixel by
   pixel in floats, which was three times slower.

`scripts/clientbench.mjs`, 16 synthetic ranks in the rank overview, `--latency
30ms-80ms`, headless Chrome without a GPU (the App UI drawing on the CPU):

| | Classic | App UI 0.4.0 | App UI 0.4.1 |
|---|---:|---:|---:|
| Full refill p50 / p95 | 98 / 131 ms | 141 / 243 ms | 126–132 / 165–177 ms |
| Held D: frames a second | 60 | 21.5 | 26–38 |
| Held D: frame work p50 | 2.1 ms | 43 ms | 22–36 ms |

Over a few runs the App UI's frames varied more than the classic client's.
The rest of a frame is MyGo's rasterizing of what changed: blending the
translucent blocks (3 ms), the text's glyphs (3 ms) and the remaining
operations. Panning changes the whole timeline each frame, about 850,000
pixels. With WebGL (hardware), none of that is the CPU's. In headless
Chrome's software WebGL the app's own work was about 14 ms a frame (paint
6.7, GPU command building and WebGL calls 4.5, layout 1.3), the rest the
browser emulating the GPU.

## Reading PyTorch profiler traces (0.5.0)

`trace.LoadTorch` on a synthetic PyTorch profile (`go run ./cmd/gentorch
-ranks 1 -steps 150 -layers 24`: 125 MB of Kineto JSON, 397,205 events), on
this machine:

| | Time | Throughput |
|---|---:|---:|
| Decoding the events one by one (encoding/json) | 1.5 s | 84 MB/s |
| Scanned into batches, decoded by workers in parallel (as it ships) | 0.36–0.39 s | about 330 MB/s |

The trace streams in. A scanner cuts `traceEvents` into raw events without
decoding them, workers decode batches of 4,096 in parallel, and the events
go into the trace in file order, so `B`/`E` spans pair up as written.
Working out the rank overview's sub-lanes adds about 1 ms. A gzipped trace
reads at gzip's speed, on one core.

## Reading PyTorch traces, faster (0.5.1)

The same synthetic profile as in 0.5.0 (125 MB, 397,205 events), read by
`trace.LoadTorch`:

| | 0.5.0 | 0.5.1 |
|---|---:|---:|
| Plain JSON | 0.36–0.39 s | 0.26–0.30 s |
| Gzipped (8.5 MB) | single-threaded gunzip, then scanning | 0.25 s |

Two things changed. The scanner that cuts the events out finds the end of
each string with `bytes.IndexByte`, since most of a trace's bytes are in
names, and looks at the bytes between strings through a table. And a
gzipped trace is decompressed by a goroutine of its own, a few megabytes
ahead of the scanner, so the two overlap. The workers decoding the events
were already parallel.

## Rows a screen (0.5.2)

Rows fully visible in the timeline of the default layout at 1600×1000
(613 pixels below the ruler), in full density, as a trace opens; and the
average over the pages of rows, scrolling a screen at a time:

| Synthetic trace | Rows (0.5.1) | Rows (0.5.2) | Average a page (0.5.1 → 0.5.2) |
|---|---:|---:|---:|
| `samples/ranks`: 8 ranks, 24 streams, 56 rows | 13 (6 streams) | 17 (9 streams) | 10.4 → 13.5 |
| 4 ranks merged into one PyTorch file, 28 rows | 13 (4 streams) | 14 (4 streams) | 8.7 → 13.5 |

A stream's or CUDA API row's lane is 16 pixels with 1 above and below
(theme.DenseLaneH, DensePad): 18 pixels for a row of one lane, 34 for two,
from 28 and 48. NVTX, CPU op and Python rows are unchanged. Drawing costs
the same (webcheck's draw p50 1.0–1.3 ms; clientbench, JavaScript client,
16 ranks at 30–80 ms latency: refill p50 82–99 ms, p95 132 ms, held-key
panning at 60 frames a second, as in 0.5.1).

## Choosing which traces show (0.5.3)

Leaving traces of a session out, or showing them again, lays the rows out
again and draws a frame: 4–7 ms on the desktop for 4 large ranks (about
150 rows); the traces left out fetch no tiles. They are loaded and aligned
all the same, so that the session's time axis and a link's view are the
same whichever show, as in the desktop app, which loads every trace of a
session. The JavaScript client's refill and panning are as in 0.5.2
(clientbench, 16 ranks at 30–80 ms latency: refill p50 99 ms, p95 132 ms,
60 frames a second).

Kernels in three shades with a seam between them, and names from 18-pixel
boxes (before, 33): webcheck's draw p50 is as before (1.0–1.3 ms on the
synthetic traces; 1.3 ms against 1.4–1.6 ms on a large 4-rank session),
its p95 on that session a little higher (19–21 ms against 17–18 ms), from
naming more of the small boxes. clientbench, 16 ranks at 30–80 ms
latency: refill p50 96–98 ms, p95 132 ms, held-key panning at 60 frames a
second, as before.
