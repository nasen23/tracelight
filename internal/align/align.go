// Package align puts the traces of a session on one time axis: Traces is
// the same function as shared.js's alignTraces (the viewer's
// TestBrowserClientMatchesDesktop checks them against each other), in
// float64 as the browser computes. The desktop viewer and traceshot share
// it.
package align

import (
	"math"
	"slices"
)

// Event times count from each capture's start. "abs" (Capture time) puts
// each trace at the moment its first event happened:
//
//   - by the wall clock of its capture (nsys records the session's start in
//     UTC: the export's utcEpochNs): basis "wall";
//   - failing that (a report read natively, which stores no wall clock),
//     by another trace of the same host that has it, through the host's
//     monotonic clock, which both started from (systemClockNs, and the
//     report's own record of it): basis "host", as exact as the other;
//   - failing that, by the host's monotonic clock among the host's traces
//     only, the earliest of them at 0: basis "monotonic";
//   - and failing that, at 0: basis "start".
//
// Hosts' wall clocks are only as close as their clock synchronization
// (NTP, PTP) keeps them. "start" mode puts every trace's first event at 0.

// In is a trace to align: its capture's start on the wall clock and
// on its host's monotonic clock (0 if unknown), its host, and the time of
// its first event from the capture's start.
type In struct {
	UTC  float64 `json:"utc"`
	Mono float64 `json:"mono"`
	Host string  `json:"host"`
	// Clock names the clock its monotonic start counts on where there is
	// no host to say it (trace.Trace.Clock: several ranks merged into one
	// file): traces of one clock compare as traces of one host do.
	Clock string  `json:"clock,omitempty"`
	Start float64 `json:"start"`
	// Coll are the ends of its collective kernels by operation, in ns from
	// its first event, sorted (trace.Collectives).
	Coll map[string][]float64 `json:"coll,omitempty"`
}

// Out is where a trace's first event is on the session's axis (the
// earliest at 0), and what that rests on.
type Out struct {
	Offset float64 `json:"offset"`
	Basis  string  `json:"basis"`
	// With basis "collectives": how many of its collective kernels matched
	// the reference trace's, and the 90th percentile of how far their ends
	// still are apart (ns); Ref marks the reference trace.
	Matches int     `json:"matches,omitempty"`
	Spread  float64 `json:"spread,omitempty"`
	Ref     bool    `json:"ref,omitempty"`
}

// clockHosts is ts with each Host, where there is none, its Clock's: what
// traces go together by their monotonic clocks (as shared.js's hostOf).
func clockHosts(ts []In) []In {
	out := slices.Clone(ts)
	for i := range out {
		if out[i].Host == "" && out[i].Clock != "" {
			out[i].Host = "\x00" + out[i].Clock
		}
	}
	return out
}

func Traces(ts []In, mode string) []Out {
	ts = clockHosts(ts)
	out := make([]Out, len(ts))
	for i := range out {
		out[i].Basis = "start"
	}
	if mode != "abs" {
		return out
	}
	// Wall clocks relative to the earliest (exact, where nanoseconds since
	// 1970 are not, in float64), +1 so that 0 says "none".
	ref := math.Inf(1)
	for _, t := range ts {
		if t.UTC > 0 {
			ref = math.Min(ref, t.UTC)
		}
	}
	wall := make([]float64, len(ts))
	for i, t := range ts {
		if t.UTC > 0 {
			wall[i], out[i].Basis = t.UTC-ref+1, "wall"
		}
	}
	for i, t := range ts {
		if wall[i] != 0 || t.Mono <= 0 || t.Host == "" {
			continue
		}
		for _, r := range ts {
			if r.Host == t.Host && r.UTC > 0 && r.Mono > 0 {
				wall[i], out[i].Basis = r.UTC-ref+1+(t.Mono-r.Mono), "host"
				break
			}
		}
	}
	origin := math.Inf(1)
	for i, t := range ts {
		if wall[i] != 0 {
			origin = math.Min(origin, wall[i]+t.Start)
		}
	}
	for i, t := range ts {
		if wall[i] != 0 {
			out[i].Offset = wall[i] + t.Start - origin
		}
	}
	// Hosts without a wall clock: by their monotonic clock, among
	// themselves.
	for i, t := range ts {
		if wall[i] != 0 || t.Mono <= 0 || t.Host == "" || out[i].Basis == "monotonic" {
			continue
		}
		m0 := math.Inf(1)
		for j, u := range ts {
			if wall[j] == 0 && u.Host == t.Host && u.Mono > 0 {
				m0 = math.Min(m0, u.Mono+u.Start)
			}
		}
		for j, u := range ts {
			if wall[j] == 0 && u.Host == t.Host && u.Mono > 0 {
				out[j] = Out{Offset: u.Mono + u.Start - m0, Basis: "monotonic"}
			}
		}
	}
	return out
}

