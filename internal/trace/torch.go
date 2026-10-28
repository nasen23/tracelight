package trace

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PyTorch's profiler (torch.profiler, through Kineto) writes Chrome-trace
// JSON (export_chrome_trace, tensorboard_trace_handler: .json, or .json.gz):
// a "traceEvents" array of events with a phase ("X" for a span), a
// category, a process and a thread, and times in microseconds. LoadTorch
// reads it into the model the nsys readers fill:
//
//	kernel, gpu_memcpy, gpu_memset     GPU work, on its device's stream
//	cuda_runtime, cuda_driver          CUDA API calls of a thread (the GPU
//	                                   work they launched by "correlation")
//	cpu_op (operator)                  operators of a thread (aten::mm, ...)
//	python_function                    Python calls of a thread (with_stack)
//	user_annotation                    record_function ranges and the
//	                                   ProfilerStep#N spans, as NVTX ranges
//
// Other Chrome traces read too: their spans ("X", or "B" and "E") as
// operators of their threads. Flow arrows, counters, and what Kineto
// records of itself are left out (the trace's warnings count them).

// FormatTorch is the Chrome-trace JSON of PyTorch's profiler (.json,
// .json.gz), or any Chrome trace.
const FormatTorch Format = "torch"

// isTorch reports whether the start of a file is a Chrome trace: a JSON
// object with "traceEvents" (Kineto writes its device properties first,
// a few kilobytes), after a gzip layer if there is one.
func isTorch(f io.Reader) bool {
	br := bufio.NewReader(f)
	if head, err := br.Peek(2); err == nil && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return false
		}
		br = bufio.NewReader(zr)
	}
	head := make([]byte, 256<<10)
	n, _ := io.ReadFull(br, head)
	head = bytes.TrimLeft(head[:n], " \t\r\n\ufeff")
	return len(head) > 0 && head[0] == '{' && bytes.Contains(head, []byte(`"traceEvents"`))
}

// torchReader opens a Chrome trace, through gzip if it is compressed.
func torchReader(path string, p *Progress) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	var src io.Reader = f
	if p != nil {
		// How far: the file's bytes read, compressed or not.
		if fi, err := f.Stat(); err == nil {
			p.Report(PhaseReading, 0, fi.Size())
		}
		src = countingReader{f, p}
	}
	br := bufio.NewReaderSize(src, 1<<20)
	if head, err := br.Peek(2); err == nil && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		// Decompressed by a goroutine of its own, ahead of the reading.
		ar := newAsyncReader(zr)
		return bufio.NewReaderSize(ar, 1<<20), func() { ar.Close(); f.Close() }, nil
	}
	return br, func() { f.Close() }, nil
}

// asyncReader reads r in a goroutine of its own, a few chunks ahead of its
// reader: decompressing overlaps with scanning.
type asyncReader struct {
	chunks chan []byte
	free   chan []byte
	buf    []byte // the chunk being read
	cur    []byte // what is left of it
	err    error
	errc   chan error
	stop   chan struct{}
	once   sync.Once
}

func newAsyncReader(r io.Reader) *asyncReader {
	a := &asyncReader{chunks: make(chan []byte, 4), free: make(chan []byte, 6), errc: make(chan error, 1), stop: make(chan struct{})}
	for range 6 {
		a.free <- make([]byte, 1<<20)
	}
	go func() {
		defer close(a.chunks)
		for {
			var buf []byte
			select {
			case buf = <-a.free:
			case <-a.stop:
				return
			}
			n, err := io.ReadFull(r, buf[:cap(buf)])
			if n > 0 {
				select {
				case a.chunks <- buf[:n]:
				case <-a.stop:
					return
				}
			}
			if err != nil {
				if err == io.ErrUnexpectedEOF {
					err = io.EOF
				}
				a.errc <- err
				return
			}
		}
	}()
	return a
}

