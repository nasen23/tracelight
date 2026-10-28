package trace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"tracelight/internal/nsysrep"
)

// NativeEnv names the environment variable that controls the native
// .nsys-rep decoder: "off" never uses it, "force" uses it on reports of any
// version, and anything else on verified versions only (nsysrep.Verified).
// (NSIGHT_ALT_NATIVE, its name before the rename, still works.)
const NativeEnv = "TRACELIGHT_NATIVE"

// Getenv returns the environment variable TRACELIGHT_<name>, or failing that
// NSIGHT_ALT_<name>, as it was called before the program was renamed.
func Getenv(name string) string {
	if v, ok := os.LookupEnv("TRACELIGHT_" + name); ok {
		return v
	}
	return os.Getenv("NSIGHT_ALT_" + name)
}

// loadReport loads an .nsys-rep report with the native decoder, or, when
// that cannot read it (an unverified version, a format it does not know),
// through export: the nsys conversion to SQLite.
func loadReport(path string, export func(string) (string, error), p *Progress) (*Trace, error) {
	var nativeErr error
	if mode := Getenv("NATIVE"); mode != "off" {
		t, err := loadNative(path, nsysrep.Options{AnyVersion: mode == "force"}, p)
		if err == nil {
			return t, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		nativeErr = err
	}
	p.Report(PhaseConverting, 0, 0)
	sqlitePath, err := export(path)
	if err != nil {
		if nativeErr != nil {
			return nil, fmt.Errorf("%w (native decoder: %v)", err, nativeErr)
		}
		return nil, err
	}
	t, err := loadSQLite(sqlitePath, p)
	if t != nil {
		t.Path = path
		if nativeErr != nil {
			t.Warnings = append(t.Warnings, "read through nsys export: native decoder: "+nativeErr.Error())
		}
	}
	return t, err
}

// Labels the SQLite export takes from its ENUM_CUDA_MEMCPY_OPER and
// ENUM_CUPTI_SYNC_TYPE tables, which a report does not hold.
var (
	nativeMemcpyKinds = map[int32]string{
		0: "Unknown", 1: "HtoD", 2: "DtoH", 3: "HtoA", 4: "AtoH", 5: "AtoA",
		6: "AtoD", 7: "DtoA", 8: "DtoD", 9: "HtoH", 10: "PtoP",
		11: "UVM_HTOD", 12: "UVM_DTOH", 13: "UVM_DTOD",
	}
	nativeSyncTypes = map[int32]string{
		0: "Unknown", 1: "Event sync", 2: "Stream wait sync", 3: "Stream sync", 4: "Context sync",
	}
)

// LoadReport reads an .nsys-rep report with the native decoder (package
// nsysrep), into the same trace LoadSQLite makes of its export.
func LoadReport(path string, o nsysrep.Options) (*Trace, error) { return loadNative(path, o, nil) }

func loadNative(path string, o nsysrep.Options, p *Progress) (*Trace, error) {
	start := time.Now()
	// How far: the file's bytes read, then its event frames decoded, each
	// of them half of the reading.
	data, err := readCounting(path, p)
	if err != nil {
		return nil, err
	}
	f, err := nsysrep.Parse(data, o)
	if err != nil {
		return nil, err
	}
	size := int64(len(data))
	b := NewBuilder(path)
	// When and where: the session's start on the host's monotonic clock and
	// on the wall clock, and the host (see nsysrep.Session).
	sess := f.Session()
	b.Trace().SessionStartMono, b.Trace().SessionStartUTC, b.Trace().Host = sess.MonoStart, sess.WallStart, sess.Host
	timing := []TableTime{{Table: "report index (version " + f.Version + ")", Time: time.Since(start), Chunks: 1}}

	// The string table and the metadata decode while the events do.
	var strs *nsysrep.StringTable
	var meta *nsysrep.Meta
	var strErr, metaErr error
	var strTime, metaTime time.Duration
	var side sync.WaitGroup
	side.Go(func() {
		s := time.Now()
		strs, strErr = f.Strings()
		strTime = time.Since(s)
	})
	side.Go(func() {
		s := time.Now()
		meta, metaErr = f.Meta()
		metaTime = time.Since(s)
	})

	frames := f.EventFrames()
	perFrame := size / int64(max(len(frames), 1))
	shards := make([]*Builder, len(frames))
	errs := make([]error, len(frames))
	procs := make([]map[int64]string, len(frames))
	evStart := time.Now()
	workers := min(runtime.GOMAXPROCS(0), max(len(frames), 1))
	var next atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			var buf []byte
			for {
				n := int(next.Add(1)) - 1
				if n >= len(frames) {
					return
				}
				sh := NewBuilder("")
				sh.capHint = 8 << 10
				pr := map[int64]string{}
				buf, errs[n] = f.DecodeEvents(frames[n], buf, nativeVisitor(sh, pr))
				shards[n], procs[n] = sh, pr
				p.Add(perFrame)
			}
		})
	}
	wg.Wait()
	side.Wait()
	rows := 0
	for _, sh := range shards {
		rows += sh.events + len(sh.syncs)
	}
	if err := errors.Join(errors.Join(errs...), strErr, metaErr); err != nil {
		return nil, err
	}
	timing = append(timing,
		TableTime{Table: "event frames", Rows: rows, Time: time.Since(evStart), Chunks: len(frames)},
		TableTime{Table: "string table", Rows: strs.Len(), Time: strTime, Chunks: 1},
		TableTime{Table: "threads and GPUs", Rows: len(meta.Threads) + len(meta.GPUs), Time: metaTime, Chunks: 1})

	p.Report(PhaseBuilding, 0, 0)
	t := b.t
	for _, g := range meta.GPUs {
		t.Devices[g.ID] = g.Name
	}
	for _, pr := range procs {
		for gpid, name := range pr {
			pid, _ := PidTid(gpid)
			t.ProcessNames[pid] = name
		}
	}
	str := func(id int64) string {
		if id >= 0 && id < int64(strs.Len()) {
			return strs.At(int(id))
		}
		return "<string " + strconv.FormatInt(id, 10) + ">"
	}
	for _, th := range meta.Threads {
		t.ThreadNames[th.GlobalTid] = str(th.NameID)
	}
	for _, sh := range shards {
		for i, id := range sh.ids {
			if id >= 0 {
				sh.t.Names[i] = str(id)
			}
		}
	}
	// The file and its string table are garbage now: collect them before
	// merging, the peak of the load.
	f, strs = nil, nil
	runtime.GC()

	mergeStart := time.Now()
	b.merge(shards)
	shards = nil
	runtime.GC() // as LoadSQLite: drop the shards before packing
	merge := time.Since(mergeStart)
	read := time.Since(start)

	start = time.Now()
	t = b.Finish()
	t.Load = LoadStats{Read: read, Merge: merge, Build: time.Since(start), Tables: timing, Workers: workers}
	return t, nil
}

