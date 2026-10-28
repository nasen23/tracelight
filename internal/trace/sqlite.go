//go:build !js

package trace

import (
	"cmp"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"tracelight/internal/nsysrep"
)

// Event tables of an nsys SQLite export that the viewer reads.
const (
	tableKernel  = "CUPTI_ACTIVITY_KIND_KERNEL"
	tableMemcpy  = "CUPTI_ACTIVITY_KIND_MEMCPY"
	tableMemset  = "CUPTI_ACTIVITY_KIND_MEMSET"
	tableRuntime = "CUPTI_ACTIVITY_KIND_RUNTIME"
	tableSync    = "CUPTI_ACTIVITY_KIND_SYNCHRONIZATION"
	tableNVTX    = "NVTX_EVENTS"
)

// NVTX event types (ENUM_NSYS_EVENT_TYPE).
const (
	nvtxMark            = 34
	nvtxPushPopRange    = 59
	nvtxStartEndRange   = 60
	nvtxPushPopPayload  = 70
	nvtxStartEndPayload = 71
)

// memcpyKinds are the labels of CUDA memcpy kinds, for exports without
// ENUM_CUDA_MEMCPY_OPER.
var memcpyKinds = map[int64]string{
	0: "Unknown", 1: "HtoD", 2: "DtoH", 3: "HtoA", 4: "AtoH", 5: "AtoA",
	6: "AtoD", 7: "DtoA", 8: "DtoD", 9: "HtoH", 10: "PtoP",
}

// syncTypes are the labels of CUPTI synchronization types, for exports
// without ENUM_CUPTI_SYNC_TYPE.
var syncTypes = map[int64]string{
	0: "Unknown sync", 1: "Event sync", 2: "Stream wait sync", 3: "Stream sync", 4: "Context sync",
}

// chunkRows is about how many rows one job reads: large tables are split
// into ranges of rowids, read in parallel.
const chunkRows = 64 << 10

// LoadSQLite reads a SQLite file written by `nsys export --type sqlite`.
// Tables it does not find are listed in Trace.Missing; the trace is still
// returned as long as the file is a database.
func LoadSQLite(path string) (*Trace, error) { return loadSQLite(path, nil) }

func loadSQLite(path string, p *Progress) (*Trace, error) {
	start := time.Now()
	c, err := openReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer c.close()
	l := &loader{path: path, c: c, b: NewBuilder(path), cols: map[string][]string{}, progress: p}
	if err := l.schema(); err != nil {
		return nil, fmt.Errorf("%s: not a SQLite database: %w", path, err)
	}
	l.timed("metadata", 0, l.metadata)
	shards := l.readEvents()
	p.Report(PhaseBuilding, 0, 0)
	l.timed("StringIds (used ids only)", 0, func() { l.resolveNames(shards) })
	mergeStart := time.Now()
	l.b.merge(shards)
	shards = nil
	// The shards' slices are garbage now, at the peak of the load: collect
	// them before packing allocates lanes, which would otherwise grow the
	// heap well past twice what it keeps. Events hold no pointers, so this
	// marks little more than the names.
	runtime.GC()
	merge := time.Since(mergeStart)
	read := time.Since(start)

	start = time.Now()
	t := l.b.Finish()
	t.Load = LoadStats{Read: read, Merge: merge, Build: time.Since(start), Tables: l.timing, Workers: l.workers}
	return t, nil
}

type loader struct {
	path    string
	c       *conn
	b       *Builder
	cols    map[string][]string
	timing  []TableTime
	workers int

	memcpyLabels map[int64]string
	syncLabels   map[int64]string
	// threadNameIDs are ThreadNames' StringIds ids, by globalTid.
	threadNameIDs map[int64]int64
	// progress counts the rows read, by the chunks done.
	progress *Progress
}

// timed runs fn and records how long it took.
func (l *loader) timed(what string, rows int, fn func()) {
	start := time.Now()
	fn()
	l.timing = append(l.timing, TableTime{Table: what, Rows: rows, Time: time.Since(start), Chunks: 1})
}

