package main

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"

	"tracelight/internal/trace"
)

// Imbalance across ranks, as collectives show it: every rank of a group
// runs the same collective kernel (all-reduce, all-gather, ...), and it
// ends at the same moment on all of them, since each waits for the others'
// data. When it starts on a rank's GPU is when that rank got there: a rank
// that arrives late (its work before the collective took longer) makes all
// the others wait, in their collective kernels. So for each collective,
// matched across ranks by its end, the spread of its starts is the
// imbalance, the last rank to start is the straggler, and the sum of the
// others' waits is the GPU time it cost.

// arrival is a rank's kernel of a collective, on the session's axis.
type arrival struct {
	r          *rank
	start, end int64
}

// collective is one collective operation, matched across ranks.
type collective struct {
	op   string
	name string // the kernel's name on the reference rank
	arr  []arrival
	// first and last index arr: the earliest and latest arrival.
	first, last int
	spread      int64 // arr[last].start - arr[first].start
	waited      int64 // the summed waits for the last rank
	start, end  int64 // the earliest start, the latest end
}

// lateness is how much later than the first rank r arrived, if it took part.
func (c *collective) lateness(r *rank) (int64, bool) {
	for _, a := range c.arr {
		if a.r == r {
			return a.start - c.arr[c.first].start, true
		}
	}
	return 0, false
}

// window is a window that shows the collective and, before it, the work
// of the rank that came last, which is what made the others wait. With
// whole false and a collective much longer than its ranks' spread (the
// ranks waited for something else too), it shows the arrivals instead,
// from a little before the first to a little after the last.
func (c *collective) window(whole bool) (int64, int64) {
	d := c.end - c.start
	if !whole && d > 6*c.spread && c.spread > 0 {
		lead := max(c.spread*3/2, 20_000)
		return max(c.start-lead, 0), c.arr[c.last].start + lead
	}
	lead := max(c.spread, d/3, 20_000)
	return max(c.start-lead, 0), c.end + max(d/6, 5_000)
}

// label names the collective by its operation and when its first rank
// started it, which stay the same whichever ranks are loaded with them.
func (c *collective) label() string { return c.op + " at " + fmtDur(float64(c.start)) }

// collectiveKernels lists r's collective kernels by operation, sorted by
// end, with their names.
func collectiveKernels(r *rank) map[string][]named {
	out := map[string][]named{}
	ops := map[int32]string{}
	for _, tk := range r.tr.Tracks {
		if tk.Kind != trace.TrackStream {
			continue
		}
		for _, l := range tk.Lanes {
			for i := range l.Events {
				e := &l.Events[i]
				if e.Kind != trace.KindKernel {
					continue
				}
				op, ok := ops[e.Name]
				if !ok {
					op = trace.CollectiveOp(r.tr.Name(e))
					ops[e.Name] = op
				}
				if op != "" {
					out[op] = append(out[op], named{arrival{r, r.toSession(e.Start), r.toSession(e.End)}, e.Name})
				}
			}
		}
	}
	for _, v := range out {
		slices.SortFunc(v, func(a, b named) int { return cmp.Compare(a.end, b.end) })
	}
	return out
}

type named struct {
	arrival
	name int32
}

// matchTol is how far apart the ends of one collective may be on two ranks
// for them to be matched: the ranks are aligned on their collectives' ends
// to microseconds, so apart by more they are different operations.
func matchTol(d int64) int64 { return max(200_000, d/50) }

// captureEdge: a collective whose kernel starts this close to the start of
// a rank's capture may have begun before it, so when that rank arrived is
// not known, and the collective is left out (as is each rank's first
// collective: see findCollectives).
const captureEdge = 1_000_000

