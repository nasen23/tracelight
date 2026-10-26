package nsysrep

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
)

// Session is when and where a report was captured: the session's start on
// the capturing host's monotonic clock (the export's
// TARGET_INFO_SESSION_START_TIME.systemClockNs; event times count from it)
// and on the wall clock (utcEpochNs), and the host's name.
//
// The wall-clock start is not stored as such: the "Time Conversion Data"
// holds, besides samples of the wall clock against a CPU counter, a reading
// of the wall clock (UTC) and the monotonic clock taken together just
// before the session starts, a group of one sample at rate 1. The wall
// clock at the session's start follows: that reading's UTC plus the time
// from it to the start on the monotonic clock, which is how the export's
// utcEpochNs comes out (to the nanosecond, checked on a report and its
// export).
type Session struct {
	// MonoStart is in nanoseconds of the host's monotonic clock, 0 if the
	// report does not say.
	MonoStart int64
	// WallStart is in nanoseconds since 1970 (UTC), 0 if the report does
	// not say.
	WallStart int64
	Host      string
}

// Plausible values: UTC between 2000 and 2100, a monotonic clock under
// about three years.
const (
	minEpoch = 946_684_800 * int64(1e9)
	maxEpoch = 4_102_444_800 * int64(1e9)
	maxMono  = 100_000_000 * int64(1e9)
)

// wallAnchor finds the reading of the wall clock and the monotonic clock
// taken together in a time conversion section: a group of one sample, at
// rate 1, of a UTC time and a monotonic time. ok is false if there is none.
func wallAnchor(tc []byte, monoStart int64) (wall, mono int64, ok bool) {
	r := newReader(tc)
	var top, g, e, p, q field
	best := int64(math.MaxInt64)
	for r.next(&top) {
		if top.num != 2 || top.typ != wireBytes {
			continue
		}
		rg := newReader(top.bytes)
		for rg.next(&g) {
			if g.typ != wireBytes {
				continue
			}
			n := 0
			var w, m int64
			var rate float64
			re := newReader(g.bytes)
			for re.next(&e) {
				if e.num != 3 || e.typ != wireBytes {
					continue
				}
				n++
				rp := newReader(e.bytes)
				for rp.next(&p) {
					switch {
					case p.num == 1 && p.typ == wireBytes:
						rq := newReader(p.bytes)
						for rq.next(&q) {
							switch {
							case q.num == 1 && q.typ == wireVarint:
								w = int64(q.v)
							case q.num == 2 && q.typ == wireVarint:
								m = int64(q.v)
							}
						}
					case p.num == 2 && p.typ == wireFixed64:
						rate = math.Float64frombits(p.v)
					}
				}
			}
			if n != 1 || rate != 1 || w < minEpoch || w > maxEpoch || m <= 0 || m > maxMono {
				continue
			}
			// The reading nearest the session's start.
			if d := abs64(m - monoStart); !ok || d < best {
				wall, mono, ok, best = w, m, true, d
			}
		}
	}
	return wall, mono, ok
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// sessionOf reads the session from the first frame (field 17: the start on
// the monotonic clock; 2.9: the host) and, for the host, the analysis
// options (field 9).
func sessionOf(frame0, options, tc []byte) Session {
	var s Session
	r := newReader(frame0)
	var e, g field
	for r.next(&e) {
		switch {
		case e.num == 17 && e.typ == wireVarint:
			s.MonoStart = int64(e.v)
		case e.num == 2 && e.typ == wireBytes:
			gr := newReader(e.bytes)
			for gr.next(&g) {
				if g.num == 9 && g.typ == wireBytes && s.Host == "" {
					s.Host = string(g.bytes)
				}
			}
		}
	}
	if s.MonoStart != 0 {
		if w, m, ok := wallAnchor(tc, s.MonoStart); ok {
			s.WallStart = w + (s.MonoStart - m)
		}
	}
	if s.Host == "" {
		r := newReader(options)
		for r.next(&e) {
			if e.num == 9 && e.typ == wireBytes {
				s.Host = string(e.bytes)
				break
			}
		}
	}
	return s
}

// Session returns when and where the report was captured.
func (f *File) Session() Session {
	if len(f.frames) == 0 {
		return Session{}
	}
	msg, _, err := f.frames[0].decode(nil)
	if err != nil {
		return Session{}
	}
	opts, _ := f.section("Analysis Options Data")
	tc, _ := f.section(sectionTime)
	return sessionOf(msg, opts, tc)
}

// ReadSession reads when and where the report at path was captured,
// reading only its header, table of contents, analysis options and first
// frame: cheap enough to list many reports.
func ReadSession(path string) (Session, error) {
	fd, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer fd.Close()
	fi, err := fd.Stat()
	if err != nil {
		return Session{}, err
	}
	size := fi.Size()
	head := make([]byte, min(size, 256))
	if _, err := io.ReadFull(fd, head); err != nil {
		return Session{}, err
	}
	if _, err := ParseVersion(head); err != nil {
		return Session{}, err
	}
	tail := make([]byte, 16)
	if size < 16 {
		return Session{}, fmt.Errorf("%w: no table of contents", ErrNotReport)
	}
	if _, err := fd.ReadAt(tail, size-16); err != nil {
		return Session{}, err
	}
	if string(tail[8:]) != trailer {
		return Session{}, fmt.Errorf("%w: no table of contents", ErrNotReport)
	}
	end := size - 16
	off := int64(le64(tail))
	if off < 0 || off >= end || end-off > 1<<24 {
		return Session{}, fmt.Errorf("%w: table of contents out of the file", errCorrupt)
	}
	toc := make([]byte, end-off)
	if _, err := fd.ReadAt(toc, off); err != nil {
		return Session{}, err
	}
	secs, err := parseTOC(toc, int(end))
	if err != nil {
		return Session{}, err
	}
	read := func(name string, limit int) []byte {
		s, ok := secs[name]
		if !ok {
			return nil
		}
		b := make([]byte, min(s[1], limit))
		n, _ := fd.ReadAt(b, int64(s[0]))
		return b[:n]
	}
	opts := read("Analysis Options Data", 1<<20)
	tc := read(sectionTime, 1<<20)
	raw := read(sectionRaw, 4<<20)
	if !bytes.HasPrefix(raw, []byte(rawMagic)) {
		return sessionOf(nil, opts, tc), nil
	}
	fr, _, err := nextFrame(raw, len(rawMagic))
	if err != nil {
		return sessionOf(nil, opts, tc), nil
	}
	msg, _, err := fr.decode(nil)
	if err != nil {
		return sessionOf(nil, opts, tc), nil
	}
	return sessionOf(msg, opts, tc), nil
}