func (l *loader) schema() error {
	var tables []string
	err := l.c.query(`SELECT name FROM sqlite_master WHERE type IN ('table','view') ORDER BY name`, nil, func(s *stmt) error {
		tables = append(tables, s.text(0))
		return nil
	})
	if err != nil {
		return err
	}
	l.b.t.Tables = tables
	for _, name := range tables {
		var cols []string
		err := l.c.query(`SELECT name FROM pragma_table_info('`+strings.ReplaceAll(name, "'", "''")+`')`, nil, func(s *stmt) error {
			cols = append(cols, s.text(0))
			return nil
		})
		if err != nil {
			return err
		}
		l.cols[name] = cols
	}
	return nil
}

func (l *loader) has(table string) bool { _, ok := l.cols[table]; return ok }

func (l *loader) hasCol(table, col string) bool { return slices.Contains(l.cols[table], col) }

// col returns col quoted if table has it, or def, an SQL expression.
func (l *loader) col(table, col, def string) string {
	if l.hasCol(table, col) {
		return `"` + col + `"`
	}
	return def
}

// firstCol returns the first of cols that table has, quoted, or def.
func (l *loader) firstCol(table, def string, cols ...string) string {
	for _, c := range cols {
		if l.hasCol(table, c) {
			return `"` + c + `"`
		}
	}
	return def
}

func (l *loader) warn(format string, args ...any) {
	t := l.b.t
	t.Warnings = append(t.Warnings, fmt.Sprintf(format, args...))
}

// metadata reads the small tables: GPUs, processes, threads' names and the
// labels of enums.
func (l *loader) metadata() {
	t := l.b.t
	info := readInfo(l.c, l.hasCol)
	t.SessionStartUTC, t.SessionStartMono, t.Host = info.SessionStartUTC, info.SessionStartMono, info.Host
	if tbl := "TARGET_INFO_GPU"; l.has(tbl) && l.hasCol(tbl, "name") {
		if idCol := l.firstCol(tbl, "", "id", "cuDevice", "deviceId"); idCol != "" {
			l.query(tbl, `SELECT `+idCol+`, name FROM `+tbl+` WHERE `+idCol+` IS NOT NULL`, func(s *stmt) {
				t.Devices[int32(s.int64(0))] = s.text(1)
			})
		}
	}
	if tbl := "PROCESSES"; l.hasCol(tbl, "pid") && l.hasCol(tbl, "name") {
		l.query(tbl, `SELECT pid, name FROM `+tbl+` WHERE pid IS NOT NULL`, func(s *stmt) {
			t.ProcessNames[s.int64(0)] = s.text(1)
		})
	}
	l.threadNameIDs = map[int64]int64{}
	if tbl := "ThreadNames"; l.hasCol(tbl, "nameId") && l.hasCol(tbl, "globalTid") {
		l.query(tbl, `SELECT nameId, globalTid FROM `+tbl+` WHERE nameId IS NOT NULL AND globalTid IS NOT NULL ORDER BY `+l.col(tbl, "priority", "0"),
			func(s *stmt) { l.threadNameIDs[s.int64(1)] = s.int64(0) })
	}
	l.memcpyLabels = labels(memcpyKinds)
	if tbl := "ENUM_CUDA_MEMCPY_OPER"; l.hasCol(tbl, "id") && l.hasCol(tbl, "name") {
		l.query(tbl, `SELECT id, name FROM `+tbl, func(s *stmt) {
			// Names are as CUDA_MEMCPY_KIND_HTOD (CUDA_MEMCPY_OPER_HTOD in
			// some versions); keep the short form, HtoD.
			name := strings.TrimPrefix(strings.TrimPrefix(s.text(1), "CUDA_MEMCPY_KIND_"), "CUDA_MEMCPY_OPER_")
			if len(name) == 4 && name[1] == 'T' && name[2] == 'O' {
				name = name[:1] + "to" + name[3:]
			}
			l.memcpyLabels[s.int64(0)] = name
		})
	}
	l.syncLabels = labels(syncTypes)
	if tbl := "ENUM_CUPTI_SYNC_TYPE"; l.hasCol(tbl, "id") && l.hasCol(tbl, "label") {
		l.query(tbl, `SELECT id, label FROM `+tbl, func(s *stmt) {
			if label := s.text(1); label != "" {
				l.syncLabels[s.int64(0)] = label
			}
		})
	}
}