// findCollectives matches the collectives of the ranks loaded that end in
// [t0, t1), whose operation matches opRE (nil for all), taking part on at
// least minRanks ranks; the most imbalanced first.
func findCollectives(ss *session, t0, t1 int64, opRE *regexp.Regexp, minRanks int) []*collective {
	if len(ss.ranks) < 2 {
		return nil
	}
	ref := ss.ranks[0]
	per := make([]map[string][]named, len(ss.ranks))
	for i, r := range ss.ranks {
		per[i] = collectiveKernels(r)
	}
	// The reference: the rank with the most collectives.
	best := -1
	for i := range ss.ranks {
		n := 0
		for _, v := range per[i] {
			n += len(v)
		}
		if n > best {
			best, ref = n, ss.ranks[i]
		}
	}
	ri := slices.Index(ss.ranks, ref)
	// used marks the kernels already matched, so that two collectives of
	// the reference that end together (on two streams) take two kernels of
	// each other rank, not the same one twice.
	used := make([]map[string][]bool, len(ss.ranks))
	for i := range ss.ranks {
		used[i] = map[string][]bool{}
		for op, v := range per[i] {
			used[i][op] = make([]bool, len(v))
		}
	}
	// firstColl is when each rank's first collective starts: the ranks'
	// captures begin at different moments, so the first collective after
	// one began says when its rank started profiling, not how balanced the
	// work was.
	firstColl := make([]int64, len(ss.ranks))
	for i := range ss.ranks {
		firstColl[i] = math.MaxInt64
		for _, v := range per[i] {
			for _, e := range v {
				firstColl[i] = min(firstColl[i], e.start)
			}
		}
	}
	var out []*collective
	ops := slices.Sorted(maps.Keys(per[ri]))
	for _, op := range ops {
		for _, e := range per[ri][op] {
			name := ref.tr.Names[e.name]
			if e.end < t0 || e.end >= t1 || (opRE != nil && !opRE.MatchString(op) && !opRE.MatchString(name)) {
				continue
			}
			c := &collective{op: op, name: name}
			unknown := false // a rank's capture began as it ran: when it arrived is not known
			for i, r := range ss.ranks {
				var got *named
				if i == ri {
					got = &e
				} else {
					o, u := per[i][op], used[i][op]
					j, _ := slices.BinarySearchFunc(o, e.end, func(a named, t int64) int { return cmp.Compare(a.end, t) })
					best := -1
					for m := max(j-3, 0); m < min(j+3, len(o)); m++ {
						if !u[m] && (best < 0 || abs(o[m].end-e.end) < abs(o[best].end-e.end)) {
							best = m
						}
					}
					if best >= 0 && abs(o[best].end-e.end) <= matchTol(min(o[best].end-o[best].start, e.end-e.start)) {
						u[best] = true
						got = &o[best]
					}
				}
				if got != nil {
					unknown = unknown || got.start-r.offset < captureEdge || got.start == firstColl[i]
					c.arr = append(c.arr, got.arrival)
				}
			}
			if unknown {
				continue
			}
			if len(c.arr) < max(minRanks, 2) {
				continue
			}
			c.start, c.end = int64(math.MaxInt64), int64(math.MinInt64)
			for i, a := range c.arr {
				if a.start < c.arr[c.first].start {
					c.first = i
				}
				if a.start > c.arr[c.last].start {
					c.last = i
				}
				c.start, c.end = min(c.start, a.start), max(c.end, a.end)
			}
			c.spread = c.arr[c.last].start - c.arr[c.first].start
			for _, a := range c.arr {
				c.waited += c.arr[c.last].start - a.start
			}
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *collective) int {
		return cmp.Or(cmp.Compare(b.spread, a.spread), cmp.Compare(a.start, b.start), strings.Compare(a.op, b.op))
	})
	return out
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// printImbalance lists the most imbalanced collectives, then, for each
// rank, how often it came last and how late it was: which rank holds the
// others up, and by how much.
func printImbalance(w io.Writer, ss *session, cs []*collective, limit int) {
	if len(ss.ranks) < 2 {
		fmt.Fprintln(w, "imbalance needs at least 2 ranks loaded")
		return
	}
	ops := map[string]int{}
	for _, c := range cs {
		ops[c.op]++
	}
	var opl []string
	for op, n := range ops {
		opl = append(opl, fmt.Sprintf("%s %d", op, n))
	}
	slices.Sort(opl)
	fmt.Fprintf(w, "%d collective(s) matched across ranks", len(cs))
	if len(opl) > 0 {
		fmt.Fprintf(w, " (%s)", strings.Join(opl, ", "))
	}
	if b := ss.basis(); !strings.Contains(b, "collectives") {
		fmt.Fprintf(w, "; note: the ranks are %s, not by collectives, so few may match (use -align collectives)", cmp.Or(b, "not aligned"))
	}
	fmt.Fprintln(w)
	if len(cs) == 0 {
		fmt.Fprintln(w, "none: the ranks loaded share no collective kernels in the window (try other -ranks, or a wider -window)")
		return
	}
	fmt.Fprintln(w, "\nMost imbalanced first (spread: how much later the last rank started the collective than the first;")
	fmt.Fprintln(w, "waited: the GPU time the other ranks spent waiting for it). Render one with: traceshot -imbalance N ...")
	fmt.Fprintf(w, "%-4s %-12s %12s %10s %-14s %-6s %10s  %s\n", "N", "collective", "starts at", "spread", "last (ranks)", "first", "waited", "-window")
	for i, c := range cs[:min(limit, len(cs))] {
		a, b := c.window(false)
		fmt.Fprintf(w, "%-4d %-12s %12s %10s %-14s %-6d %10s  %s\n", i+1, c.op, fmtDur(float64(c.start)), fmtDur(float64(c.spread)),
			fmt.Sprintf("%d (%d)", c.arr[c.last].r.num, len(c.arr)), c.arr[c.first].r.num, fmtDur(float64(c.waited)), fmtArg(a)+":"+fmtArg(b))
	}
	// By rank.
	fmt.Fprintf(w, "\nBy rank, over the %d collectives (late: after the first rank to start each):\n", len(cs))
	fmt.Fprintf(w, "%-5s %-18s %12s %12s %12s %12s\n", "rank", "label", "came last", "mean late", "p90 late", "max late")
	for _, r := range ss.ranks {
		var lates []int64
		last := 0
		for _, c := range cs {
			if l, ok := c.lateness(r); ok {
				lates = append(lates, l)
				if c.arr[c.last].r == r && c.spread > 0 {
					last++
				}
			}
		}
		if len(lates) == 0 {
			fmt.Fprintf(w, "%-5d %-18s %12s\n", r.num, short(r.label), "-")
			continue
		}
		slices.Sort(lates)
		var sum int64
		for _, l := range lates {
			sum += l
		}
		fmt.Fprintf(w, "%-5d %-18s %12s %12s %12s %12s\n", r.num, short(r.label), fmt.Sprintf("%d/%d", last, len(lates)),
			fmtDur(float64(sum)/float64(len(lates))), fmtDur(float64(lates[min(len(lates)-1, len(lates)*9/10)])), fmtDur(float64(lates[len(lates)-1])))
	}
}
