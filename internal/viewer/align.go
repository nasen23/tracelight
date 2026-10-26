package viewer

import "tracelight/internal/align"

// Aligning the traces of a session on one time axis: package align, which
// traceshot shares.
type (
	alignIn  = align.In
	alignOut = align.Out
)

func alignTraces(ts []alignIn, mode string) []alignOut  { return align.Traces(ts, mode) }
func alignSession(ts []alignIn, mode string) []alignOut { return align.Session(ts, mode) }