func (a *asyncReader) Read(p []byte) (int, error) {
	for len(a.cur) == 0 {
		if a.err != nil {
			return 0, a.err
		}
		c, ok := <-a.chunks
		if !ok {
			a.err = <-a.errc
			return 0, a.err
		}
		a.buf, a.cur = c, c
	}
	n := copy(p, a.cur)
	a.cur = a.cur[n:]
	if len(a.cur) == 0 {
		a.free <- a.buf[:0] // the whole chunk, back to be filled again
		a.buf = nil
	}
	return n, nil
}

// Close stops the goroutine.
func (a *asyncReader) Close() { a.once.Do(func() { close(a.stop) }) }

// torchEvent is an event of traceEvents, as far as it is read.
type torchEvent struct {
	Ph   string          `json:"ph"`
	Cat  string          `json:"cat"`
	Name string          `json:"name"`
	Pid  json.RawMessage `json:"pid"`
	Tid  json.RawMessage `json:"tid"`
	Ts   json.Number     `json:"ts"`
	Dur  json.Number     `json:"dur"`
	Args json.RawMessage `json:"args"`
}

// torchArgs are the arguments of an event that it reads.
type torchArgs struct {
	Name        string  `json:"name"`
	Correlation int64   `json:"correlation"`
	Device      *int64  `json:"device"`
	Stream      *int64  `json:"stream"`
	Grid        []int32 `json:"grid"`
	Block       []int32 `json:"block"`
	Registers   int32   `json:"registers per thread"`
	Shared      int32   `json:"shared memory"`
	Bytes       int64   `json:"bytes"`
}

