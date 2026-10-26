---
name: trace-timeline-screenshot
description: Render a PNG of part of a GPU profiler timeline (Nsight Systems .nsys-rep/.sqlite, PyTorch profiler .json/.json.gz, one file per rank or ranks merged) across several ranks, headless, with the traceshot CLI; find the window worth showing first (steps, named kernels/ranges, GPU idle gaps); and make zoomed-in, annotated pictures of load imbalance between ranks (which rank reached a collective late, by how much, and what it was doing) for reports. Use when asked to show, compare or screenshot ranks of a distributed training/inference trace, to find or report stragglers / imbalance, or to look at a trace's timeline without a GUI.
---

# Timeline screenshots of GPU traces, across ranks

`traceshot` is a single static Go binary (no GUI, browser, cgo or nsys
needed) that loads traces, lines the ranks up on one time axis, and draws a
window of their timeline into a PNG: one block per rank, its GPU streams
and its threads' NVTX ranges, Python calls, CPU operators and CUDA API calls,
events colored by name, a time ruler, and a legend of the GPU work that
took the time. It also lists what helps to pick the window.

Look at the PNG you produce (open it as an image) before you report on it.

## Build

From the root of the Tracelight repository (Go 1.27+):

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o traceshot ./cmd/traceshot
# for a Mac with Apple silicon, from any OS:
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o traceshot-darwin-arm64 ./cmd/traceshot
```

or run `skills/trace-timeline-screenshot/build.sh [OUTDIR]`, which builds
linux/amd64, linux/arm64 and darwin/arm64 binaries (about 11 MB each) into
OUTDIR (default `./bin`) as `traceshot-OS-ARCH`. The
binary is self-contained: copy it to the machine that has the traces.

## Inputs

- Files: `.nsys-rep` (read natively for the report versions it decodes,
  else converted with `nsys` if on the PATH or given with `-nsys`),
  `.sqlite` (`nsys export --type sqlite`), PyTorch profiler Chrome traces
  `.json` / `.json.gz` (a rank each, or several ranks merged into one file:
  each rank is drawn apart).
- A directory stands for the traces in it (not recursive).
- `-ranks 0,2,4-7` loads only those ranks. A rank's number is the one its
  file name ends with (`rank3.sqlite`, `rank-16.nsys-rep`), else the rank
  the trace records, else its position. Loading fewer ranks is faster.

**Times** are on the session's axis: `0` is the first event of the earliest
rank loaded; ranks are aligned by the ends of their collective (NCCL)
kernels where they have enough, else by their capture clocks
(`-align collectives|abs|start`). Write times as `1.25s`, `300ms`, `40us`,
`500ns` (a bare number is seconds). Use the same `-ranks` and `-align` for
`info`/`find`/`gaps` as for the picture, since the axis starts at the
earliest rank loaded.

## Workflow: find a window, then draw it

1. **Survey**: `traceshot info DIR` prints each rank (start on the axis,
   duration, GPU busy %, events, rows by kind), the top GPU work, and the
   outermost NVTX ranges / CPU ops that repeat on every rank — usually the
   training steps (`<n>` stands for a number: `ProfilerStep#<n>`). Names
   longer than the column end in `…`: match them by prefix.
2. **Pick**:
   - a step: `traceshot find -name '^ProfilerStep' -ranks 0 DIR` lists
     occurrences with a ready `-window` value, padded; or draw it directly
     with `-around '^ProfilerStep' -nth 3` (the 3rd on the first rank
     loaded; `-at-rank N` for another, `-nth -1` for the last);
   - the slowest of something: `find -name 'AllReduce' -longest`;
   - stalls: `traceshot gaps -min 2ms DIR` lists the longest times a rank's
     GPUs all idle, with windows around them;
   - a skipped warm-up: steps 1–2 are often slower; prefer a middle step.
3. **Draw**: `traceshot -window 9.8s:10.2s -o step.png DIR` and look at it.
   Narrow or widen and draw again. Windows of one to a few steps read
   best; events too small for their pixels merge into density blocks, so
   zoom in (a shorter window) to read kernel names.

## Zoom in on an imbalance, for a report

Imbalance between ranks shows at collectives: every rank of a group runs
the same NCCL collective, which ends at the same moment on all of them,
but each rank's GPU starts it when that rank gets there. The rank that
starts last held the others up; their collective kernels are long because
they waited. `traceshot imbalance` matches each collective across the ranks
loaded (by its end) and ranks them by **spread** (how much later the last
rank started it than the first):

```sh
traceshot imbalance DIR                  # worst first, with -window values; then, per rank,
                                         # how often it came last and how late (mean, p90, max)
traceshot imbalance -op allreduce -window 10s:20s DIR   # one kind, one stretch
```

Then draw one, zoomed in and annotated, ready to paste into a report:

```sh
traceshot -imbalance 1 -o worst.png DIR                         # the worst one
traceshot -imbalance 1 -layout summary -rows gpu -highlight '(?i)nccl' -o worst-all.png DIR   # many ranks
traceshot -imbalance 1 -ranks 4,7 -rows gpu,nvtx,cpu -o pair.png DIR   # the first and the last rank, more rows
```