// Collective alignment (mode "collectives", the default): the ranks of a
// job run the same collective kernels (all-reduce, all-gather, ...), which
// end together on all of them, to microseconds, where hosts' clocks agree
// to milliseconds at best. Session places each trace by its clocks
// (Traces), then moves it so that its collectives' ends meet the
// reference trace's (the first with enough of them): it takes the most
// common difference of the two traces' ends of the same operation (100 µs
// bins), within 20 ms of their clocks' offset where their clocks are
// comparable (which picks the right step of a job whose steps look alike,
// and tolerates clocks milliseconds off), or over all differences where
// they are not (then a job of identical steps can line up a step off);
// then it pairs each of the reference's ends with the trace's nearest and
// takes the median of the differences, within 5 ms, then 1 ms, then
// 200 µs. A trace with too few matches (under 8, or a fifth of the fewer
// collectives) keeps its clocks' place.

const (
	collMin    = 8
	collBin    = 100e3
	collSample = 400
	collNear   = 20e6
)

var collWindows = []float64{5e6, 1e6, 200e3}

func Session(ts []In, mode string) []Out {
	ts = clockHosts(ts)
	base := "abs"
	if mode == "start" {
		base = "start"
	}
	out := Traces(ts, base)
	if mode != "collectives" {
		return out
	}
	total := func(i int) int {
		n := 0
		for _, v := range ts[i].Coll {
			n += len(v)
		}
		return n
	}
	ref := -1
	for i := range ts {
		if total(i) >= collMin {
			ref = i
			break
		}
	}
	if ref < 0 {
		return out
	}
	comparable := func(i int) bool {
		a, b := out[i].Basis, out[ref].Basis
		wallish := func(x string) bool { return x == "wall" || x == "host" }
		return (wallish(a) && wallish(b)) || (a == "monotonic" && b == "monotonic" && ts[i].Host == ts[ref].Host)
	}
	refOff := out[ref].Offset
	any := false
	for i := range ts {
		if i == ref || total(i) < collMin {
			continue
		}
		off, n, spread, ok := matchCollectives(ts[ref].Coll, ts[i].Coll, out[i].Offset-refOff, comparable(i))
		if ok {
			out[i] = Out{Offset: refOff + off, Basis: "collectives", Matches: n, Spread: spread}
			any = true
		}
	}
	if any {
		out[ref] = Out{Offset: refOff, Basis: "collectives", Ref: true}
	}
	lo := math.Inf(1)
	for _, o := range out {
		lo = math.Min(lo, o.Offset)
	}
	for i := range out {
		out[i].Offset -= lo
	}
	return out
}

// nearestIn is the index of the value of sorted v nearest x.
func nearestIn(v []float64, x float64) int {
	i, _ := slices.BinarySearch(v, x)
	if i == len(v) || (i > 0 && x-v[i-1] <= v[i]-x) {
		i--
	}
	return i
}

// collOps are the operations both traces have, sorted.
func collOps(a, b map[string][]float64) []string {
	var ops []string
	for op, v := range a {
		if len(v) > 0 && len(b[op]) > 0 {
			ops = append(ops, op)
		}
	}
	slices.Sort(ops)
	return ops
}

// collDiffs are the differences between a's ends and b's nearest, moved by
// off, of those within w.
func collDiffs(a, b map[string][]float64, ops []string, off, w float64) []float64 {
	var d []float64
	for _, op := range ops {
		bs := b[op]
		for _, x := range a[op] {
			y := bs[nearestIn(bs, x-off)] + off
			if diff := x - y; math.Abs(diff) < w {
				d = append(d, diff)
			}
		}
	}
	slices.Sort(d)
	return d
}

// matchCollectives finds how far to move b's times so that its collectives
// end with a's; ok if enough match.
func matchCollectives(a, b map[string][]float64, guess float64, haveGuess bool) (off float64, matches int, spread float64, ok bool) {
	ops := collOps(a, b)
	if len(ops) == 0 {
		return 0, 0, 0, false
	}
	na, nb := 0, 0
	for _, op := range ops {
		na += len(a[op])
		nb += len(b[op])
	}
	need := max(collMin, int(math.Ceil(0.2*float64(min(na, nb)))))
	{
		// The most common difference of their ends (near the clocks'
		// offset, if comparable), ties to the smallest.
		hist := map[float64]int{}
		for _, op := range ops {
			as, bs := a[op], b[op]
			for _, x := range as[:min(len(as), collSample)] {
				for _, y := range bs[:min(len(bs), collSample)] {
					if haveGuess && math.Abs(x-y-guess) > collNear {
						continue
					}
					hist[math.Floor((x-y)/collBin)]++
				}
			}
		}
		if len(hist) == 0 {
			return 0, 0, 0, false
		}
		bins := make([]float64, 0, len(hist))
		for k := range hist {
			bins = append(bins, k)
		}
		slices.Sort(bins)
		best, bestN := 0.0, -1
		for _, k := range bins {
			if hist[k] > bestN {
				best, bestN = k, hist[k]
			}
		}
		off = (best + 0.5) * collBin
	}
	for _, w := range collWindows {
		d := collDiffs(a, b, ops, off, w)
		if len(d) == 0 {
			return 0, 0, 0, false
		}
		off += d[len(d)/2]
	}
	d := collDiffs(a, b, ops, off, collWindows[len(collWindows)-1])
	if len(d) < need {
		return 0, 0, 0, false
	}
	abs := make([]float64, len(d))
	for i, v := range d {
		abs[i] = math.Abs(v)
	}
	slices.Sort(abs)
	return off, len(d), abs[min(len(abs)-1, int(0.9*float64(len(abs))))], true
}