// torchMeta is the top level of the file, but its events.
type torchMeta struct {
	BaseTimeNanoseconds int64 `json:"baseTimeNanoseconds"`
	DistributedInfo     *struct {
		Rank      *int `json:"rank"`
		WorldSize int  `json:"world_size"`
	} `json:"distributedInfo"`
	DeviceProperties []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"deviceProperties"`
}

// usToNs converts a time in microseconds, as JSON writes it ("1729.123",
// "1.7e3"), to nanoseconds: exactly for the decimal forms, which float64
// would round at epoch times (1.7e15 µs).
func usToNs(n json.Number) (int64, bool) {
	s := string(n)
	if s == "" {
		return 0, false
	}
	if !strings.ContainsAny(s, "eE") {
		neg := strings.HasPrefix(s, "-")
		s = strings.TrimPrefix(s, "-")
		whole, frac, _ := strings.Cut(s, ".")
		w, err := strconv.ParseInt(whole, 10, 64)
		if err != nil {
			return 0, false
		}
		frac = (frac + "000")[:3]
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, false
		}
		v := w*1000 + f
		if neg {
			v = -v
		}
		return v, true
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(math.Round(f * 1000)), true
}

// idOf is a pid or tid: a number, or a string (Kineto writes some as
// "stream 7", Chrome traces as names): the number it ends in, or numbered
// by names in the order seen.
func idOf(v torchID, names map[string]int64) int64 {
	if !v.str {
		return v.n
	}
	if i := strings.LastIndexByte(v.s, ' '); i >= 0 {
		if n, err := strconv.ParseInt(v.s[i+1:], 10, 64); err == nil {
			return n
		}
	}
	if n, ok := names[v.s]; ok {
		return n
	}
	n := int64(1<<20 + len(names))
	names[v.s] = n
	return n
}

// torchID is a pid or tid as the file has it.
type torchID struct {
	n   int64
	s   string
	str bool
}

func readID(raw json.RawMessage) torchID {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return torchID{s: s, str: true}
	}
	if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		return torchID{n: n}
	}
	if f, err := strconv.ParseFloat(string(raw), 64); err == nil {
		return torchID{n: int64(f)}
	}
	return torchID{}
}

// torchSkipped are the categories left out: true for what Kineto records
// of itself and arrows between events, which the warnings do not count;
// false for what is lost (said in the warnings).
var torchSkipped = map[string]bool{"ac2g": true, "fwdbwd": true, "flow": true, "trace": true, "overhead": true,
	"gpu_user_annotation": false, "cuda_sync": false, "cuda_profiler_range": true, "python_function_mark": true}

// copyKindOf is the CUDA memcpy kind of a Kineto memcpy's name ("Memcpy
// HtoD (Pageable -> Device)").
func copyKindOf(name string) int32 {
	switch {
	case strings.Contains(name, "HtoD"):
		return 1
	case strings.Contains(name, "DtoH"):
		return 2
	case strings.Contains(name, "DtoD"):
		return 8
	case strings.Contains(name, "PtoP"):
		return 10
	}
	return 0
}

// decoded is an event of traceEvents, decoded by a worker of LoadTorch.
type decoded struct {
	ph, cat, name string
	pid, tid      torchID
	ts, dur       int64
	timed         bool
	args          torchArgs
	bad           bool
}

// decodeEvent decodes an event, and the arguments of those whose
// arguments are read.
func decodeEvent(raw []byte) decoded {
	var ev torchEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return decoded{bad: true}
	}
	d := decoded{ph: ev.Ph, cat: strings.ToLower(ev.Cat), name: ev.Name, pid: readID(ev.Pid), tid: readID(ev.Tid)}
	d.ts, d.timed = usToNs(ev.Ts)
	d.dur, _ = usToNs(ev.Dur)
	switch {
	case ev.Ph == "M", d.cat == "kernel", d.cat == "gpu_memcpy", d.cat == "gpu_memset", d.cat == "cuda_runtime", d.cat == "cuda_driver":
		json.Unmarshal(ev.Args, &d.args)
	}
	return d
}

// LoadTorch reads a Chrome-trace JSON file of PyTorch's profiler (see
// FormatTorch). Its times count from the capture's first event, whose
// wall-clock time (Kineto's are Unix epoch microseconds, or count from
// baseTimeNanoseconds) is the trace's SessionStartUTC.
//
// A file of several ranks merged (Members) reads as its ranks: path names
// one as "file#rank8"; the file itself reads as its first rank, its
// warnings saying so.
func LoadTorch(path string) (*Trace, error) { return loadTorch(path, nil) }

func loadTorch(path string, p *Progress) (*Trace, error) {
	file, rank, member := SplitMember(path)
	trs, err := loadTorchCached(file, p)
	if err != nil {
		return nil, err
	}
	if member {
		for _, tr := range trs {
			if tr.Rank == rank {
				return tr, nil
			}
		}
		return nil, fmt.Errorf("%s: no rank %d in it", file, rank)
	}
	if len(trs) > 1 {
		tr := trs[0]
		tr.Warnings = append(tr.Warnings, fmt.Sprintf("holds %d ranks merged: rank %d shown; open the file as a session (by its path, or its folder) for all of them", len(trs), tr.Rank))
		return tr, nil
	}
	return trs[0], nil
}

// memberSep joins a merged file's path and one of its ranks: "file#rank8".
const memberSep = "#rank"

// SplitMember splits a path naming a rank of a merged file (Members) into
// the file and the rank; ok is false for any other path (one that exists
// as it is, too).
func SplitMember(path string) (file string, rank int, ok bool) {
	i := strings.LastIndex(path, memberSep)
	if i < 0 {
		return path, -1, false
	}
	r, err := strconv.Atoi(path[i+len(memberSep):])
	if err != nil || r < 0 {
		return path, -1, false
	}
	if _, err := os.Stat(path); err == nil {
		return path, -1, false // a file named so
	}
	return path[:i], r, true
}

// MemberPath is the path of rank of the merged file file.
func MemberPath(file string, rank int) string { return file + memberSep + strconv.Itoa(rank) }

// Members returns the paths of the traces the file at path holds: for a
// PyTorch trace of several ranks merged into one file (as tools that merge
// the ranks' traces write them: every process named "rank8:<pid>"), one a
// rank (MemberPath); for any other, path. It reads the file to know; what
// it read serves the opens that follow (LoadTorch).
func Members(path string) []string { return MembersProgress(path, nil) }

// MembersProgress is Members, saying in p how far the read of a PyTorch
// trace (which tells whether it holds several ranks) has got.
func MembersProgress(path string, p *Progress) []string {
	if _, _, ok := SplitMember(path); ok || Detect(path) != FormatTorch {
		return []string{path}
	}
	trs, err := loadTorchCached(path, p)
	if err != nil || len(trs) < 2 {
		return []string{path}
	}
	out := make([]string, len(trs))
	for i, tr := range trs {
		out[i] = MemberPath(path, tr.Rank)
	}
	return out
}

// torchCache keeps what a file read into for a minute: a session's opens of
// each rank of a merged file read it once; Members and the opens after it,
// once.
var torchCache = struct {
	sync.Mutex
	m map[string]*torchEntry
}{m: map[string]*torchEntry{}}

type torchEntry struct {
	once  sync.Once
	trs   []*Trace
	err   error
	since time.Time
	// progress is the read's, which the loads waiting for it follow.
	progress Progress
}

func loadTorchCached(file string, p *Progress) ([]*Trace, error) {
	key := file
	if fi, err := os.Stat(file); err == nil {
		key += "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	}
	torchCache.Lock()
	for k, e := range torchCache.m {
		if time.Since(e.since) > time.Minute {
			delete(torchCache.m, k)
		}
	}
	e, ok := torchCache.m[key]
	if !ok {
		e = &torchEntry{since: time.Now()}
		torchCache.m[key] = e
	}
	torchCache.Unlock()
	p.Follow(&e.progress)
	e.once.Do(func() {
		e.trs, e.err = loadTorchAll(file, &e.progress)
		e.progress.Finish(e.err)
	})
	return e.trs, e.err
}

// rankPid reads a pid of a merged trace: a name ending in the rank, ":",
// and the process's own pid ("rank8:4127"); ok is false for any other.
func rankPid(s string) (prefix string, rank int, rest string, ok bool) {
	head, rest, found := strings.Cut(s, ":")
	if !found {
		return "", 0, "", false
	}
	i := len(head)
	for i > 0 && head[i-1] >= '0' && head[i-1] <= '9' {
		i--
	}
	if i == len(head) || i == 0 || len(head)-i > 6 {
		return "", 0, "", false
	}
	for _, c := range head[:i] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-' || c == ' ' || c == '[') {
			return "", 0, "", false
		}
	}
	r, _ := strconv.Atoi(head[i:])
	return head[:i], r, rest, true
}

// torchRank is the state of reading one rank's events (all of a file's,
// unless it merges several).
type torchRank struct {
	rank    int
	b       *Builder
	t       *Trace
	threads map[[2]int64]int64 // pid, tid → globalTid, for those that do not fit
	open    map[int64][]Event  // "B" spans open on each thread
	gpuPids map[int64]bool
	events  int
}

func (tr *torchRank) threadOf(pid, tid int64) int64 {
	if pid >= 0 && pid < 1<<24 && tid >= 0 && tid < 1<<24 {
		return pid<<24 | tid
	}
	k := [2]int64{pid, tid}
	g, ok := tr.threads[k]
	if !ok {
		g = (pid&0xFFFFFF)<<24 | int64(len(tr.threads)+1)
		tr.threads[k] = g
		tr.t.ThreadNames[g] = "Thread " + strconv.FormatInt(tid, 10)
	}
	return g
}

// loadTorchAll reads a Chrome-trace file into its traces: one, or one a
// rank for a file of several merged (rankPid), sorted by rank.
//
// It streams: a scanner cuts traceEvents into its events, raw, in batches,
// which workers decode in parallel (decoding JSON is most of the time),
// and the events go into the traces in the file's order.
func loadTorchAll(path string, p *Progress) ([]*Trace, error) {
	r, done, err := torchReader(path, p)
	if err != nil {
		return nil, err
	}
	defer done()
	var meta torchMeta
	names := map[string]int64{}
	ranks := map[int]*torchRank{}
	rankOf := func(rank int) *torchRank {
		tr, ok := ranks[rank]
		if !ok {
			b := NewBuilder(path)
			tr = &torchRank{rank: rank, b: b, t: b.Trace(), threads: map[[2]int64]int64{}, open: map[int64][]Event{}, gpuPids: map[int64]bool{}}
			tr.t.Tables = []string{"traceEvents"}
			ranks[rank] = tr
		}
		return tr
	}
	// pids: of a merged trace, its rank and the process's own pid.
	type pidInfo struct {
		rank int
		pid  torchID
	}
	pids := map[string]pidInfo{}
	pidOf := func(v torchID) (int, int64) {
		if !v.str {
			return -1, v.n
		}
		pi, ok := pids[v.s]
		if !ok {
			pi = pidInfo{-1, v}
			if _, rank, rest, ok := rankPid(v.s); ok {
				pi = pidInfo{rank, readID(json.RawMessage(strconv.Quote(rest)))}
				if n, err := strconv.ParseInt(rest, 10, 64); err == nil {
					pi.pid = torchID{n: n}
				}
			}
			pids[v.s] = pi
		}
		return pi.rank, idOf(pi.pid, names)
	}
	var (
		origin, firstTs int64
		minRel          int64 // the earliest time kept, from origin
		haveOrigin      bool
		events          int
		skipped         = map[string]int{}
	)
	// origin is the first event's time, to the millisecond: the times kept
	// count from it.
	rel := func(ns int64) int64 {
		if !haveOrigin {
			haveOrigin, firstTs = true, ns
			origin = ns - ns%1_000_000
		}
		minRel = min(minRel, ns-origin)
		return ns - origin
	}
	add := func(d *decoded) {
		if d.bad {
			skipped["unreadable"]++
			return
		}
		cat := d.cat
		switch d.ph {
		case "M":
			rank, pid := pidOf(d.pid)
			tr := rankOf(rank)
			tid := idOf(d.tid, names)
			switch d.name {
			case "process_name":
				if d.args.Name != "" {
					tr.t.ProcessNames[pid&0xFFFFFF] = d.args.Name
				}
			case "thread_name":
				if d.args.Name != "" && !tr.gpuPids[pid] {
					g := tr.threadOf(pid, tid)
					if _, renamed := tr.threads[[2]int64{pid, tid}]; !renamed {
						tr.t.ThreadNames[g] = d.args.Name
					}
				}
			}
			return
		case "X", "B", "E", "i", "I":
		case "s", "t", "f": // flow arrows
			return
		default:
			skipped["phase "+d.ph]++
			return
		}
		if quiet, ok := torchSkipped[cat]; ok {
			if !quiet {
				skipped[cat]++
			}
			return
		}
		if !d.timed {
			skipped["no time"]++
			return
		}
		if (d.ph == "i" || d.ph == "I") && cat == "" {
			return // Kineto's own ("Record Window End"), not to count in the times either
		}
		rank, pid := pidOf(d.pid)
		tr := rankOf(rank)
		b := tr.b
		start := rel(d.ts)
		e := Event{Start: start, End: start + max(d.dur, 0), Name: b.Intern(d.name)}
		tid := idOf(d.tid, names)
		events++
		tr.events++
		switch cat {
		case "kernel", "gpu_memcpy", "gpu_memset":
			a := &d.args
			dev, stream := pid, tid
			if a.Device != nil {
				dev = *a.Device
			}
			if a.Stream != nil {
				stream = *a.Stream
			}
			tr.gpuPids[pid] = true
			e.Corr = int32(a.Correlation)
			if cat == "kernel" {
				k := KernelInfo{Device: int32(dev), Stream: int32(stream), Registers: a.Registers, DynamicShared: a.Shared}
				if len(a.Grid) == 3 {
					k.GridX, k.GridY, k.GridZ = a.Grid[0], a.Grid[1], a.Grid[2]
				}
				if len(a.Block) == 3 {
					k.BlockX, k.BlockY, k.BlockZ = a.Block[0], a.Block[1], a.Block[2]
				}
				b.AddKernel(e, k)
			} else {
				m := MemOpInfo{Device: int32(dev), Stream: int32(stream), Bytes: a.Bytes, CopyKind: copyKindOf(d.name)}
				e.Kind = KindMemcpy
				if cat == "gpu_memset" {
					e.Kind, m.CopyKind = KindMemset, -1
				}
				b.AddMemOp(e, m)
			}
		case "cuda_runtime", "cuda_driver":
			e.Corr = int32(d.args.Correlation)
			b.AddRuntime(tr.threadOf(pid, tid), e)
		case "python_function":
			b.AddPython(tr.threadOf(pid, tid), e)
		case "user_annotation":
			if d.ph == "i" || d.ph == "I" {
				e.Kind = KindNVTXMark
			}
			b.AddNVTX(tr.threadOf(pid, tid), e)
		default:
			g := tr.threadOf(pid, tid)
			switch d.ph {
			case "B":
				tr.open[g] = append(tr.open[g], e)
			case "E":
				if st := tr.open[g]; len(st) > 0 {
					s := st[len(st)-1]
					tr.open[g] = st[:len(st)-1]
					s.End = max(start, s.Start)
					b.AddCPUOp(g, s)
				}
			case "i", "I":
				e.Kind = KindNVTXMark
				b.AddNVTX(g, e)
			default:
				b.AddCPUOp(g, e)
			}
		}
	}

	sc := &jsonScanner{br: bufio.NewReaderSize(r, 1<<20)}
	if c, err := sc.next(); err != nil || c != '{' {
		return nil, fmt.Errorf("%s: not a Chrome trace (a JSON object with traceEvents)", path)
	}
	for {
		c, err := sc.next()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if c == '}' {
			break
		}
		if c == ',' {
			continue
		}
		if c != '"' {
			return nil, fmt.Errorf("%s: not JSON", path)
		}
		rawKey, err := sc.value(c, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var key string
		json.Unmarshal(rawKey, &key)
		if c, err := sc.next(); err != nil || c != ':' {
			return nil, fmt.Errorf("%s: not JSON", path)
		}
		c, err = sc.next()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if key == "traceEvents" && c == '[' {
			if err := sc.events(add); err != nil {
				return nil, fmt.Errorf("%s: traceEvents: %w", path, err)
			}
			continue
		}
		raw, err := sc.value(c, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		switch key {
		case "baseTimeNanoseconds":
			var n json.Number
			if json.Unmarshal(raw, &n) == nil {
				meta.BaseTimeNanoseconds, _ = n.Int64()
			}
		case "distributedInfo":
			json.Unmarshal(raw, &meta.DistributedInfo)
		case "deviceProperties":
			json.Unmarshal(raw, &meta.DeviceProperties)
		}
	}
	if events == 0 {
		return nil, fmt.Errorf("%s: no events (a Chrome trace's traceEvents)", path)
	}
	// Processes named by their rank ("rank0:", "rank8:"): one trace a
	// rank (what belongs to none, as Kineto's own spans, left out); else
	// one.
	var list []*torchRank
	for _, tr := range ranks {
		if tr.rank >= 0 && tr.events > 0 {
			list = append(list, tr)
		}
	}
	if len(list) == 0 {
		list = []*torchRank{rankOf(-1)}
	}
	slices.SortFunc(list, func(a, b *torchRank) int { return a.rank - b.rank })
	merged := len(list) > 1
	// The first event of the file is not always the earliest (traces of
	// several processes merged one after the other): times count from the
	// earliest's millisecond, never below 0.
	if minRel < 0 {
		d := (-minRel + 999_999) / 1_000_000 * 1_000_000
		for _, tr := range list {
			tr.b.shift(d)
		}
		origin -= d
	}
	p.Report(PhaseBuilding, 0, 0)
	var out []*Trace
	for _, rk := range list {
		t := rk.t
		for _, d := range meta.DeviceProperties {
			t.Devices[int32(d.ID)] = d.Name
		}
		// Processes a Chrome trace names by strings are called so.
		for name, id := range names {
			if _, ok := t.ProcessNames[id]; !ok {
				t.ProcessNames[id] = name
			}
		}
		// The wall clock: Kineto's times are Unix epoch microseconds
		// (after 2001 in nanoseconds, 1e18); older ones count from
		// baseTimeNanoseconds.
		switch {
		case firstTs > 1e18:
			t.SessionStartUTC = origin
		case meta.BaseTimeNanoseconds > 0:
			t.SessionStartUTC = meta.BaseTimeNanoseconds + origin
		}
		t.Rank = rk.rank
		if d := meta.DistributedInfo; d != nil && d.Rank != nil && rk.rank < 0 {
			t.Rank = *d.Rank
		}
		if merged {
			// The ranks' times count on the clock of the file they were
			// merged into, the same for all of them.
			t.Path = MemberPath(path, rk.rank)
			t.Clock = path
			t.SessionStartMono = origin
		}
		if len(skipped) > 0 {
			var parts []string
			for k, n := range skipped {
				parts = append(parts, fmt.Sprintf("%d %s", n, k))
			}
			slices.Sort(parts)
			t.Warnings = append(t.Warnings, "left out: "+strings.Join(parts, ", "))
		}
		tr := rk.b.Finish()
		// A profile's ranges are record_function's and the profiler's steps.
		for _, tk := range tr.Tracks {
			if tk.Kind == TrackNVTX {
				tk.Name = strings.TrimSuffix(tk.Name, " · NVTX") + " · Annotations"
			}
		}
		if tr.Counts[KindPython] == 0 {
			tr.Warnings = append(tr.Warnings, "no Python calls: profile with torch.profiler.profile(with_stack=True) for Python stacks")
		}
		out = append(out, tr)
	}
	return out, nil
}

// jsonScanner reads a JSON document's values raw, as they stream.
type jsonScanner struct {
	br *bufio.Reader
	// slashes is the run of backslashes the last chunk container scanned
	// ended in, inside a string.
	slashes int
}

// next returns the next byte that is not white space.
func (s *jsonScanner) next() (byte, error) {
	for {
		c, err := s.br.ReadByte()
		if err != nil {
			return 0, err
		}
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return c, nil
		}
	}
}

// value appends to out the raw value that starts with c (read already).
func (s *jsonScanner) value(c byte, out []byte) ([]byte, error) {
	out = append(out, c)
	switch c {
	case '"':
		return s.str(out)
	case '{', '[':
		return s.container(out)
	}
	// A number, true, false or null: up to the next delimiter.
	for {
		c, err := s.br.ReadByte()
		if err != nil {
			return out, nil
		}
		switch c {
		case ',', '}', ']', ' ', '\n', '\r', '\t':
			s.br.UnreadByte()
			return out, nil
		}
		out = append(out, c)
	}
}

// str appends the rest of a string (its opening quote read already).
func (s *jsonScanner) str(out []byte) ([]byte, error) {
	for {
		c, err := s.br.ReadByte()
		if err != nil {
			return out, err
		}
		out = append(out, c)
		switch c {
		case '\\':
			c, err := s.br.ReadByte()
			if err != nil {
				return out, err
			}
			out = append(out, c)
		case '"':
			return out, nil
		}
	}
}

// structural marks the bytes container stops at outside strings.
var structural = func() (t [256]bool) {
	for _, c := range []byte(`"{}[]`) {
		t[c] = true
	}
	return
}()

