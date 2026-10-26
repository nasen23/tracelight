package trace

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // writes the test databases
)

// makeDB writes a database from SQL statements.
func makeDB(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return path
}

func TestLoadSQLite(t *testing.T) {
	path := makeDB(t,
		`CREATE TABLE StringIds (id INTEGER PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO StringIds VALUES (1,'gemm'),(2,'void gemm<float>(float*)'),(3,'cudaLaunchKernel_v7000'),(4,'registered'),(5,'main-thread')`,
		`CREATE TABLE ThreadNames (nameId INTEGER, priority INTEGER, globalTid INTEGER)`,
		`INSERT INTO ThreadNames VALUES (5, 0, 72057595715649637)`, // pid 100, tid 101
		`CREATE TABLE TARGET_INFO_GPU (id INTEGER, name TEXT)`,
		`INSERT INTO TARGET_INFO_GPU VALUES (0, 'Test GPU')`,
		`CREATE TABLE ENUM_CUDA_MEMCPY_OPER (id INTEGER PRIMARY KEY, name TEXT, label TEXT)`,
		`INSERT INTO ENUM_CUDA_MEMCPY_OPER VALUES (1,'CUDA_MEMCPY_KIND_HTOD','Host-to-Device')`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_KERNEL (start INTEGER, end INTEGER, deviceId INTEGER, streamId INTEGER,
			correlationId INTEGER, demangledName INTEGER, shortName INTEGER, gridX INTEGER, blockX INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_KERNEL VALUES (1000, 2000, 0, 7, 11, 2, 1, 64, 128), (2500, 2600, 0, 7, 12, 2, 1, 1, 32)`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_MEMCPY (start INTEGER, end INTEGER, deviceId INTEGER, streamId INTEGER,
			correlationId INTEGER, bytes INTEGER, copyKind INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_MEMCPY VALUES (500, 900, 0, 14, 10, 4096, 1)`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (start INTEGER, end INTEGER, globalTid INTEGER, correlationId INTEGER, nameId INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_RUNTIME VALUES (400, 450, 72057595715649637, 10, 3), (800, 820, 72057595715649637, 11, 3), (900, 910, 72057595715649637, 12, 3)`,
		`CREATE TABLE NVTX_EVENTS (start INTEGER, end INTEGER, eventType INTEGER, text TEXT, globalTid INTEGER, textId INTEGER)`,
		`INSERT INTO NVTX_EVENTS VALUES
			(300, 3000, 59, 'outer', 72057595715649637, NULL),
			(350, 1000, 59, NULL, 72057595715649637, 4),
			(1100, 1200, 59, 'sibling', 72057595715649637, NULL),
			(1150, 1180, 59, 'inner', 72057595715649637, NULL),
			(1500, NULL, 34, 'mark', 72057595715649637, NULL),
			(1600, NULL, 59, 'never closed', 72057595715649637, NULL),
			(0, NULL, 75, 'domain', 72057595715649637, NULL)`,
	)
	tr, err := LoadSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Missing) != 1 || tr.Missing[0] != tableMemset {
		t.Errorf("Missing = %v, want only the memset table", tr.Missing)
	}
	if len(tr.Warnings) != 0 {
		t.Errorf("warnings: %v", tr.Warnings)
	}
	if got := tr.Counts; got[KindKernel] != 2 || got[KindMemcpy] != 1 || got[KindRuntime] != 3 || got[KindNVTX] != 4 || got[KindNVTXMark] != 1 {
		t.Errorf("counts %v", got)
	}
	if tr.Start != 300 || tr.End != 3000 {
		t.Errorf("bounds %d..%d, want 300..3000", tr.Start, tr.End)
	}

	byName := map[string]*Track{}
	for _, tk := range tr.Tracks {
		byName[tk.Group+"/"+tk.Name] = tk
	}
	s7 := byName["GPU 0 · Test GPU/Stream 7"]
	if s7 == nil {
		t.Fatalf("no stream 7 track; tracks: %v", keys(byName))
	}
	k := &s7.Lanes[0].Events[0]
	if tr.Name(k) != "gemm" || tr.Names[tr.Kernels[k.Extra].FullName] != "void gemm<float>(float*)" || tr.Kernels[k.Extra].GridX != 64 {
		t.Errorf("kernel %q %+v", tr.Name(k), tr.Kernels[k.Extra])
	}
	s14 := byName["GPU 0 · Test GPU/Stream 14"]
	if s14 == nil || tr.Name(&s14.Lanes[0].Events[0]) != "Memcpy HtoD" || tr.MemOps[0].Bytes != 4096 {
		t.Fatalf("memcpy track wrong: %v", keys(byName))
	}

	nvtx := byName["Process 100/main-thread (101) · NVTX"]
	if nvtx == nil {
		t.Fatalf("no NVTX track; tracks: %v", keys(byName))
	}
	// outer at depth 0; registered and sibling at 1; inner at 2. The mark is
	// zero-length and fits in lane 0 (nothing else there ends after it... it
	// is inside outer), so it goes deeper; check names by depth instead.
	depth := map[string]int{}
	for li, l := range nvtx.Lanes {
		for i := range l.Events {
			depth[tr.Name(&l.Events[i])] = li
		}
	}
	want := map[string]int{"outer": 0, "registered": 1, "sibling": 1, "inner": 2}
	for n, d := range want {
		if got, ok := depth[n]; !ok || got != d {
			t.Errorf("%s at depth %d (found %v), want %d", n, got, ok, d)
		}
	}
	if _, ok := depth["never closed"]; ok {
		t.Error("an unclosed range was loaded")
	}

	// Correlation links the launch to its kernel and back.
	api := byName["Process 100/main-thread (101) · CUDA API"]
	if api == nil {
		t.Fatal("no CUDA API track")
	}
	ti := -1
	for i, tk := range tr.Tracks {
		if tk == api {
			ti = i
		}
	}
	gpu, ok := tr.Correlated(Ref{int32(ti), 0, 1})
	if !ok || tr.Event(gpu).Start != 1000 {
		t.Fatalf("launch at 800 correlates to %+v %v", gpu, ok)
	}
	back, ok := tr.Correlated(gpu)
	if !ok || back != (Ref{int32(ti), 0, 1}) {
		t.Errorf("kernel correlates back to %+v %v", back, ok)
	}

	top := tr.TopByTotal(5, KindKernel)
	if len(top) != 1 || top[0].Count != 2 || top[0].Total != 1100 || top[0].Min != 100 || top[0].Max != 1000 || top[0].Share != 1 {
		t.Errorf("top kernels %+v", top)
	}
}

// A file with none of the event tables still loads, empty.
func TestLoadSQLiteNoEventTables(t *testing.T) {
	path := makeDB(t, `CREATE TABLE Unrelated (x INTEGER)`)
	tr, err := LoadSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Tracks) != 0 || len(tr.Missing) != 5 {
		t.Errorf("tracks %d, missing %v", len(tr.Tracks), tr.Missing)
	}
}

// A kernel table of an unexpected shape is a warning, not a failure.
func TestLoadSQLiteBadTable(t *testing.T) {
	path := makeDB(t,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_KERNEL (start INTEGER, end INTEGER)`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (start INTEGER, end INTEGER, globalTid INTEGER, nameId INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_RUNTIME VALUES (5, 10, 1, 99)`,
	)
	tr, err := LoadSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Warnings) == 0 {
		t.Error("no warning for a kernel table without names")
	}
	if tr.Counts[KindRuntime] != 1 || tr.Names[1] != "<string 99>" {
		t.Errorf("runtime not loaded: %v %v", tr.Counts, tr.Names)
	}
}

func TestLoadNotADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	if err := writeFile(path, "this is not sqlite, just some text that is long enough to have a header"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSQLite(path); err == nil {
		t.Error("no error for a text file")
	}
}

func TestPidTid(t *testing.T) {
	// From a real export: PROCESSES maps this globalPid to pid 2830693.
	if pid, tid := PidTid(328966124601344); pid != 2830693 || tid != 0 {
		t.Errorf("PidTid = %d, %d", pid, tid)
	}
}

func TestSampleFile(t *testing.T) {
	path := filepath.Join("..", "..", "samples", "sample.sqlite")
	if !exists(path) {
		t.Skip("run go run ./cmd/gensample first")
	}
	tr, err := LoadSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := tr.EventCount(); n < 150_000 {
		t.Errorf("only %d events", n)
	}
	if len(tr.Missing) != 0 || len(tr.Warnings) != 0 {
		t.Errorf("missing %v warnings %v", tr.Missing, tr.Warnings)
	}
	for _, tk := range tr.Tracks {
		for _, l := range tk.Lanes {
			for i := 1; i < len(l.Events); i++ {
				if l.Events[i].Start < l.Events[i-1].End {
					t.Fatalf("%s: events overlap in a lane", tk.Name)
				}
			}
		}
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
