package trace

import (
	"bufio"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
)

// A snapshot is a loaded trace written compactly, for a server to send to
// a desktop client: the parsed model, not the SQLite file, so the client
// needs neither the export nor nsys. Metadata is gob; events are varints,
// starts and correlation ids as deltas, which compress well (callers
// gzip it).

const snapshotMagic = "NSALT-SNAPSHOT-1\n"

type snapshotMeta struct {
	Path                  string
	Names                 []string
	Start, End            int64
	Kernels               []KernelInfo
	MemOps                []MemOpInfo
	Devices               map[int32]string
	ThreadNames           map[int64]string
	SessionStartUTC       int64
	SessionStartMono      int64
	Host                  string
	Rank                  int
	Clock                 string
	ProcessNames          map[int64]string
	Tables, Missing, Warn []string
	Counts                map[Kind]int
	Tracks                []snapshotTrack
}

type snapshotTrack struct {
	Group, Name string
	Kind        TrackKind
	Device      int32
	Thread      int64
	Count       int
	Busy        int64
	Lanes       []int // events in each lane
}

// WriteSnapshot writes t to w.
func (t *Trace) WriteSnapshot(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	if _, err := bw.WriteString(snapshotMagic); err != nil {
		return err
	}
	m := snapshotMeta{
		Path: t.Path, Names: t.Names, Start: t.Start, End: t.End,
		SessionStartUTC: t.SessionStartUTC, SessionStartMono: t.SessionStartMono, Host: t.Host, Rank: t.Rank, Clock: t.Clock,
		Kernels: t.Kernels, MemOps: t.MemOps, Devices: t.Devices,
		ThreadNames: t.ThreadNames, ProcessNames: t.ProcessNames,
		Tables: t.Tables, Missing: t.Missing, Warn: t.Warnings, Counts: t.Counts,
	}
	for _, tr := range t.Tracks {
		st := snapshotTrack{Group: tr.Group, Name: tr.Name, Kind: tr.Kind, Device: tr.Device, Thread: tr.Thread, Count: tr.Count, Busy: tr.Busy}
		for _, l := range tr.Lanes {
			st.Lanes = append(st.Lanes, len(l.Events))
		}
		m.Tracks = append(m.Tracks, st)
	}
	if err := gob.NewEncoder(bw).Encode(&m); err != nil {
		return err
	}
	var buf [binary.MaxVarintLen64]byte
	put := func(v int64) error {
		_, err := bw.Write(buf[:binary.PutVarint(buf[:], v)])
		return err
	}
	for _, tr := range t.Tracks {
		for _, l := range tr.Lanes {
			var start, corr int64
			for i := range l.Events {
				e := &l.Events[i]
				for _, v := range [...]int64{e.Start - start, e.End - e.Start, int64(e.Name), int64(e.Corr) - corr, int64(e.Extra), int64(e.Kind)} {
					if err := put(v); err != nil {
						return err
					}
				}
				start, corr = e.Start, int64(e.Corr)
			}
		}
	}
	return bw.Flush()
}

// ReadSnapshot reads a trace that WriteSnapshot wrote.
func ReadSnapshot(r io.Reader) (*Trace, error) {
	br := bufio.NewReaderSize(r, 1<<16) // an io.ByteReader, so gob reads no further than its own
	magic := make([]byte, len(snapshotMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != snapshotMagic {
		return nil, errors.New("not a trace snapshot")
	}
	var m snapshotMeta
	if err := gob.NewDecoder(br).Decode(&m); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	t := &Trace{
		Path: m.Path, Names: m.Names, Start: m.Start, End: m.End, SessionStartUTC: m.SessionStartUTC, SessionStartMono: m.SessionStartMono, Host: m.Host, Rank: m.Rank, Clock: m.Clock,
		Kernels: m.Kernels, MemOps: m.MemOps, Devices: m.Devices,
		ThreadNames: m.ThreadNames, ProcessNames: m.ProcessNames,
		Tables: m.Tables, Missing: m.Missing, Warnings: m.Warn, Counts: m.Counts,
	}
	if t.Devices == nil {
		t.Devices = map[int32]string{}
	}
	if t.ThreadNames == nil {
		t.ThreadNames = map[int64]string{}
	}
	if t.ProcessNames == nil {
		t.ProcessNames = map[int64]string{}
	}
	if t.Counts == nil {
		t.Counts = map[Kind]int{}
	}
	get := func() (int64, error) { return binary.ReadVarint(br) }
	for _, st := range m.Tracks {
		tr := &Track{Group: st.Group, Name: st.Name, Kind: st.Kind, Device: st.Device, Thread: st.Thread, Count: st.Count, Busy: st.Busy}
		for _, n := range st.Lanes {
			if n < 0 || n > 1<<31 {
				return nil, errors.New("snapshot: bad lane length")
			}
			events := make([]Event, n)
			var start, corr int64
			for i := range events {
				var v [6]int64
				for k := range v {
					x, err := get()
					if err != nil {
						return nil, fmt.Errorf("snapshot: %w", err)
					}
					v[k] = x
				}
				start += v[0]
				corr += v[3]
				if v[2] < 0 || v[2] >= int64(len(t.Names)) {
					return nil, errors.New("snapshot: bad name index")
				}
				events[i] = Event{Start: start, End: start + v[1], Name: int32(v[2]), Corr: int32(corr), Extra: int32(v[4]), Kind: Kind(v[5])}
				switch k := events[i].Kind; {
				case k == KindKernel && (v[4] < 0 || v[4] >= int64(len(t.Kernels))),
					(k == KindMemcpy || k == KindMemset) && (v[4] < 0 || v[4] >= int64(len(t.MemOps))):
					return nil, errors.New("snapshot: bad event detail index")
				}
			}
			tr.Lanes = append(tr.Lanes, NewLane(events))
		}
		t.Tracks = append(t.Tracks, tr)
	}
	return t, nil
}