// container appends the rest of an object or array (its opening read
// already), scanning the buffered input a chunk at a time: outside strings
// for the bytes that matter, inside them for their closing quote (with
// IndexByte, most of the bytes being in strings).
func (s *jsonScanner) container(out []byte) ([]byte, error) {
	depth, inStr := 1, false
	for {
		if _, err := s.br.Peek(1); err != nil {
			return out, err
		}
		buf, _ := s.br.Peek(s.br.Buffered())
		i := 0
		for i < len(buf) {
			if inStr {
				j := bytes.IndexByte(buf[i:], '"')
				if j < 0 {
					i = len(buf)
					break
				}
				i += j
				// Escaped after an odd run of backslashes (one that began
				// in the chunk before counts on: s.slashes).
				k := i
				for k > 0 && buf[k-1] == '\\' {
					k--
				}
				run := i - k
				if k == 0 {
					run += s.slashes
				}
				s.slashes = 0
				i++
				if run%2 == 0 {
					inStr = false
				}
				continue
			}
			for i < len(buf) && !structural[buf[i]] {
				i++
			}
			if i == len(buf) {
				break
			}
			switch buf[i] {
			case '"':
				inStr, s.slashes = true, 0
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					out = append(out, buf[:i+1]...)
					s.br.Discard(i + 1)
					return out, nil
				}
			}
			i++
		}
		// A string open at the chunk's end: the backslashes it ends in.
		if inStr {
			k := len(buf)
			for k > 0 && buf[k-1] == '\\' {
				k--
			}
			if k == 0 {
				s.slashes += len(buf)
			} else {
				s.slashes = len(buf) - k
			}
		}
		out = append(out, buf...)
		s.br.Discard(len(buf))
	}
}

