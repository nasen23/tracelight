package nsysrep

import (
	"fmt"
	"slices"
)

// StringTable is the report's string table: a string's id is its index.
type StringTable struct {
	buf []byte
	// span holds the start and end in buf of each string.
	span []uint32
}

// Len is the number of strings.
func (t *StringTable) Len() int { return len(t.span) / 2 }

// At returns string id, which must be in [0, Len()).
func (t *StringTable) At(id int) string { return string(t.buf[t.span[2*id]:t.span[2*id+1]]) }

// Strings decodes the string table.
func (f *File) Strings() (*StringTable, error) {
	msg, _, err := f.frames[f.strFrame].decode(nil)
	if err != nil {
		return nil, fmt.Errorf("string table: %w", err)
	}
	if len(msg) > 1<<32-1 {
		return nil, fmt.Errorf("string table: %d bytes", len(msg))
	}
	t := &StringTable{buf: msg}
	r := newReader(msg)
	var e field
	for r.next(&e) {
		if e.num == 1 && e.typ == wireBytes {
			end := uint32(r.i)
			t.span = append(t.span, end-uint32(len(e.bytes)), end)
		}
	}
	if r.err != nil {
		return nil, fmt.Errorf("string table: %w", r.err)
	}
	return t, nil
}

// Thread names a thread (ThreadNames).
type Thread struct {
	GlobalTid int64
	NameID    int64
	Priority  int64
}

// GPUInfo describes a GPU (TARGET_INFO_GPU).
type GPUInfo struct {
	ID   int32
	Name string
}

// Meta is what the frames after the string table describe.
type Meta struct {
	// Threads are in file order, stably sorted by priority, so that a
	// later entry for the same thread overrides an earlier one.
	Threads []Thread
	GPUs    []GPUInfo
}

// Meta decodes the thread names and target information.
func (f *File) Meta() (*Meta, error) {
	m := &Meta{}
	for i := f.strFrame + 1; i < len(f.frames); i++ {
		msg, _, err := f.frames[i].decode(nil)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", i, err)
		}
		switch {
		case isThreadNames(msg):
			if err := m.readThreads(msg); err != nil {
				return nil, fmt.Errorf("thread names: %w", err)
			}
		default:
			if err := m.readTarget(msg); err != nil {
				return nil, fmt.Errorf("target info: %w", err)
			}
		}
	}
	slices.SortStableFunc(m.Threads, func(a, b Thread) int { return int(min(max(a.Priority-b.Priority, -1), 1)) })
	return m, nil
}

// isThreadNames reports whether msg is the thread names list: field 1
// messages of varints 2 (name id), 3 (priority) and 4 (globalTid).
func isThreadNames(msg []byte) bool {
	r := newReader(msg)
	var e, g field
	n := 0
	for r.next(&e) {
		if e.num != 1 || e.typ != wireBytes {
			return false
		}
		er := newReader(e.bytes)
		for er.next(&g) {
			if g.typ != wireVarint || g.num < 2 || g.num > 4 {
				return false
			}
		}
		if er.err != nil {
			return false
		}
		n++
	}
	return r.err == nil && n > 0
}

func (m *Meta) readThreads(msg []byte) error {
	r := newReader(msg)
	var e, g field
	for r.next(&e) {
		var t Thread
		er := newReader(e.bytes)
		for er.next(&g) {
			switch g.num {
			case 2:
				t.NameID = int64(g.v)
			case 3:
				t.Priority = int64(g.v)
			case 4:
				t.GlobalTid = int64(g.v)
			}
		}
		m.Threads = append(m.Threads, t)
	}
	return r.err
}

// readTarget reads the GPUs from the target information (field 1, its
// field 2, repeated field 7 messages), if msg is that; other messages are
// skipped. Messages that are not protobuf are skipped too: frames here are
// not all decoded.
func (m *Meta) readTarget(msg []byte) error {
	r := newReader(msg)
	var e, g, h, k field
	for r.next(&e) {
		if e.num != 1 || e.typ != wireBytes {
			continue
		}
		er := newReader(e.bytes)
		for er.next(&g) {
			if g.num != 2 || g.typ != wireBytes {
				continue
			}
			gr := newReader(g.bytes)
			for gr.next(&h) {
				if h.num != 7 || h.typ != wireBytes {
					continue
				}
				var info GPUInfo
				hasID := false
				hr := newReader(h.bytes)
				for hr.next(&k) {
					switch {
					case k.num == 1 && k.typ == wireVarint:
						info.ID, hasID = int32(k.v), true
					case k.num == 2 && k.typ == wireBytes:
						info.Name = string(k.bytes)
					}
				}
				if hr.err == nil && hasID {
					m.GPUs = append(m.GPUs, info)
				}
			}
		}
	}
	return nil
}
