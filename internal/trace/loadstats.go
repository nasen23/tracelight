package trace

import "time"

// LoadStats says where the time of a load went.
type LoadStats struct {
	// Read is the time from opening the file until every row is decoded
	// and merged; Merge is the merging part of it, Build the time sorting
	// and packing them into lanes.
	Read, Merge, Build time.Duration
	// Tables are the read times of each table, in the order read. Tables
	// read in parallel overlap.
	Tables []TableTime
	// Workers is the number of connections reading at once.
	Workers int
}

// TableTime is how long reading a table took, and how many rows it gave.
type TableTime struct {
	Table string
	Rows  int
	// Time is from the start of the table's first chunk to the end of its
	// last; Chunks is the number of jobs it was split into.
	Time   time.Duration
	Chunks int
}

// Info is what ReadInfo says of a trace file without loading its events.
type Info struct {
	// SessionStartUTC is when the capture started, in nanoseconds since
	// 1970, or 0; SessionStartMono the same on the host's monotonic clock
	// (see Trace); Host the machine's name, if known.
	SessionStartUTC  int64
	SessionStartMono int64
	Host             string
	// Rank is the rank the trace says it is of (PyTorch's distributedInfo),
	// -1 if it does not; Clock is Trace.Clock.
	Rank  int
	Clock string
}
