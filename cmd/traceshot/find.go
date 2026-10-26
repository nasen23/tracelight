package main

import (
	"cmp"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"tracelight/internal/trace"
)

// What helps pick a window: info (the ranks, their span, what took the
// time, the ranges that repeat, which are steps), find (the events a
// pattern names, with windows around them) and gaps (where GPUs idle).

const nameW = 72 // the most of a name a listing prints

var digitsRun = regexp.MustCompile(`[0-9]+`)

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > nameW {
		return string(r[:nameW-1]) + "…"
	}
	return s
}

// info prints the session's ranks, the GPU work that took the most time,
// and the outermost ranges that repeat (the candidates for steps).
func info(w io.Writer, ss *session, top int) {
	fmt.Fprintf(w, "%d rank(s), span 0 – %s", len(ss.ranks), fmtDur(float64(ss.span)))
	if b := ss.basis(); b != "" {
		fmt.Fprintf(w, ", %s", b)
	}
	fmt.Fprintln(w, " (times below are on this axis; pass them to -window)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-5s %-18s %12s %12s %9s %11s   rows gpu/nvtx/python/cpu/api\n", "rank", "label", "starts at", "duration", "gpu busy", "events")
	all := &options{t0: 0, t1: ss.span}
	for _, r := range ss.ranks {
		n := map[string]int{}
		for _, tk := range r.tr.Tracks {
			n[keyOf(tk.Kind)]++
		}
		busy := "-"
		if b := all.busyShare(r); b >= 0 {
			busy = trimFloat(100*b, 1) + "%"
		}
		fmt.Fprintf(w, "%-5d %-18s %12s %12s %9s %11s   %d/%d/%d/%d/%d\n", r.num, short(r.label), fmtDur(float64(r.offset)),
			fmtDur(float64(r.tr.Duration())), busy, fmtCount(r.tr.EventCount()), n["gpu"], n["nvtx"], n["python"], n["cpu"], n["api"])
	}

	type stat struct {
		name   string
		count  int
		total  int64
		durs   []int64
		first  int64
		ranks  map[int]bool
		isKind trace.TrackKind
	}
	gpu, ranges := map[string]*stat{}, map[string]*stat{}
	for _, r := range ss.ranks {
		for _, tk := range r.tr.Tracks {
			var m map[string]*stat
			lanes := tk.Lanes
			switch tk.Kind {
			case trace.TrackStream:
				m = gpu
			case trace.TrackNVTX, trace.TrackCPU:
				m = ranges
				lanes = lanes[:min(len(lanes), 1)] // the outermost
			default:
				continue
			}
			for _, l := range lanes {
				for i := range l.Events {
					e := &l.Events[i]
					name := r.tr.Name(e)
					if tk.Kind != trace.TrackStream {
						name = digitsRun.ReplaceAllString(name, "<n>") // step 12, step 13: one range
					}
					s := m[name]
					if s == nil {
						s = &stat{name: name, first: r.toSession(e.Start), ranks: map[int]bool{}, isKind: tk.Kind}
						m[name] = s
					}
					s.count++
					s.total += e.Dur()
					s.first = min(s.first, r.toSession(e.Start))
					s.ranks[r.num] = true
					if tk.Kind != trace.TrackStream {
						s.durs = append(s.durs, e.Dur())
					}
				}
			}
		}
	}
	sorted := func(m map[string]*stat, by func(a, b *stat) int) []*stat {
		var out []*stat
		for _, s := range m {
			out = append(out, s)
		}
		slices.SortFunc(out, func(a, b *stat) int { return cmp.Or(by(a, b), strings.Compare(a.name, b.name)) })
		return out[:min(len(out), top)]
	}
	if len(gpu) > 0 {
		var sum int64
		for _, s := range gpu {
			sum += s.total
		}
		fmt.Fprintf(w, "\nTop GPU work, all ranks, by total time:\n")
		fmt.Fprintf(w, "  %10s %6s %9s %10s  %s\n", "total", "share", "count", "avg", "name")
		for _, s := range sorted(gpu, func(a, b *stat) int { return cmp.Compare(b.total, a.total) }) {
			fmt.Fprintf(w, "  %10s %5s%% %9s %10s  %s\n", fmtDur(float64(s.total)), trimFloat(100*float64(s.total)/float64(max(sum, 1)), 1),
				fmtCount(s.count), fmtDur(float64(s.total)/float64(s.count)), short(s.name))
		}
	}
	// Ranges that repeat, on every rank shown, longest first: the steps.
	reps := map[string]*stat{}
	for k, s := range ranges {
		if s.count >= 2*len(ss.ranks) && len(s.ranks) == len(ss.ranks) {
			reps[k] = s
		}
	}
	if len(reps) > 0 {
		fmt.Fprintf(w, "\nOutermost NVTX ranges / CPU ops that repeat on every rank (candidate steps; <n> stands for a number), longest first:\n")
		fmt.Fprintf(w, "  %9s %11s %12s  %s\n", "count", "median", "first at", "name")
		for _, s := range sorted(reps, func(a, b *stat) int { return cmp.Compare(median(b.durs), median(a.durs)) }) {
			fmt.Fprintf(w, "  %9s %11s %12s  %s\n", fmtCount(s.count), fmtDur(float64(median(s.durs))), fmtDur(float64(s.first)), short(s.name))
		}
	}
	fmt.Fprintln(w, "\nNext: `traceshot find -name REGEX ...` lists occurrences with windows; `traceshot gaps ...` where GPUs idle.")
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := slices.Clone(v)
	slices.Sort(c)
	return c[len(c)/2]
}