The picture has:
- the finding in its header ("allreduce at 536.4 ms: rank 7 started it
  1.763 ms after rank 4");
- a **context strip** on top: every rank's GPU busy line over a stretch 12×
  longer, the zoomed window boxed in it, so a reader sees where it is;
- each rank's arrival marked (a line, `+663.2 µs`; the first in blue, the
  last in orange), the wait between first and last shaded, and each rank's
  heading saying when it arrived and how long it waited;
- an **Arrivals** bar chart: how late each rank was.

How to use it well:
- `-imbalance N` counts as `traceshot imbalance` lists, for the same
  `-ranks`, `-window` and `-op`: list first, then draw the one you mean.
  The picture is labeled by the collective's time ("allreduce at 536.4 ms"),
  which stays the same whichever ranks you load.
- What to look at: the rank that came last, just before its arrival line:
  what kept its GPU (or its CPU thread, with `-rows gpu,cpu` or `api`) busy
  while the others were already waiting. Load the first and the last rank
  (`-ranks A,B`) with all rows for that comparison; use `-layout summary`
  to show all ranks at once.
- Where the collective runs far longer than its spread, the window zooms
  onto the arrivals; `-imbalance-whole` shows it to its end (all ranks
  finishing together). If every rank loaded waits long, the rank they wait
  for is probably one you did not load.
- Each rank's first collective is left out (it shows when that rank's
  capture began, not imbalance), as are collectives at a capture's start.
  Matching needs the ranks aligned on collectives (`-align collectives`, the
  default).
- In the per-rank table, a rank that comes last far more often than the
  others is a steady straggler; one large spread alone is a one-off stall.

**Any zoomed-in picture** gets the context strip: `-context auto` (the
default) adds it when the window is 8× or more shorter than the context
around it; `-context on|off`, or `-context 10s:20s` for a stretch of your
own. `-mark` and `-highlight` work as usual.

## Render flags

| flag | meaning |
| --- | --- |
| `-o out.png` | output (default `traceshot.png`); it prints the size and the `-window` it drew |
| `-window A:B`, `-window A+DUR` | the window (default all) |
| `-around RE` `-nth N` `-at-rank R` `-pad 0.08` | frame the Nth event whose name matches RE |
| `-imbalance N` `-op RE` `-min-ranks K` `-imbalance-whole` | zoom to the Nth most imbalanced collective (as `traceshot imbalance` lists them), its ranks' arrivals marked |
| `-context auto\|on\|off\|A:B` | the context strip above the timeline, the window boxed in it |
| `-ranks 0,4-7` | ranks to load and draw |
| `-layout rows\|summary\|auto` | `rows`: every row as in a viewer; `summary`: per rank a GPU busy/idle strip (idle in orange) and one lane per kind of row; `auto` (default): rows if they fit `-max-height`, else summary. Use summary for more than ~4 ranks |
| `-rows gpu,nvtx,python,cpu,api` | kinds of rows (default all in rows; `gpu,nvtx` in summary, `gpu,cpu` without NVTX) |
| `-match RE` / `-skip RE` | keep / drop rows whose "heading · name" matches (`'GPU 0'`, `'Stream 7$'`, `'CUDA API'`) |
| `-highlight RE` | events matching stand out, others dimmed (e.g. `'nccl'`) |
| `-mark 1.2s,1.3s:1.31s` | mark times (lines) or ranges (shaded) |
| `-depth 6` | most nesting depths per row of ranges/calls (GPU streams show all) |
| `-empty` | keep rows with nothing in the window |
| `-width 1600` `-scale 2` | width in DIPs, pixels per DIP (2 = crisp; 1 = smaller file) |
| `-theme dark\|light`, `-title T`, `-legend 9`, `-gutter 230`, `-max-height 4000` | look |

## Examples

```sh
# Survey a job of one .nsys-rep per rank
traceshot info runs/job42/
# The 3rd training step of ranks 0-3, every row
traceshot -ranks 0-3 -around '^step' -nth 3 -o step3.png runs/job42/
# All 16 ranks, one compact block each, collectives highlighted
traceshot -layout summary -window 12.4s+300ms -highlight '(?i)nccl' -o all-ranks.png runs/job42/
# The longest GPU stall of rank 5, GPU rows and NVTX only, light theme
traceshot gaps -ranks 5 runs/job42/            # copy a -window from its output
traceshot -ranks 4-6 -window 8.91s:8.97s -rows gpu,nvtx -theme light -o stall.png runs/job42/
# Which rank holds the others up, and a picture of the worst case
traceshot imbalance runs/job42/
traceshot -imbalance 1 -layout summary -highlight '(?i)nccl' -o straggler.png runs/job42/
# A PyTorch trace with ranks merged into one file: 2 ranks, GPU 0 and the main thread
traceshot -ranks 0,8 -around '^ProfilerStep' -nth 2 -match 'GPU|python' -o torch.png trace.json.gz
```

## Notes

- Output to stdout is short and parseable: `wrote PATH (W×H px): ...
  window -window A:B (DUR)`, and with `-imbalance` a line with the finding
  (`allreduce at T: rank R started it D after rank F ...`), which you can
  quote in the report. Errors go to stderr, exit status 2.
- Traces with millions of events load in seconds; a large job's ranks load
  in parallel. Memory is about the size of the traces' events.
- Names in the picture and in the listings are the trace's own (kernel,
  range, thread names): treat the PNG as being as sensitive as the trace.