func labels(m map[int64]string) map[int64]string {
	out := make(map[int64]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// query runs a metadata query on the main connection, warning on errors.
func (l *loader) query(table, q string, fn func(*stmt)) {
	if err := l.c.query(q, nil, func(s *stmt) error { fn(s); return nil }); err != nil {
		l.warn("%s: %v", table, err)
	}
}

// job reads rows [from, to) of a table into a shard.
type job struct {
	table    string
	from, to int64 // rowids; to <= from reads the whole table
	read     func(c *conn, sh *Builder, where string, args []int64) error
}

// readEvents reads the event tables, in parallel, into shards in the order
// of their rows.
func (l *loader) readEvents() []*Builder {
	t := l.b.t
	tables := []struct {
		name string
		read func(c *conn, sh *Builder, where string, args []int64) error
	}{
		{tableKernel, l.readKernels},
		{tableMemcpy, l.readMemcpy},
		{tableMemset, l.readMemset},
		{tableRuntime, l.readRuntime},
		{tableSync, l.readSync},
		{tableNVTX, l.readNVTX},
	}
	var jobs []job
	for _, tb := range tables {
		if !l.has(tb.name) {
			if tb.name != tableSync { // optional: shown when there
				t.Missing = append(t.Missing, tb.name)
			}
			continue
		}
		lo, hi, ok := int64(0), int64(-1), false
		err := l.c.query(`SELECT min(rowid), max(rowid) FROM "`+tb.name+`"`, nil, func(s *stmt) error {
			lo, hi, ok = s.int64(0), s.int64(1), !s.null(0)
			return nil
		})
		switch {
		case err != nil: // no rowid: read it whole
			jobs = append(jobs, job{table: tb.name, read: tb.read})
		case !ok: // empty
		default:
			for from := lo; from <= hi; from += chunkRows {
				jobs = append(jobs, job{table: tb.name, from: from, to: min(from+chunkRows, hi+1), read: tb.read})
			}
		}
	}

	// How far: the rows of the chunks read (a table read whole, as a chunk).
	rowsOf := func(j job) int64 { return max(j.to-j.from, chunkRows/4) }
	var total int64
	for _, j := range jobs {
		total += rowsOf(j)
	}
	l.progress.Report(PhaseReading, 0, total)
	shards := make([]*Builder, len(jobs))
	starts, ends := make([]time.Time, len(jobs)), make([]time.Time, len(jobs))
	errs := make([]error, len(jobs))
	// The largest jobs first, for the workers to finish together.
	order := make([]int, len(jobs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(jobs[b].to-jobs[b].from, jobs[a].to-jobs[a].from)
	})
	conns := []*conn{l.c}
	for len(conns) < min(runtime.GOMAXPROCS(0), 8, max(len(jobs), 1)) {
		c, err := openReadOnly(l.path)
		if err != nil {
			break // read with the connections there are
		}
		conns = append(conns, c)
	}
	l.workers = len(conns)
	var next atomic.Int32
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Go(func() {
			for {
				n := int(next.Add(1)) - 1
				if n >= len(order) {
					return
				}
				i := order[n]
				j := jobs[i]
				sh := NewBuilder("")
				// A chunk's rows mostly go to one track (a thread or a
				// stream): size its first track for them, rather than
				// doubling it up to there.
				sh.capHint = int(j.to - j.from)
				starts[i] = time.Now()
				where, args := "", []int64(nil)
				if j.to > j.from {
					where, args = ` rowid >= ? AND rowid < ?`, []int64{j.from, j.to}
				}
				errs[i] = j.read(c, sh, where, args)
				ends[i] = time.Now()
				shards[i] = sh
				l.progress.Add(rowsOf(j))
			}
		})
	}
	wg.Wait()
	for _, c := range conns[1:] {
		c.close()
	}

	// One timing line per table, and its errors as warnings.
	for i := 0; i < len(jobs); {
		k := i
		tt := TableTime{Table: jobs[i].table}
		first, last := starts[i], ends[i]
		var tableErr error
		for ; k < len(jobs) && jobs[k].table == jobs[i].table; k++ {
			tt.Rows += shards[k].events + len(shards[k].syncs)
			tt.Chunks++
			if starts[k].Before(first) {
				first = starts[k]
			}
			if ends[k].After(last) {
				last = ends[k]
			}
			tableErr = cmp.Or(tableErr, errs[k])
		}
		tt.Time = last.Sub(first)
		l.timing = append(l.timing, tt)
		if tableErr != nil {
			l.warn("%s: %v", jobs[i].table, tableErr)
		}
		i = k
	}
	return shards
}

// where appends a rowid range to a query's conditions.
func where(conds, rng string) string {
	switch {
	case conds == "" && rng == "":
		return ""
	case conds == "":
		return " WHERE" + rng
	case rng == "":
		return " WHERE " + conds
	}
	return " WHERE " + conds + " AND" + rng
}

func (l *loader) readKernels(c *conn, sh *Builder, rng string, args []int64) error {
	const t = tableKernel
	nameCol := l.firstCol(t, "", "shortName", "demangledName", "mangledName")
	fullCol := l.firstCol(t, "NULL", "demangledName", "mangledName", "shortName")
	if nameCol == "" {
		return errors.New("no kernel name column")
	}
	q := `SELECT start, end, ` +
		l.col(t, "deviceId", "0") + `, ` + l.col(t, "streamId", "0") + `, ` +
		l.col(t, "correlationId", "0") + `, ` + nameCol + `, ` + fullCol + `, ` +
		l.col(t, "gridX", "0") + `, ` + l.col(t, "gridY", "0") + `, ` + l.col(t, "gridZ", "0") + `, ` +
		l.col(t, "blockX", "0") + `, ` + l.col(t, "blockY", "0") + `, ` + l.col(t, "blockZ", "0") + `, ` +
		l.col(t, "registersPerThread", "0") + `, ` + l.col(t, "staticSharedMemory", "0") + `, ` +
		l.col(t, "dynamicSharedMemory", "0") + ` FROM ` + t + where("", rng)
	return c.query(q, args, func(s *stmt) error {
		k := KernelInfo{
			Device: int32(s.int64(2)), Stream: int32(s.int64(3)),
			GridX: int32(s.int64(7)), GridY: int32(s.int64(8)), GridZ: int32(s.int64(9)),
			BlockX: int32(s.int64(10)), BlockY: int32(s.int64(11)), BlockZ: int32(s.int64(12)),
			Registers: int32(s.int64(13)), StaticShared: int32(s.int64(14)), DynamicShared: int32(s.int64(15)),
		}
		if !s.null(6) {
			k.FullName = sh.nameID(s.int64(6))
		}
		sh.AddKernel(Event{Start: s.int64(0), End: s.int64(1), Name: sh.nameID(s.int64(5)), Corr: int32(s.int64(4))}, k)
		return nil
	})
}

func (l *loader) readMemcpy(c *conn, sh *Builder, rng string, args []int64) error {
	const t = tableMemcpy
	names := map[int64]int32{}
	q := `SELECT start, end, ` + l.col(t, "deviceId", "0") + `, ` + l.col(t, "streamId", "0") + `, ` +
		l.col(t, "correlationId", "0") + `, ` + l.col(t, "bytes", "0") + `, ` + l.col(t, "copyKind", "0") +
		` FROM ` + t + where("", rng)
	return c.query(q, args, func(s *stmt) error {
		kind := s.int64(6)
		n, ok := names[kind]
		if !ok {
			label, ok := l.memcpyLabels[kind]
			if !ok {
				label = "kind " + strconv.FormatInt(kind, 10)
			}
			n = sh.Intern("Memcpy " + label)
			names[kind] = n
		}
		sh.AddMemOp(Event{Start: s.int64(0), End: s.int64(1), Name: n, Corr: int32(s.int64(4)), Kind: KindMemcpy},
			MemOpInfo{Device: int32(s.int64(2)), Stream: int32(s.int64(3)), Bytes: s.int64(5), CopyKind: int32(kind)})
		return nil
	})
}

func (l *loader) readMemset(c *conn, sh *Builder, rng string, args []int64) error {
	const t = tableMemset
	name := sh.Intern("Memset")
	q := `SELECT start, end, ` + l.col(t, "deviceId", "0") + `, ` + l.col(t, "streamId", "0") + `, ` +
		l.col(t, "correlationId", "0") + `, ` + l.col(t, "bytes", "0") + `, ` + l.col(t, "value", "0") +
		` FROM ` + t + where("", rng)
	return c.query(q, args, func(s *stmt) error {
		sh.AddMemOp(Event{Start: s.int64(0), End: s.int64(1), Name: name, Corr: int32(s.int64(4)), Kind: KindMemset},
			MemOpInfo{Device: int32(s.int64(2)), Stream: int32(s.int64(3)), Bytes: s.int64(5), CopyKind: -1, Value: int32(s.int64(6))})
		return nil
	})
}

func (l *loader) readRuntime(c *conn, sh *Builder, rng string, args []int64) error {
	const t = tableRuntime
	nameCol := l.firstCol(t, "0", "nameId", "name")
	q := `SELECT start, end, ` + l.col(t, "globalTid", "0") + `, ` + l.col(t, "correlationId", "0") + `, ` +
		nameCol + ` FROM ` + t + where("", rng)
	return c.query(q, args, func(s *stmt) error {
		sh.AddRuntime(s.int64(2), Event{Start: s.int64(0), End: s.int64(1), Name: sh.nameID(s.int64(4)), Corr: int32(s.int64(3))})
		return nil
	})
}

func (l *loader) readSync(c *conn, sh *Builder, rng string, args []int64) error {
	const t = tableSync
	if !l.hasCol(t, "correlationId") {
		return nil // nothing to place them by
	}
	typeCol := l.firstCol(t, "0", "syncType", "deprecatedSyncType")
	names := map[int64]int32{}
	q := `SELECT start, end, correlationId, ` + typeCol + ` FROM ` + t + where("", rng)
	return c.query(q, args, func(s *stmt) error {
		typ := s.int64(3)
		n, ok := names[typ]
		if !ok {
			label, ok := l.syncLabels[typ]
			if !ok {
				label = "Sync type " + strconv.FormatInt(typ, 10)
			}
			n = sh.Intern(label)
			names[typ] = n
		}
		sh.AddSync(Event{Start: s.int64(0), End: s.int64(1), Name: n, Corr: int32(s.int64(2))})
		return nil
	})
}

func (l *loader) readNVTX(c *conn, sh *Builder, rng string, args []int64) error {
	const t = tableNVTX
	conds := ""
	if l.hasCol(t, "eventType") {
		conds = fmt.Sprintf(`eventType IN (%d,%d,%d,%d,%d)`, nvtxMark, nvtxPushPopRange, nvtxStartEndRange, nvtxPushPopPayload, nvtxStartEndPayload)
	}
	q := `SELECT start, end, ` + l.col(t, "eventType", "59") + `, ` + l.col(t, "globalTid", "0") + `, ` +
		l.col(t, "text", "NULL") + `, ` + l.col(t, "textId", "NULL") + ` FROM ` + t + where(conds, rng)
	unnamed := int32(-1)
	return c.query(q, args, func(s *stmt) error {
		start, end := s.int64(0), s.int64(1)
		kind := KindNVTX
		switch s.int64(2) {
		case nvtxPushPopRange, nvtxStartEndRange, nvtxPushPopPayload, nvtxStartEndPayload:
			if s.null(1) {
				return nil // a range never closed
			}
		case nvtxMark:
			kind = KindNVTXMark
			end = start
		default:
			return nil // categories, domains, thread names
		}
		var name int32
		switch {
		case !s.null(4):
			name = sh.internBytes(s.bytes(4))
		case !s.null(5):
			name = sh.nameID(s.int64(5))
		default:
			if unnamed < 0 {
				unnamed = sh.Intern("(unnamed)")
			}
			name = unnamed
		}
		sh.AddNVTX(s.int64(3), Event{Start: start, End: end, Name: name, Kind: kind})
		return nil
	})
}

// resolveNames reads the strings of the StringIds ids the shards and the
// thread names refer to, and fills them in.
func (l *loader) resolveNames(shards []*Builder) {
	seen := map[int64]bool{}
	var ids []int64
	need := func(id int64) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, sh := range shards {
		for _, id := range sh.ids {
			if id >= 0 {
				need(id)
			}
		}
	}
	for _, id := range l.threadNameIDs {
		need(id)
	}
	slices.Sort(ids)
	strs := make(map[int64]string, len(ids))
	if len(ids) > 0 {
		if !l.hasCol("StringIds", "id") || !l.hasCol("StringIds", "value") {
			l.warn("no StringIds table: names will show as ids")
		} else {
			const batch = 500
			for i := 0; i < len(ids); i += batch {
				var q strings.Builder
				q.WriteString(`SELECT id, value FROM StringIds WHERE id IN (`)
				for k, id := range ids[i:min(i+batch, len(ids))] {
					if k > 0 {
						q.WriteByte(',')
					}
					q.WriteString(strconv.FormatInt(id, 10))
				}
				q.WriteByte(')')
				l.query("StringIds", q.String(), func(s *stmt) { strs[s.int64(0)] = s.text(1) })
			}
		}
	}
	str := func(id int64) string {
		if s, ok := strs[id]; ok {
			return s
		}
		return "<string " + strconv.FormatInt(id, 10) + ">"
	}
	for _, sh := range shards {
		for i, id := range sh.ids {
			if id >= 0 {
				sh.t.Names[i] = str(id)
			}
		}
	}
	for gtid, id := range l.threadNameIDs {
		l.b.t.ThreadNames[gtid] = str(id)
	}
}

// readInfo reads when the capture started (TARGET_INFO_SESSION_START_TIME)
// and on which host (TARGET_INFO_SYSTEM_ENV), where the export says.
func readInfo(c *conn, hasCol func(table, col string) bool) (info Info) {
	if hasCol("TARGET_INFO_SESSION_START_TIME", "utcEpochNs") {
		c.query(`SELECT utcEpochNs FROM TARGET_INFO_SESSION_START_TIME WHERE utcEpochNs IS NOT NULL LIMIT 1`, nil, func(s *stmt) error {
			info.SessionStartUTC = s.int64(0)
			return nil
		})
	}
	if hasCol("TARGET_INFO_SESSION_START_TIME", "systemClockNs") {
		c.query(`SELECT systemClockNs FROM TARGET_INFO_SESSION_START_TIME WHERE systemClockNs IS NOT NULL LIMIT 1`, nil, func(s *stmt) error {
			info.SessionStartMono = s.int64(0)
			return nil
		})
	}
	var host string
	if hasCol("TARGET_INFO_SYSTEM_ENV", "name") && hasCol("TARGET_INFO_SYSTEM_ENV", "value") {
		c.query(`SELECT value FROM TARGET_INFO_SYSTEM_ENV WHERE name = 'Hostname' AND value IS NOT NULL LIMIT 1`, nil, func(s *stmt) error {
			host = s.text(0)
			return nil
		})
	}
	info.Host = host
	return info
}

// ReadInfo reads when and where a trace was captured, without loading its
// events: from a SQLite export's metadata tables, or for a report, from
// its export if there is one (the native decoder does not read the
// report's clock).
func (o OpenOptions) ReadInfo(path string) Info {
	if Detect(path) == FormatTorch {
		if file, rank, ok := SplitMember(path); ok {
			// A rank of a merged trace: read with the others (Members).
			trs, err := loadTorchCached(file, nil)
			for _, tr := range trs {
				if err == nil && tr.Rank == rank {
					return Info{SessionStartUTC: tr.SessionStartUTC, SessionStartMono: tr.SessionStartMono, Rank: rank, Clock: tr.Clock}
				}
			}
			return Info{Rank: rank}
		}
		return readTorchInfo(path)
	}
	if Detect(path) == FormatReport {
		// The report says its start on the host's monotonic clock and the
		// wall clock, and its host; an export, where the report does not.
		var info Info
		if s, err := nsysrep.ReadSession(path); err == nil {
			info = Info{SessionStartUTC: s.WallStart, SessionStartMono: s.MonoStart, Host: s.Host}
		}
		if c, ok := o.Converted(path); ok && info.SessionStartUTC == 0 {
			ex := o.ReadInfo(c)
			info.SessionStartUTC = ex.SessionStartUTC
			info.SessionStartMono = cmp.Or(info.SessionStartMono, ex.SessionStartMono)
			info.Host = cmp.Or(info.Host, ex.Host)
		}
		return info
	}
	c, err := openReadOnly(path)
	if err != nil {
		return Info{}
	}
	defer c.close()
	cols := map[string]map[string]bool{}
	hasCol := func(table, col string) bool {
		m, ok := cols[table]
		if !ok {
			m = map[string]bool{}
			c.query(`SELECT name FROM pragma_table_info('`+strings.ReplaceAll(table, "'", "''")+`')`, nil, func(s *stmt) error {
				m[s.text(0)] = true
				return nil
			})
			cols[table] = m
		}
		return m[col]
	}
	return readInfo(c, hasCol)
}