// nativeVisitor adds a frame's events to sh, as the SQLite loader adds the
// rows of the tables they come from, and the names of processes to procs.
func nativeVisitor(sh *Builder, procs map[int64]string) *nsysrep.Visitor {
	memcpyNames := map[int32]int32{}
	syncNames := map[int32]int32{}
	memset, unnamed := int32(-1), int32(-1)
	return &nsysrep.Visitor{
		Kernel: func(k *nsysrep.Kernel) {
			name := k.ShortName
			if name < 0 {
				name = k.DemangledName
			}
			if name < 0 {
				name = k.MangledName
			}
			full := k.DemangledName
			if full < 0 {
				full = k.MangledName
			}
			if full < 0 {
				full = k.ShortName
			}
			info := KernelInfo{
				Device: k.Device, Stream: k.Stream,
				GridX: k.GridX, GridY: k.GridY, GridZ: k.GridZ,
				BlockX: k.BlockX, BlockY: k.BlockY, BlockZ: k.BlockZ,
				Registers: k.Registers, StaticShared: k.StaticShared, DynamicShared: k.DynamicShared,
			}
			if full >= 0 {
				info.FullName = sh.nameID(full)
			}
			n := int32(0)
			if name >= 0 {
				n = sh.nameID(name)
			}
			sh.AddKernel(Event{Start: k.Start, End: k.End, Name: n, Corr: int32(k.Correlation)}, info)
		},
		Memcpy: func(m *nsysrep.Memcpy) {
			n, ok := memcpyNames[m.CopyKind]
			if !ok {
				label, ok := nativeMemcpyKinds[m.CopyKind]
				if !ok {
					label = "kind " + strconv.Itoa(int(m.CopyKind))
				}
				n = sh.Intern("Memcpy " + label)
				memcpyNames[m.CopyKind] = n
			}
			sh.AddMemOp(Event{Start: m.Start, End: m.End, Name: n, Corr: int32(m.Correlation), Kind: KindMemcpy},
				MemOpInfo{Device: m.Device, Stream: m.Stream, Bytes: m.Bytes, CopyKind: m.CopyKind})
		},
		Memset: func(m *nsysrep.Memset) {
			if memset < 0 {
				memset = sh.Intern("Memset")
			}
			sh.AddMemOp(Event{Start: m.Start, End: m.End, Name: memset, Corr: int32(m.Correlation), Kind: KindMemset},
				MemOpInfo{Device: m.Device, Stream: m.Stream, Bytes: m.Bytes, CopyKind: -1, Value: m.Value})
		},
		Sync: func(s *nsysrep.Sync) {
			n, ok := syncNames[s.SyncType]
			if !ok {
				label, ok := nativeSyncTypes[s.SyncType]
				if !ok {
					label = "Sync type " + strconv.Itoa(int(s.SyncType))
				}
				n = sh.Intern(label)
				syncNames[s.SyncType] = n
			}
			sh.AddSync(Event{Start: s.Start, End: s.End, Name: n, Corr: int32(s.Correlation)})
		},
		API: func(a *nsysrep.API) {
			sh.AddRuntime(a.GlobalTid, Event{Start: a.Start, End: a.End, Name: sh.nameID(a.NameID), Corr: int32(a.Correlation)})
		},
		NVTX: func(n *nsysrep.NVTX) {
			kind, end := KindNVTX, n.End
			switch n.EventType {
			case nsysrep.NVTXPushPopRange, nsysrep.NVTXStartEndRange, nsysrep.NVTXPushPopPayload, nsysrep.NVTXStartEndPayload:
				if !n.HasEnd {
					return // a range never closed
				}
			case nsysrep.NVTXMark:
				kind, end = KindNVTXMark, n.Start
			default:
				return // categories, domains, thread names
			}
			var name int32
			switch {
			case n.Text != nil:
				name = sh.internBytes(n.Text)
			case n.TextID >= 0:
				name = sh.nameID(n.TextID)
			default:
				if unnamed < 0 {
					unnamed = sh.Intern("(unnamed)")
				}
				name = unnamed
			}
			sh.AddNVTX(n.GlobalTid, Event{Start: n.Start, End: end, Name: name, Kind: kind})
		},
		Process: func(p *nsysrep.Process) {
			procs[p.GlobalPid] = string(p.Name)
		},
	}
}

// readCounting reads the file at path whole, its bytes counted into p as
// the first half of the reading (Report's total is twice its size).
func readCounting(path string, p *Progress) ([]byte, error) {
	if p == nil {
		return os.ReadFile(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	p.Report(PhaseReading, 0, 2*fi.Size())
	data := make([]byte, 0, fi.Size()+1)
	buf := bytes.NewBuffer(data)
	_, err = buf.ReadFrom(countingReader{f, p})
	return buf.Bytes(), err
}
