package trace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"tracelight/internal/nsysrep"
)

// OpenWith decides by Detect: a SQLite export opens whatever its name, a
// file named .sqlite that is not one fails as not a database, and other
// files are not traces.
func TestOpenDispatch(t *testing.T) {
	dir := t.TempDir()
	db := makeDB(t,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (start INTEGER, end INTEGER, globalTid INTEGER, nameId INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_RUNTIME VALUES (5, 10, 1, 1)`)
	data, _ := os.ReadFile(db)
	odd := filepath.Join(dir, "run.db")
	os.WriteFile(odd, data, 0o644)
	fake := filepath.Join(dir, "fake.sqlite")
	os.WriteFile(fake, []byte("not a database at all, just text"), 0o644)
	notes := filepath.Join(dir, "notes.txt")
	os.WriteFile(notes, []byte("hello"), 0o644)

	cases := []struct {
		path string
		want Format
	}{
		{odd, FormatSQLite}, {db, FormatSQLite}, {fake, FormatSQLite},
		{notes, ""}, {filepath.Join(dir, "x.nsys-rep"), FormatReport}, {filepath.Join(dir, "missing.bin"), ""},
	}
	for _, c := range cases {
		if got := Detect(c.path); got != c.want {
			t.Errorf("Detect(%s) = %q, want %q", filepath.Base(c.path), got, c.want)
		}
	}
	if tr, err := Open(odd); err != nil || tr.Counts[KindRuntime] != 1 {
		t.Errorf("a SQLite export named .db: %v", err)
	}
	if _, err := Open(fake); err == nil || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Errorf("a text file named .sqlite: %v", err)
	}
	if _, err := Open(notes); err == nil || !strings.Contains(err.Error(), "not a trace") {
		t.Errorf("a text file: %v", err)
	}
	if f, ready, _ := (OpenOptions{}).Probe(odd); f != FormatSQLite || !ready {
		t.Errorf("Probe of an export: %q %v", f, ready)
	}
	if f, ready, note := (OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: dir}).Probe(filepath.Join(dir, "x.nsys-rep")); f != FormatReport || ready || note == "" {
		t.Errorf("Probe of a report without nsys: %q %v %q", f, ready, note)
	}
}

// A report the native decoder reads is ready without nsys, and opens.
func TestProbeNativeReport(t *testing.T) {
	report := realReportPath()
	if _, err := os.Stat(report); report == "" || err != nil {
		t.Skip("no real report")
	}
	o := OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}
	if !nativeReady(report) {
		t.Skip("the real report's version is not one the native decoder verifies")
	}
	if f, ready, note := o.Probe(report); f != FormatReport || !ready || note != "read natively" {
		t.Errorf("Probe: %q %v %q", f, ready, note)
	}
	if testing.Short() {
		return
	}
	tr, err := OpenWith(report, o)
	if err != nil || tr.EventCount() == 0 {
		t.Fatalf("OpenWith without nsys: %v", err)
	}
	for _, w := range tr.Warnings {
		if strings.Contains(w, "nsys export") {
			t.Errorf("read through nsys export, not natively: %s", w)
		}
	}
}

// The capture's clock and host come from the export's metadata, through a
// full load and through ReadInfo alike.
func TestSessionInfo(t *testing.T) {
	path := makeDB(t,
		`CREATE TABLE TARGET_INFO_SESSION_START_TIME (utcEpochNs INTEGER, utcTime TEXT, systemClockNs INTEGER)`,
		`INSERT INTO TARGET_INFO_SESSION_START_TIME VALUES (1792411200123456789, 'x', 1515000000000000)`,
		`CREATE TABLE TARGET_INFO_SYSTEM_ENV (name TEXT, value TEXT)`,
		`INSERT INTO TARGET_INFO_SYSTEM_ENV VALUES ('OsDescription', 'Linux'), ('Hostname', 'node7')`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (start INTEGER, end INTEGER, globalTid INTEGER, nameId INTEGER)`,
		`INSERT INTO CUPTI_ACTIVITY_KIND_RUNTIME VALUES (5, 10, 1, 1)`)
	tr, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if tr.SessionStartUTC != 1792411200123456789 || tr.SessionStartMono != 1515000000000000 || tr.Host != "node7" {
		t.Errorf("loaded: %d %d %q", tr.SessionStartUTC, tr.SessionStartMono, tr.Host)
	}
	if info := (OpenOptions{}).ReadInfo(path); info.SessionStartUTC != tr.SessionStartUTC || info.SessionStartMono != tr.SessionStartMono || info.Host != "node7" {
		t.Errorf("ReadInfo: %+v", info)
	}
	// Exports without those tables: zero, no error.
	bare := makeDB(t, `CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (start INTEGER, end INTEGER, globalTid INTEGER, nameId INTEGER)`)
	if info := (OpenOptions{}).ReadInfo(bare); info != (Info{}) {
		t.Errorf("bare: %+v", info)
	}
	if info := (OpenOptions{}).ReadInfo(filepath.Join(t.TempDir(), "missing.sqlite")); info != (Info{}) {
		t.Errorf("missing: %+v", info)
	}
}

// The environment's names before the rename still work, after the new ones.
func TestGetenvFallback(t *testing.T) {
	t.Setenv("NSIGHT_ALT_X_TEST", "old")
	if Getenv("X_TEST") != "old" {
		t.Error("the old name is not a fallback")
	}
	t.Setenv("TRACELIGHT_X_TEST", "new")
	if Getenv("X_TEST") != "new" {
		t.Error("the new name does not come first")
	}
}

// A report read natively says its start on the host's monotonic clock and
// its host, loaded or read cheaply; not the wall clock, which it does not
// store (an export next to it gives that).
func TestReportSessionInfo(t *testing.T) {
	sess := nsysrep.Session{MonoStart: 1_515_000_123_456_789, Host: "node3"}
	path := filepath.Join(t.TempDir(), "rank3.nsys-rep")
	if err := os.WriteFile(path, nsysrep.Synthetic("2026@3@7@1-2", sess), 0o644); err != nil {
		t.Fatal(err)
	}
	o := OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}
	tr, err := OpenWith(path, o)
	if err != nil {
		t.Fatal(err)
	}
	if tr.SessionStartMono != sess.MonoStart || tr.Host != "node3" || tr.SessionStartUTC != 0 {
		t.Errorf("loaded: mono %d host %q utc %d", tr.SessionStartMono, tr.Host, tr.SessionStartUTC)
	}
	if info := o.ReadInfo(path); info != (Info{SessionStartMono: sess.MonoStart, Host: "node3"}) {
		t.Errorf("ReadInfo: %+v", info)
	}
}

