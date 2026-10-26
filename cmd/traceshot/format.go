package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// parseTime reads a time or duration: a number with a unit (s, ms, us or
// µs, ns), seconds without one.
func parseTime(s string) (int64, error) {
	s = strings.TrimSpace(s)
	unit := 1e9
	num := s
	for _, u := range []struct {
		suffix string
		ns     float64
	}{{"ms", 1e6}, {"us", 1e3}, {"µs", 1e3}, {"ns", 1}, {"s", 1e9}} {
		if strings.HasSuffix(s, u.suffix) {
			unit, num = u.ns, strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%q is not a time (1.25s, 300ms, 40us, 500ns; seconds without a unit)", s)
	}
	return int64(math.Round(v * unit)), nil
}

// parseWindow reads a window of the session's time axis: "START:END" or
// "START+DURATION" (either end may be left out: ":2s", "1s:").
func parseWindow(s string, span int64) (t0, t1 int64, err error) {
	if a, d, ok := strings.Cut(s, "+"); ok && a != "" {
		if t0, err = parseTime(a); err != nil {
			return
		}
		var dur int64
		if dur, err = parseTime(d); err != nil {
			return
		}
		return t0, t0 + dur, nil
	}
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("-window %q: give START:END or START+DURATION (1.2s:1.35s, 1.2s+150ms)", s)
	}
	t0, t1 = 0, span
	if strings.TrimSpace(a) != "" {
		if t0, err = parseTime(a); err != nil {
			return
		}
	}
	if strings.TrimSpace(b) != "" {
		if t1, err = parseTime(b); err != nil {
			return
		}
	}
	if t1 <= t0 {
		return 0, 0, fmt.Errorf("-window %q: it ends before it starts", s)
	}
	return t0, t1, nil
}

// fmtDur formats a duration of ns nanoseconds: "1.25 ms".
func fmtDur(ns float64) string {
	a := math.Abs(ns)
	switch {
	case a < 1e3:
		return trimFloat(ns, 0) + " ns"
	case a < 1e6:
		return trimFloat(ns/1e3, 3) + " µs"
	case a < 1e9:
		return trimFloat(ns/1e6, 3) + " ms"
	}
	return trimFloat(ns/1e9, 3) + " s"
}

// fmtArg formats a time as -window takes it, to the nanosecond: "1.2s",
// "23.054796ms".
func fmtArg(ns int64) string {
	if ns%1_000_000 == 0 || ns >= 1e9 {
		return strconv.FormatFloat(float64(ns)/1e9, 'f', -1, 64) + "s"
	}
	return strconv.FormatFloat(float64(ns)/1e6, 'f', -1, 64) + "ms"
}

// trimFloat formats v with up to decimals decimals (fewer as its integer
// part grows), without trailing zeros.
func trimFloat(v float64, decimals int) string {
	if decimals == 0 {
		return fmt.Sprintf("%.0f", v)
	}
	switch a := math.Abs(v); {
	case a >= 100:
		decimals = max(decimals-2, 1)
	case a >= 10:
		decimals = max(decimals-1, 1)
	}
	s := fmt.Sprintf("%.*f", decimals, v)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// fmtCount formats a count with thousands separators from 10,000 on.
func fmtCount(n int) string {
	s := strconv.Itoa(n)
	if n < 10000 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tickStep is the spacing of ruler ticks, 1, 2 or 5 times a power of ten
// nanoseconds, at least minPx pixels apart.
func tickStep(nsPerPx, minPx float64) float64 {
	raw := nsPerPx * minPx
	if raw <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if p*m >= raw {
			return max(p*m, 1)
		}
	}
	return max(p*10, 1)
}

// fmtTick formats a ruler tick with the decimals its step needs.
func fmtTick(ns, step float64) string {
	unit, name := 1.0, "ns"
	switch {
	case step >= 1e8:
		unit, name = 1e9, "s"
	case step >= 1e6 && math.Abs(ns) >= 1e9: // 12.25 s, not 12250 ms
		unit, name = 1e9, "s"
	case step >= 1e5:
		unit, name = 1e6, "ms"
	case step >= 1e2:
		unit, name = 1e3, "µs"
	}
	digits := 0
	for s := step / unit; s < 1 && digits < 9; s *= 10 {
		digits++
	}
	return strconv.FormatFloat(ns/unit, 'f', digits, 64) + " " + name
}

// fmtSpan formats a window: "1.2 s – 1.35 s (150 ms)".
func fmtSpan(t0, t1 int64) string {
	step := tickStep(float64(t1-t0)/20, 1)
	return fmtTick(float64(t0), step) + " – " + fmtTick(float64(t1), step) + "  (" + fmtDur(float64(t1-t0)) + ")"
}