// events reads the rest of traceEvents (its "[" read already): its events
// cut out raw in batches, decoded by workers in parallel, given to add in
// their order.
func (s *jsonScanner) events(add func(*decoded)) error {
	const batchLen = 4096
	type batch struct {
		raw  [][]byte
		buf  []byte
		out  []decoded
		done chan struct{}
	}
	workers := max(runtime.GOMAXPROCS(0), 1)
	todo := make(chan *batch, workers)
	ordered := make(chan *batch, 2*workers)
	var scanErr error
	go func() {
		defer close(ordered)
		defer close(todo)
		cur := &batch{done: make(chan struct{})}
		flush := func() {
			if len(cur.raw) == 0 {
				return
			}
			ordered <- cur
			todo <- cur
			cur = &batch{done: make(chan struct{})}
		}
		for {
			c, err := s.next()
			if err != nil {
				scanErr = err
				break
			}
			if c == ']' {
				break
			}
			if c == ',' {
				continue
			}
			start := len(cur.buf)
			cur.buf, err = s.value(c, cur.buf)
			if err != nil {
				scanErr = err
				break
			}
			cur.raw = append(cur.raw, cur.buf[start:len(cur.buf):len(cur.buf)])
			if len(cur.raw) == batchLen {
				flush()
			}
		}
		flush()
	}()
	for range workers {
		go func() {
			for bt := range todo {
				bt.out = make([]decoded, len(bt.raw))
				for i, raw := range bt.raw {
					bt.out[i] = decodeEvent(raw)
				}
				close(bt.done)
			}
		}()
	}
	for bt := range ordered {
		<-bt.done
		for i := range bt.out {
			add(&bt.out[i])
		}
	}
	return scanErr
}