// occurrence is an event a pattern matched, on the session's axis.
type occurrence struct {
	r          *rank
	start, end int64
	name       string
	kind       trace.TrackKind
	depth      int
}

// findEvents lists the events of the rows of kinds whose names match re,
// in the window, by start.
func findEvents(ss *session, re *regexp.Regexp, kindKeys []string, t0, t1 int64) []occurrence {
	var out []occurrence
	for _, r := range ss.ranks {
		a, b := r.toTrace(t0), r.toTrace(t1)
		matches := map[int32]bool{}
		for _, tk := range r.tr.Tracks {
			if !slices.Contains(kindKeys, keyOf(tk.Kind)) {
				continue
			}
			for li, l := range tk.Lanes {
				i, j := l.Visible(a, b)
				for k := i; k < j; k++ {
					e := &l.Events[k]
					m, ok := matches[e.Name]
					if !ok {
						m = re.MatchString(r.tr.Name(e))
						matches[e.Name] = m
					}
					if m {
						out = append(out, occurrence{r, r.toSession(e.Start), r.toSession(e.End), r.tr.Name(e), tk.Kind, li})
					}
				}
			}
		}
	}
	slices.SortStableFunc(out, func(x, y occurrence) int {
		return cmp.Or(cmp.Compare(x.start, y.start), cmp.Compare(x.r.num, y.r.num))
	})
	return out
}

// padded is the window around [a, b) with frac of its length on either
// side.
func padded(a, b int64, frac float64) (int64, int64) {
	p := int64(frac * float64(max(b-a, 1)))
	return max(a-p, 0), b + p
}

func printOccurrences(w io.Writer, occ []occurrence, limit int, byDur bool, pad float64) {
	if byDur {
		slices.SortStableFunc(occ, func(x, y occurrence) int { return cmp.Compare(y.end-y.start, x.end-x.start) })
	}
	fmt.Fprintf(w, "%d match(es)", len(occ))
	if len(occ) > limit {
		fmt.Fprintf(w, ", the first %d", limit)
		if byDur {
			fmt.Fprint(w, " by duration")
		}
	}
	fmt.Fprintln(w, ":")
	if len(occ) == 0 {
		return
	}
	fmt.Fprintf(w, "%-4s %-5s %12s %12s %8s  %-28s %s\n", "#", "rank", "start", "duration", "kind", "-window (padded)", "name")
	for i, o := range occ[:min(limit, len(occ))] {
		a, b := padded(o.start, o.end, pad)
		fmt.Fprintf(w, "%-4d %-5d %12s %12s %8s  %-28s %s\n", i+1, o.r.num, fmtDur(float64(o.start)), fmtDur(float64(o.end-o.start)),
			keyOf(o.kind), fmtArg(a)+":"+fmtArg(b), short(o.name))
	}
}

// gap is a time a rank's GPUs all idle.
type gap struct {
	r          *rank
	start, end int64
}

// findGaps lists the times, at least minDur long, in the window and inside
// each rank's trace, that none of its GPU streams is busy, longest first.
func findGaps(ss *session, t0, t1, minDur int64) []gap {
	var out []gap
	for _, r := range ss.ranks {
		o := &options{t0: max(t0, r.offset), t1: min(t1, r.offset+r.tr.Duration())}
		if o.t1 <= o.t0 {
			continue
		}
		ivs := o.gpuIntervals(r)
		if ivs == nil {
			continue
		}
		at := o.t0
		for _, iv := range append(ivs, [2]int64{o.t1, o.t1}) {
			if iv[0]-at >= minDur {
				out = append(out, gap{r, at, iv[0]})
			}
			at = max(at, iv[1])
		}
	}
	slices.SortStableFunc(out, func(x, y gap) int { return cmp.Compare(y.end-y.start, x.end-x.start) })
	return out
}

func printGaps(w io.Writer, gaps []gap, limit int, pad float64) {
	fmt.Fprintf(w, "%d GPU idle gap(s)", len(gaps))
	if len(gaps) > limit {
		fmt.Fprintf(w, ", the %d longest", limit)
	}
	fmt.Fprintln(w, ":")
	if len(gaps) == 0 {
		return
	}
	fmt.Fprintf(w, "%-4s %-5s %12s %12s  %s\n", "#", "rank", "start", "idle for", "-window (padded)")
	for i, g := range gaps[:min(limit, len(gaps))] {
		a, b := padded(g.start, g.end, pad)
		fmt.Fprintf(w, "%-4d %-5d %12s %12s  %s\n", i+1, g.r.num, fmtDur(float64(g.start)), fmtDur(float64(g.end-g.start)), fmtArg(a)+":"+fmtArg(b))
	}
}
