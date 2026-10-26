package viewer

import (
	"fmt"
	"math"
	"strings"
)

// fmtDur formats a duration in nanoseconds with a unit that keeps three or
// four significant digits.
func fmtDur(ns float64) string {
	a := math.Abs(ns)
	switch {
	case a < 1e3:
		return trimFloat(ns, 0) + " ns"
	case a < 1e6:
		return trimFloat(ns/1e3, 3) + " µs"
	case a < 1e9:
		return trimFloat(ns/1e6, 3) + " ms"
	default:
		return trimFloat(ns/1e9, 3) + " s"
	}
}

// fmtTime formats a time relative to the trace start, as a timestamp with a
// precision fit for a view of nsPerPx nanoseconds a pixel.
func fmtTime(ns, nsPerPx float64) string {
	// digits after the decimal point of the unit chosen, enough to tell
	// apart about 10 pixels.
	step := max(nsPerPx*10, 1)
	unit, name := 1e9, "s"
	switch a := math.Abs(ns); {
	case a < 1e3 && step < 1e3:
		unit, name = 1, "ns"
	case a < 1e6 && step < 1e6:
		unit, name = 1e3, "µs"
	case a < 1e9 && step < 1e9:
		unit, name = 1e6, "ms"
	}
	digits := max(0, int(math.Ceil(-math.Log10(step/unit))))
	digits = min(digits, 9)
	return fmt.Sprintf("%.*f %s", digits, ns/unit, name)
}

// trimFloat formats v with up to `sig` decimals, dropping trailing zeros.
func trimFloat(v float64, decimals int) string {
	if decimals == 0 {
		return fmt.Sprintf("%.0f", v)
	}
	// Fewer decimals as the integer part grows: 1.234, 12.34, 123.4.
	a := math.Abs(v)
	switch {
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

func fmtBytes(b int64) string {
	const k = 1024
	f := float64(b)
	switch {
	case b < k:
		return fmt.Sprintf("%d B", b)
	case b < k*k:
		return trimFloat(f/k, 2) + " KiB"
	case b < k*k*k:
		return trimFloat(f/(k*k), 2) + " MiB"
	default:
		return trimFloat(f/(k*k*k), 2) + " GiB"
	}
}

func fmtCount(n int) string {
	s := fmt.Sprint(n)
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

// tickStep returns the spacing of ruler ticks, a 1, 2 or 5 times a power of
// ten nanoseconds, so that ticks are at least minPx pixels apart.
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

// fmtTick formats a ruler tick at ns from the trace start, with as many
// decimals as the step needs.
func fmtTick(ns, step float64) string {
	unit, name := 1.0, "ns"
	switch {
	case step >= 1e8:
		unit, name = 1e9, "s"
	case step >= 1e5:
		unit, name = 1e6, "ms"
	case step >= 1e2:
		unit, name = 1e3, "µs"
	}
	digits := max(0, int(math.Ceil(-math.Log10(step/unit)-1e-9)))
	return fmt.Sprintf("%.*f %s", digits, ns/unit, name)
}
