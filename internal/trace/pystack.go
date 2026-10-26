package trace

import (
	"cmp"
	"slices"
)

// StackFrame is a frame of a Python stack: its function, as PyTorch's
// profiler names it ("train.py(61): train_step"), and its call's event.
type StackFrame struct {
	Name string
	Ref  Ref
}

// PythonStack returns the Python stack an event ran under, the outermost
// frame first (as a traceback lists them): the Python calls of its thread
// around its start, for an event of a thread (a CUDA API call, an operator,
// a range; a Python call's own frame last); for GPU work, those around the
// CUDA API call that launched it (by correlation id). Nil if the thread has
// no Python calls there (PyTorch records them with with_stack=True).
func (t *Trace) PythonStack(r Ref) []StackFrame {
	tk := t.Tracks[r.Track]
	if tk.Kind == TrackStream {
		api, ok := t.Correlated(r)
		if !ok {
			return nil
		}
		r, tk = api, t.Tracks[api.Track]
	}
	at := t.Event(r).Start
	t.pyOnce.Do(func() {
		t.pyTracks = map[int64]int{}
		for i, tr := range t.Tracks {
			if tr.Kind == TrackPython {
				t.pyTracks[tr.Thread] = i
			}
		}
	})
	pi, ok := t.pyTracks[tk.Thread]
	if !ok || tk.Thread == 0 && tk.Kind != TrackPython {
		return nil
	}
	py := t.Tracks[pi]
	lanes := len(py.Lanes)
	if tk.Kind == TrackPython {
		lanes = int(r.Lane) // the frames around the call; it comes last
	}
	// A lane is a depth of the stack (calls nest; Pack puts a call below
	// its caller's lane). A lane with no call around the time is skipped,
	// not taken as the end: a tracer's calls may not nest exactly.
	var out []StackFrame
	for li := range lanes {
		l := py.Lanes[li]
		i := l.FirstEnding(at)
		if i < len(l.Events) && l.Events[i].Start <= at {
			e := &l.Events[i]
			out = append(out, StackFrame{t.Name(e), Ref{Track: int32(pi), Lane: int32(li), Index: int32(i)}})
		}
	}
	if tk.Kind == TrackPython {
		out = append(out, StackFrame{t.Name(t.Event(r)), r})
	}
	return out
}

// OperatorStack returns the operators (PyTorch's cpu_op: aten::linear >
// aten::addmm) and annotations (record_function, ProfilerStep#N) an event
// ran under on its thread, the outermost first: those around its start
// for a CUDA API call or a Python call, around the CUDA call that launched
// it for GPU work, and around itself, which comes last, for an operator or
// an annotation. Nil if there are none (a trace without operators).
func (t *Trace) OperatorStack(r Ref) []StackFrame {
	tk := t.Tracks[r.Track]
	if tk.Kind == TrackStream {
		api, ok := t.Correlated(r)
		if !ok {
			return nil
		}
		r, tk = api, t.Tracks[api.Track]
	}
	self := tk.Kind == TrackCPU || tk.Kind == TrackNVTX
	e := t.Event(r)
	t.thOnce.Do(func() {
		t.thTracks = map[int64][]int{}
		for i, tr := range t.Tracks {
			if tr.Kind == TrackCPU || tr.Kind == TrackNVTX {
				t.thTracks[tr.Thread] = append(t.thTracks[tr.Thread], i)
			}
		}
	})
	type frame struct {
		f    StackFrame
		s, e int64
	}
	var fs []frame
	for _, ti := range t.thTracks[tk.Thread] {
		for li, l := range t.Tracks[ti].Lanes {
			i := l.FirstEnding(e.Start)
			for ; i < len(l.Events) && l.Events[i].Start <= e.Start; i++ {
				f := &l.Events[i]
				ref := Ref{Track: int32(ti), Lane: int32(li), Index: int32(i)}
				switch {
				case ref == r:
				case f.End <= f.Start: // a mark
					continue
				case self && (f.End < e.End || f.Start == e.Start && f.End == e.End && ref.Lane > r.Lane):
					continue // not around it
				}
				fs = append(fs, frame{StackFrame{t.Name(f), ref}, f.Start, f.End})
			}
		}
	}
	slices.SortStableFunc(fs, func(a, b frame) int {
		return cmp.Or(cmp.Compare(a.s, b.s), cmp.Compare(b.e, a.e), cmp.Compare(a.f.Ref.Lane, b.f.Ref.Lane))
	})
	out := make([]StackFrame, 0, len(fs))
	for _, f := range fs {
		if f.f.Ref == r {
			continue
		}
		out = append(out, f.f)
	}
	if self {
		out = append(out, StackFrame{t.Name(e), r})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