// Collective kernels: NCCL's collectives by operation, not its
// point-to-point kernels nor other kernels; their ends from the first
// event, sorted.
func TestCollectives(t *testing.T) {
	for name, want := range map[string]string{
		"ncclDevKernel_AllReduce_Sum_f32_RING_LL":          "allreduce",
		"ncclKernel_AllGather_RING_LL_Sum_int8_t":          "allgather",
		"ncclDevKernel_ReduceScatter_Sum_bf16_RING_LL":     "reducescatter",
		"ncclDevKernel_Broadcast_RING_LL":                  "broadcast",
		"ncclDevKernel_Reduce_Sum_f32_RING_LL":             "reduce",
		"ncclDevKernel_SendRecv":                           "",
		"ampere_sgemm_128x128_tn":                          "",
		"void allreduce_custom_kernel<float>(float*, int)": "",
	} {
		if got := CollectiveOp(name); got != want {
			t.Errorf("CollectiveOp(%q) = %q, want %q", name, got, want)
		}
	}
	b := NewBuilder("x")
	ar, gemm := b.Intern("ncclDevKernel_AllReduce_Sum_f32_RING_LL"), b.Intern("gemm")
	b.AddKernel(Event{Start: 1000, End: 1500, Name: gemm}, KernelInfo{Stream: 7})
	b.AddKernel(Event{Start: 3000, End: 9000, Name: ar}, KernelInfo{Stream: 13})
	b.AddKernel(Event{Start: 1200, End: 2000, Name: ar}, KernelInfo{Stream: 13})
	tr := b.Finish()
	got := tr.Collectives()
	if len(got) != 1 || !slices.Equal(got["allreduce"], []int64{1000, 8000}) {
		t.Errorf("Collectives: %v", got)
	}
}
