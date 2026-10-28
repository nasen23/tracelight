package trace

import (
	"io"
	"math"
	"sync/atomic"
)

// Progress is how far a load has got, written by the goroutines that load
// and read by any (a window drawing it, a server answering /api/progress).
// The zero value is a load that has not started; a nil *Progress takes
// reports and drops them, so readers report unconditionally.
type Progress struct {
	phase       atomic.Int32
	done, total atomic.Int64
	err         atomic.Pointer[error]
	// follow is the progress of a load this one waits for (the ranks of a
	// merged file wait for the file's one read): it says how far that is.
	follow atomic.Pointer[Progress]
	// fixed, if not 0, is the fraction someone else worked out, plus one,
	// as bits (SetFraction: a server's load, as its client mirrors it).
	fixed atomic.Uint64
}

// Phase is the stage a load is at.
type Phase int32

const (
	// PhaseQueued waits for its turn (a server loads a few traces at once).
	PhaseQueued Phase = iota
	// PhaseOpening reads the file's index or schema.
	PhaseOpening
	// PhaseReading reads the events: Fraction says how far.
	PhaseReading
	// PhaseConverting converts a report with nsys export, which says
	// nothing of how far it is.
	PhaseConverting
	// PhaseBuilding merges and packs the events read into rows.
	PhaseBuilding
	PhaseDone
	PhaseFailed
)

var phaseNames = [...]string{"queued", "opening", "reading", "converting", "building", "done", "failed"}

func (ph Phase) String() string {
	if ph >= 0 && int(ph) < len(phaseNames) {
		return phaseNames[ph]
	}
	return "queued"
}

// ParsePhase is the phase a name says (Phase.String), PhaseQueued for
// anything else.
func ParsePhase(s string) Phase {
	for i, n := range phaseNames {
		if n == s {
			return Phase(i)
		}
	}
	return PhaseQueued
}

// Report sets the phase and, where total is more than 0, how much of it is
// done (rows, frames, bytes: whatever the reader counts).
func (p *Progress) Report(ph Phase, done, total int64) {
	if p == nil {
		return
	}
	if total > 0 {
		p.total.Store(total)
		p.done.Store(done)
	}
	p.phase.Store(int32(ph))
}

// SetFraction sets the phase and the fraction a load elsewhere reports (a
// server's, as /api/progress says): Fraction says it as it is.
func (p *Progress) SetFraction(ph Phase, f float64) {
	if p == nil {
		return
	}
	p.fixed.Store(math.Float64bits(min(max(f, 0), 1) + 1))
	p.phase.Store(int32(ph))
}

// Add counts n more done of the total.
func (p *Progress) Add(n int64) {
	if p != nil {
		p.done.Add(n)
	}
}

// Finish ends the load: done, or failed with err.
func (p *Progress) Finish(err error) {
	if p == nil {
		return
	}
	if err != nil {
		p.err.Store(&err)
		p.phase.Store(int32(PhaseFailed))
		return
	}
	p.done.Store(p.total.Load())
	p.phase.Store(int32(PhaseDone))
}

// Follow makes p say how far q is, until p reports again past it: a load
// that waits for another's (the ranks of a merged file, read once).
func (p *Progress) Follow(q *Progress) {
	if p != nil && q != p {
		p.follow.Store(q)
	}
}

func (p *Progress) source() *Progress {
	for range 4 { // a follows b follows c: as far as that goes
		q := p.follow.Load()
		if q == nil || Phase(p.phase.Load()) >= PhaseReading {
			return p
		}
		p = q
	}
	return p
}

// Phase is the stage the load is at.
func (p *Progress) Phase() Phase {
	if p == nil {
		return PhaseQueued
	}
	return Phase(p.source().phase.Load())
}

// Err is why the load failed, or nil.
func (p *Progress) Err() error {
	if p == nil {
		return nil
	}
	if e := p.err.Load(); e != nil {
		return *e
	}
	return nil
}

// Fraction is how far the load is, from 0 to 1: reading the events is
// most of a load (5% to 90%), merging and packing them the rest.
func (p *Progress) Fraction() float64 {
	if p == nil {
		return 0
	}
	q := p.source()
	ph := Phase(q.phase.Load())
	if b := q.fixed.Load(); b != 0 && ph != PhaseDone {
		return math.Float64frombits(b) - 1
	}
	switch ph {
	case PhaseOpening:
		return 0.02
	case PhaseReading:
		if t := q.total.Load(); t > 0 {
			return 0.05 + 0.85*min(float64(q.done.Load())/float64(t), 1)
		}
		return 0.05
	case PhaseConverting:
		return 0.03
	case PhaseBuilding:
		return 0.92
	case PhaseDone:
		return 1
	}
	return 0
}

// countingReader counts the bytes read through it into a progress.
type countingReader struct {
	r io.Reader
	p *Progress
}

func (c countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.p.Add(int64(n))
	return n, err
}
