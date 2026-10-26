// Command traceshot draws a part of the timeline of GPU traces, across
// several ranks, into a PNG: headless, without a window or a browser, as
// Tracelight's viewer draws it (its palette, its fonts, its rank
// alignment). It reads what Tracelight reads: Nsight Systems reports
// (.nsys-rep, natively for the versions it decodes) and their SQLite
// exports, and PyTorch profiler traces (.json, .json.gz), one rank a file
// or several merged into one.
//
//	traceshot [render] [flags] TRACE|DIR ...   draw a window into a PNG
//	traceshot info     [flags] TRACE|DIR ...   the ranks, what took the time, the steps
//	traceshot find     -name REGEX TRACE|DIR ... the events named so, with windows
//	traceshot gaps     [flags] TRACE|DIR ...   where the GPUs idle
//
// Times are on the session's axis: 0 is the first event of the earliest
// rank shown, ranks aligned as the viewer aligns them (by the ends of
// their collective kernels where they have them, else by their clocks).
// Run with -h for the flags. See skills/trace-timeline-screenshot/SKILL.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
	"tracelight/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "traceshot:", err)
		}
		os.Exit(2)
	}
}

const usage = `traceshot draws part of the timeline of GPU traces, across ranks, into a PNG.

Usage:
  traceshot [render] [flags] TRACE|DIR ...     draw a window into a PNG
  traceshot info     [flags] TRACE|DIR ...     the ranks, the top GPU work, the repeated ranges (steps)
  traceshot find     -name REGEX [flags] TRACE|DIR ...   events named so, with -window values
  traceshot gaps     [flags] TRACE|DIR ...     the longest times the GPUs idle, with -window values
  traceshot version

Traces: .nsys-rep, .sqlite (nsys export), PyTorch .json/.json.gz (one rank, or ranks merged);
a directory stands for the traces in it. Times: 1.25s, 300ms, 40us, 500ns (seconds without a
unit), on the session's axis (0 = the first event of the earliest rank loaded).
Run "traceshot COMMAND -h" for a command's flags.
`

