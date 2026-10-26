package viewer

import (
	"math"
	"slices"
	"testing"
)

// Each trace at the moment its first event happened: by its wall clock; a
// report without one through its host's monotonic clock and a trace of the
// same host that has one; a host with no wall clock among its own traces;
// no clock at all at 0. "start": all at 0.
func TestAlignTraces(t *testing.T) {
	const utc0, mono0 = 1_792_411_200_000_000_000.0, 1_515_000_000_000_000.0
	ts := []alignIn{
		{UTC: utc0, Mono: mono0, Host: "a", Start: 1e6},         // its own wall clock
		{Mono: mono0 + 123_456, Host: "a", Start: 2e6},          // a report, on host a
		{UTC: utc0 - 300e6, Mono: 4e15, Host: "b", Start: 50e6}, // started earlier, another host
		{Mono: 3e15 + 7e6, Host: "c", Start: 1e6},               // reports of host c, no wall clock
		{Mono: 3e15, Host: "c", Start: 5e6},
		{Host: "d", Start: 9e6}, // nothing
	}
	got := alignTraces(ts, "abs")
	// The earliest first event: trace 2's, at utc0 - 300 ms + 50 ms; exact,
	// though nanoseconds since 1970 are not in float64.
	want := []alignOut{
		{Offset: 251e6, Basis: "wall"},
		{Offset: 252_123_456, Basis: "host"},
		{Offset: 0, Basis: "wall"},
		{Offset: 3e6, Basis: "monotonic"}, // 3e15+8e6 against host c's earliest, 3e15+5e6
		{Offset: 0, Basis: "monotonic"},
		{Offset: 0, Basis: "start"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("abs:\n got  %+v\n want %+v", got, want)
	}
	for _, a := range alignTraces(ts, "start") {
		if a != (alignOut{Offset: 0, Basis: "start"}) {
			t.Errorf("start: %+v", a)
		}
	}
}

// syntheticColl is one job's collective ends, as a rank whose first event
// came shift ns after the job's start sees them, over collectives
// [from, to), each off by up to ±20 µs.
func syntheticColl(shift float64, from, to, seed int) map[string][]float64 {
	m := map[string][]float64{}
	t := 0.0
	for k := 0; k < to; k++ {
		t += 2e6 + float64((k*k*7919+k*31)%97)*1.7e4
		if k < from {
			continue
		}
		op := "allreduce"
		if k%5 == 0 {
			op = "allgather"
		}
		m[op] = append(m[op], t-shift+float64(((k+seed)*104729)%41-20)*1e3)
	}
	return m
}

// Ranks whose clocks are milliseconds off, or missing, line up on their
// collectives to the microseconds of their jitter; a rank with too few
// keeps its clocks' place.
func TestAlignCollectives(t *testing.T) {
	// True first-event times: 0, 480 ms, 380 ms, 311 ms after the job's.
	ts := []alignIn{
		{UTC: 1, Host: "a", Coll: syntheticColl(0, 0, 120, 0)},
		{UTC: 1 + 480e6 + 2.3e6, Host: "b", Coll: syntheticColl(480e6, 10, 130, 3)},      // clock 2.3 ms fast
		{UTC: 1 + 380e6 - 0.5e6, Host: "c", Coll: syntheticColl(380e6, 4, 110, 5)},       // 0.5 ms slow
		{Host: "d", Coll: syntheticColl(311e6, 0, 125, 7)},                               // no clock
		{UTC: 1 + 100e6, Host: "e", Coll: map[string][]float64{"allreduce": {1e6, 2e6}}}, // too few
	}
	clocks := alignSession(ts, "abs")
	got := alignSession(ts, "collectives")
	want := []float64{0, 480e6, 380e6, 311e6}
	for i, w := range want {
		if d := math.Abs(got[i].Offset - w); d > 25e3 || got[i].Basis != "collectives" {
			t.Errorf("rank %d: %.1f us (%s), want %.1f us", i, got[i].Offset/1e3, got[i].Basis, w/1e3)
		}
	}
	if !got[0].Ref || got[1].Matches < 100 || got[1].Spread > 40e3 {
		t.Errorf("reference %v, matches %d, spread %.1f us", got[0].Ref, got[1].Matches, got[1].Spread/1e3)
	}
	if got[4].Basis != "wall" || got[4].Offset != clocks[4].Offset {
		t.Errorf("a rank with too few collectives: %+v (clocks: %+v)", got[4], clocks[4])
	}
	// The clocks alone are milliseconds off; the rank without one, at 0.
	if d := math.Abs(clocks[1].Offset - 482.3e6); d > 1 || clocks[3].Basis != "start" {
		t.Errorf("clocks: %+v", clocks)
	}
}

// Ranks merged into one file without a wall clock (times from boot) line
// up on the file's clock they share, as traces of one host do on its
// monotonic clock; a trace of another file's clock stays at its start.
func TestAlignClock(t *testing.T) {
	ts := []alignIn{{Mono: 1.95e15, Clock: "/r/job.json.gz", Start: 6e6}, {Mono: 1.95e15, Clock: "/r/job.json.gz", Start: 1e6}, {Mono: 5e12, Start: 3e6}}
	out := alignTraces(ts, "abs")
	if out[0].Basis != "monotonic" || out[1].Basis != "monotonic" || out[0].Offset-out[1].Offset != 5e6 || out[2].Basis != "start" {
		t.Errorf("aligned %+v", out)
	}
}