// readTorchInfo reads when a Chrome trace's capture started and its rank,
// from its start: up to its first timed event (Kineto writes its
// distributedInfo before its events).
func readTorchInfo(path string) Info {
	info := Info{Rank: -1}
	r, done, err := torchReader(path, nil)
	if err != nil {
		return info
	}
	defer done()
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return info
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return info
		}
		switch tok {
		case "distributedInfo":
			var d struct {
				Rank *int `json:"rank"`
			}
			if dec.Decode(&d) == nil && d.Rank != nil {
				info.Rank = *d.Rank
			}
		case "traceEvents":
			if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
				return info
			}
			for dec.More() {
				var ev torchEvent
				if dec.Decode(&ev) != nil {
					return info
				}
				if _, skip := torchSkipped[strings.ToLower(ev.Cat)]; skip || ev.Ph == "M" || ev.Ph == "s" || ev.Ph == "f" || ev.Ph == "t" {
					continue
				}
				// As LoadTorch takes it (epoch times; not those that count
				// from baseTimeNanoseconds, which comes after the events).
				if ns, ok := usToNs(ev.Ts); ok && ns > 1e18 {
					info.SessionStartUTC = ns - ns%1_000_000
				}
				return info
			}
			return info
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return info
			}
		}
	}
	return info
}