func run(args []string, stdout, stderr io.Writer) error {
	cmd := "render"
	if len(args) > 0 {
		switch args[0] {
		case "render", "info", "find", "gaps":
			cmd, args = args[0], args[1:]
		case "version", "-version", "--version":
			fmt.Fprintln(stdout, "traceshot", version.Version)
			return nil
		case "help", "-h", "-help", "--help":
			fmt.Fprint(stdout, usage)
			return nil
		}
	}
	fs := flag.NewFlagSet("traceshot "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	ranksF := fs.String("ranks", "", "ranks to load: 0,2,4-7 (default all); a rank's number is the one its file's name ends with (rank3.sqlite), else the one the trace says, else its place")
	alignF := fs.String("align", "collectives", "how ranks line up: collectives (ends of collective kernels, then clocks), abs (capture clocks), start (all start at 0)")
	nsysF := fs.String("nsys", "", "nsys, to convert the .nsys-rep reports the native decoder cannot read (default $NSYS, then nsys on the PATH)")
	windowF := fs.String("window", "", "the window: START:END or START+DURATION (1.2s:1.35s, 1.2s+150ms); default all")

	var (
		out, layoutF, rowsF, matchF, skipF, hiF, themeF, title, aroundF, markF *string
		width, gutter, maxH, nth, depth, legend, atRank                        *int
		scale, padF                                                            *float64
		emptyF                                                                 *bool
		nameF, kindF                                                           *string
		limit                                                                  *int
		byDur                                                                  *bool
		minGap                                                                 *string
		top                                                                    *int
	)
	switch cmd {
	case "render":
		out = fs.String("o", "traceshot.png", "the PNG to write")
		layoutF = fs.String("layout", "auto", "rows (each row as in the viewer), summary (a busy strip and a lane per kind of row, each rank), or auto (rows if they fit -max-height)")
		rowsF = fs.String("rows", "", "kinds of rows, of gpu,nvtx,python,cpu,api (default: all for rows; gpu,nvtx for summary, or gpu,cpu without NVTX)")
		matchF = fs.String("match", "", "keep only the rows whose \"heading · name\" matches this regexp (\"GPU 0\", \"Stream 7\", \"NVTX\")")
		skipF = fs.String("skip", "", "leave out the rows whose \"heading · name\" matches this regexp")
		hiF = fs.String("highlight", "", "events whose names match this regexp stand out, the others dimmed")
		aroundF = fs.String("around", "", "frame an event whose name matches this regexp (within -window, if given), with -pad around it")
		nth = fs.Int("nth", 1, "with -around, which match on the -at-rank rank: 1 the first, 2 the second, -1 the last")
		atRank = fs.Int("at-rank", -1, "with -around, the rank whose matches -nth counts (default the first rank loaded)")
		padF = fs.Float64("pad", 0.08, "with -around, the room on either side, as a fraction of the event's length")
		markF = fs.String("mark", "", "times to mark with a line, or START:END ranges to shade: 1.2s,1.3s:1.31s")
		width = fs.Int("width", 1600, "width in DIPs (the PNG is -scale times as wide)")
		scale = fs.Float64("scale", 2, "pixels per DIP: 2 is crisp on any screen, 1 is a smaller file")
		gutter = fs.Int("gutter", 230, "width of the row labels' gutter, in DIPs")
		maxH = fs.Int("max-height", 4000, "the most height in DIPs; rows past it are left out, and the picture says how many")
		depth = fs.Int("depth", 6, "the most nesting depths (lanes) of a row of ranges, operators or calls (GPU streams show all theirs)")
		legend = fs.Int("legend", 9, "names in the legend of the top GPU work in the window (0 for none)")
		themeF = fs.String("theme", "dark", "dark or light")
		title = fs.String("title", "", "the title (default the first input's name)")
		emptyF = fs.Bool("empty", false, "keep rows with no events in the window")
	case "find":
		nameF = fs.String("name", "", "a regexp the events' names match (required): (?i)allreduce, ^ProfilerStep, nccl")
		kindF = fs.String("rows", "gpu,nvtx,python,cpu,api", "kinds of rows to look in")
		limit = fs.Int("limit", 20, "the most matches listed")
		byDur = fs.Bool("longest", false, "list the longest matches first, not the earliest")
		padF = fs.Float64("pad", 0.08, "room around each match in the -window values printed, as a fraction of its length")
	case "gaps":
		minGap = fs.String("min", "1ms", "the shortest gap listed")
		limit = fs.Int("limit", 15, "the most gaps listed")
		padF = fs.Float64("pad", 0.5, "room around each gap in the -window values printed, as a fraction of its length")
	case "info":
		top = fs.Int("top", 12, "names listed of each table")
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: traceshot %s [flags] TRACE|DIR ...\n\n", cmd)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	inputs := fs.Args()
	if len(inputs) == 0 {
		fs.Usage()
		return errors.New("no traces given")
	}
	switch *alignF {
	case "collectives", "abs", "start":
	default:
		return fmt.Errorf("-align %q: collectives, abs or start", *alignF)
	}
	sel, err := parseRanks(*ranksF)
	if err != nil {
		return err
	}
	ss, err := load(trace.OpenOptions{Nsys: *nsysF}, inputs, sel, *alignF)
	if err != nil {
		return err
	}
	t0, t1 := int64(0), ss.span
	if *windowF != "" {
		if t0, t1, err = parseWindow(*windowF, ss.span); err != nil {
			return err
		}
	}

	switch cmd {
	case "info":
		info(stdout, ss, *top)
		return nil
	case "find":
		if *nameF == "" {
			return errors.New("find: give -name REGEX")
		}
		re, err := regexp.Compile(*nameF)
		if err != nil {
			return fmt.Errorf("-name: %w", err)
		}
		keys, err := parseKinds(*kindF)
		if err != nil {
			return err
		}
		printOccurrences(stdout, findEvents(ss, re, keys, t0, t1), *limit, *byDur, *padF)
		return nil
	case "gaps":
		m, err := parseTime(*minGap)
		if err != nil {
			return fmt.Errorf("-min: %w", err)
		}
		printGaps(stdout, findGaps(ss, t0, t1, m), *limit, *padF)
		return nil
	}

	// render
	o := &options{t0: t0, t1: t1, width: float32(*width), scale: float32(*scale), gutter: float32(*gutter),
		depth: max(*depth, 1), legend: *legend, title: *title, maxH: float32(*maxH), empty: *emptyF}
	if o.width < 400 || o.width > 8000 {
		return errors.New("-width: from 400 to 8000")
	}
	if o.scale < 0.5 || o.scale > 4 {
		return errors.New("-scale: from 0.5 to 4")
	}
	o.gutter = min(max(o.gutter, 80), o.width/2)
	o.maxH = max(o.maxH, 300)
	if o.pal = theme.Palettes[*themeF]; o.pal == nil {
		return fmt.Errorf("-theme %q: dark or light", *themeF)
	}
	for _, f := range []struct {
		flag string
		s    string
		re   **regexp.Regexp
	}{{"-match", *matchF, &o.match}, {"-skip", *skipF, &o.skip}, {"-highlight", *hiF, &o.highlight}} {
		if f.s == "" {
			continue
		}
		if *f.re, err = regexp.Compile(f.s); err != nil {
			return fmt.Errorf("%s: %w", f.flag, err)
		}
	}
	if *aroundF != "" {
		re, err := regexp.Compile(*aroundF)
		if err != nil {
			return fmt.Errorf("-around: %w", err)
		}
		at := ss.ranks[0].num
		if *atRank >= 0 {
			at = *atRank
		}
		var occ []occurrence
		for _, oc := range findEvents(ss, re, []string{"gpu", "nvtx", "python", "cpu", "api"}, t0, t1) {
			if oc.r.num == at {
				occ = append(occ, oc)
			}
		}
		if len(occ) == 0 {
			return fmt.Errorf("-around: no event of rank %d is named so (in the window); list them with traceshot find", at)
		}
		i := *nth - 1
		if *nth < 0 {
			i = len(occ) + *nth
		}
		if i < 0 || i >= len(occ) {
			return fmt.Errorf("-around -nth %d: rank %d has %d matches", *nth, at, len(occ))
		}
		o.t0, o.t1 = padded(occ[i].start, occ[i].end, *padF)
	}
	if o.t1 <= o.t0 {
		return errors.New("the window is empty")
	}
	if *markF != "" {
		for _, m := range strings.Split(*markF, ",") {
			a, b, isRange := strings.Cut(m, ":")
			ta, err := parseTime(a)
			if err != nil {
				return fmt.Errorf("-mark: %w", err)
			}
			tb := ta
			if isRange {
				if tb, err = parseTime(b); err != nil {
					return fmt.Errorf("-mark: %w", err)
				}
			}
			o.mark = append(o.mark, [2]int64{ta, tb})
		}
	}
	if *rowsF != "" {
		if o.kinds, err = parseKinds(*rowsF); err != nil {
			return err
		}
	}
	o.layout = *layoutF
	switch o.layout {
	case "rows", "summary":
	case "auto":
		o.layout = "rows"
		if o.kinds == nil {
			o.kinds = defaultKinds(ss, "rows")
		}
		if pc := layout(ss, o); pc.cut > 0 {
			o.layout = "summary"
			if *rowsF == "" {
				o.kinds = nil
			}
		}
	default:
		return fmt.Errorf("-layout %q: rows, summary or auto", o.layout)
	}
	if o.kinds == nil {
		o.kinds = defaultKinds(ss, o.layout)
	}
	pc := layout(ss, o)
	img := render(pc)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(f, img); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	b := img.Bounds()
	fmt.Fprintf(stdout, "wrote %s (%d×%d px): %d rank(s), layout %s, rows %s, window -window %s:%s (%s)\n",
		*out, b.Dx(), b.Dy(), len(ss.ranks), o.layout, strings.Join(o.kinds, ","), fmtArg(o.t0), fmtArg(o.t1), fmtDur(float64(o.t1-o.t0)))
	if pc.cut > 0 {
		fmt.Fprintf(stdout, "note: %d rows did not fit -max-height %d and were left out\n", pc.cut, *maxH)
	}
	return nil
}

func parseKinds(s string) ([]string, error) {
	var out []string
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		ok := false
		for _, kd := range kinds {
			ok = ok || kd.key == k
		}
		if !ok {
			return nil, fmt.Errorf("-rows: %q is not a kind of row: gpu, nvtx, python, cpu, api", k)
		}
		out = append(out, k)
	}
	return out, nil
}

// defaultKinds are the kinds of rows shown unless -rows says: all in the
// rows layout; in the summary layout the GPU work and the NVTX phases,
// what lines ranks up (the viewer's rank overview's default), or the CPU
// operators where no rank has NVTX ranges (PyTorch's traces).
func defaultKinds(ss *session, layout string) []string {
	if layout == "rows" {
		return []string{"gpu", "nvtx", "python", "cpu", "api"}
	}
	for _, r := range ss.ranks {
		for _, tk := range r.tr.Tracks {
			if tk.Kind == trace.TrackNVTX {
				return []string{"gpu", "nvtx"}
			}
		}
	}
	return []string{"gpu", "cpu"}
}
