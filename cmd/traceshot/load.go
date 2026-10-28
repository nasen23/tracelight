package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"tracelight/internal/align"
	"tracelight/internal/trace"
)

// rank is a trace of the session: one rank's file, or one rank of a merged
// file.
type rank struct {
	path  string
	label string // the file's name without its ending: "rank3"
	num   int    // the rank's number: in its label, else what the trace says, else its place
	tr    *trace.Trace
	// offset is where the trace's first event (tr.Start) is on the
	// session's time axis, which starts at 0 with the earliest rank.
	offset int64
	basis  string // what the offset rests on (align.Out.Basis)
}

// toTrace converts a time on the session's axis into the trace's own.
func (r *rank) toTrace(t int64) int64 { return t - r.offset + r.tr.Start }

// toSession converts a time of the trace into the session's axis.
func (r *rank) toSession(t int64) int64 { return t - r.tr.Start + r.offset }

// session is the ranks shown, on one time axis.
type session struct {
	name  string // the first input's name, for the title
	ranks []*rank
	span  int64 // the end of the last rank on the axis
	mode  string
}

// traceExts are the endings of trace files' names, the longest first.
var traceExts = []string{".sqlite", ".nsys-rep", ".qdrep", ".pt.trace.json.gz", ".pt.trace.json", ".json.gz", ".json"}

// baseName is the last element of a path without a trace file's ending
// ("rank0" of "/runs/rank0.pt.trace.json"; "rank8" of a merged file's
// "job.json#rank8").
func baseName(p string) string {
	if i := strings.LastIndex(p, "#rank"); i >= 0 && i+5 < len(p) && strings.Trim(p[i+5:], "0123456789") == "" {
		return "rank" + p[i+5:]
	}
	s := strings.TrimRight(p, "/\\")
	b := s[strings.LastIndexAny(s, "/\\")+1:]
	if b == "" {
		b = p
	}
	for _, ext := range traceExts {
		if len(b) > len(ext) && strings.EqualFold(b[len(b)-len(ext):], ext) {
			return b[:len(b)-len(ext)]
		}
	}
	return b
}

var trailingNum = regexp.MustCompile(`(\d+)$`)

// labelNum is the number a label ends with ("rank12": 12), or -1.
func labelNum(label string) int {
	m := trailingNum.FindString(label)
	if m == "" {
		return -1
	}
	n, err := strconv.Atoi(m)
	if err != nil {
		return -1
	}
	return n
}

// expand lists the traces the inputs name: files, directories of them
// (not recursively), each rank of a merged PyTorch file, and brace
// patterns of them, quoted from the shell (rank{0..7}.sqlite).
func expand(o trace.OpenOptions, inputs []string) ([]string, error) {
	var braced []string
	for _, in := range inputs {
		ps, err := trace.ExpandBraces(in, nil)
		if err != nil {
			return nil, err
		}
		braced = append(braced, ps...)
	}
	var out []string
	for _, in := range braced {
		fi, err := os.Stat(in)
		if _, _, member := trace.SplitMember(in); err != nil && !member {
			return nil, err
		}
		files := []string{in}
		if err == nil && fi.IsDir() {
			if files, err = o.DirTraces(in); err != nil {
				return nil, err
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("%s: no traces in it", in)
			}
		}
		for _, f := range files {
			out = append(out, trace.Members(f)...)
		}
	}
	return out, nil
}

// rankSet is a selection of ranks: "0,2,4-7"; nil selects all.
type rankSet [][2]int

func parseRanks(s string) (rankSet, error) {
	if strings.TrimSpace(s) == "" || s == "all" {
		return nil, nil
	}
	var set rankSet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("-ranks %q: %q is not a rank or a range of them (0,2,4-7)", s, part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("-ranks %q: %q is not a range (4-7)", s, part)
			}
		}
		set = append(set, [2]int{a, b})
	}
	return set, nil
}

func (s rankSet) has(n int) bool {
	if s == nil {
		return true
	}
	for _, r := range s {
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

// load opens the traces the inputs name, those of the ranks selected, in
// parallel, and aligns them on one time axis (mode: "collectives", "abs"
// or "start", as the viewer's).
func load(o trace.OpenOptions, inputs []string, sel rankSet, mode string) (*session, error) {
	files, err := expand(o, inputs)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no traces given")
	}
	ss := &session{name: baseName(inputs[0]), mode: mode}
	if len(inputs) > 1 {
		ss.name += fmt.Sprintf(" +%d", len(inputs)-1)
	}
	// A rank's number is known before loading where its label has one.
	var todo []*rank
	for _, f := range files {
		r := &rank{path: f, label: baseName(f), num: labelNum(baseName(f))}
		if r.num < 0 || sel.has(r.num) {
			todo = append(todo, r)
		}
	}
	errs := make([]error, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, r := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.tr, errs[i] = trace.OpenWith(r.path, o)
		}()
	}
	wg.Wait()
	for i, r := range todo {
		if errs[i] != nil {
			return nil, fmt.Errorf("%s: %w", r.label, errs[i])
		}
		if r.num < 0 {
			r.num = r.tr.Rank
		}
		if r.num < 0 {
			r.num = i
		}
		if sel.has(r.num) {
			ss.ranks = append(ss.ranks, r)
		}
	}
	if len(ss.ranks) == 0 {
		return nil, fmt.Errorf("no trace is of the ranks selected (%d traces)", len(files))
	}
	ss.align()
	return ss, nil
}

// align places each rank on the session's axis as the viewer does
// (package align): by their collectives' ends, their clocks, or at 0.
func (ss *session) align() {
	var ref int64
	for _, r := range ss.ranks {
		if u := r.tr.SessionStartUTC; u > 0 && (ref == 0 || u < ref) {
			ref = u
		}
	}
	in := make([]align.In, len(ss.ranks))
	for i, r := range ss.ranks {
		tr := r.tr
		in[i] = align.In{Mono: float64(tr.SessionStartMono), Host: tr.Host, Clock: tr.Clock, Start: float64(tr.Start), Coll: collOf(tr.Collectives())}
		if tr.SessionStartUTC > 0 {
			in[i].UTC = float64(tr.SessionStartUTC-ref) + 1
		}
	}
	for i, a := range align.Session(in, ss.mode) {
		r := ss.ranks[i]
		r.offset, r.basis = int64(math.Round(a.Offset)), a.Basis
		ss.span = max(ss.span, r.offset+r.tr.Duration())
	}
}

// basis says what the ranks' places rest on, for the subtitle.
func (ss *session) basis() string {
	seen := map[string]bool{}
	var out []string
	for _, r := range ss.ranks {
		if !seen[r.basis] {
			seen[r.basis] = true
			out = append(out, r.basis)
		}
	}
	if len(ss.ranks) == 1 {
		return ""
	}
	return "aligned by " + strings.Join(out, ", ")
}

func collOf(c map[string][]int64) map[string][]float64 {
	if len(c) == 0 {
		return nil
	}
	out := make(map[string][]float64, len(c))
	for op, v := range c {
		f := make([]float64, len(v))
		for i, x := range v {
			f[i] = float64(x)
		}
		out[op] = f
	}
	return out
}
