package trace

import (
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
)

// The real trace: an export (nsys export --type sqlite) of an actual
// Nsight Systems run of your own, named by TRACELIGHT_TRACE; none is in
// the repository. Tests that need it skip without it.
var (
	realOnce  sync.Once
	realTrace *Trace
	realErr   error
)

func realPath() string { return os.Getenv("TRACELIGHT_TRACE") }

func loadReal(t *testing.T) *Trace {
	t.Helper()
	if _, err := os.Stat(realPath()); realPath() == "" || err != nil {
		t.Skip("no real trace (set TRACELIGHT_TRACE to an nsys SQLite export)")
	}
	realOnce.Do(func() { realTrace, realErr = LoadSQLite(realPath()) })
	if realErr != nil {
		t.Fatal(realErr)
	}
	return realTrace
}

// The parallel reader loads exactly the rows the tables hold, as counted
// by plain SQL through database/sql.
func TestRealTraceCounts(t *testing.T) {
	tr := loadReal(t)
	db, err := sql.Open("sqlite", "file:"+realPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	has := func(table string) bool {
		return count(`SELECT count(*) FROM sqlite_master WHERE name = '`+table+`'`) > 0
	}
	want := map[Kind]int{}
	if has(tableKernel) {
		want[KindKernel] = count(`SELECT count(*) FROM ` + tableKernel)
	}
	if has(tableMemcpy) {
		want[KindMemcpy] = count(`SELECT count(*) FROM ` + tableMemcpy)
	}
	if has(tableMemset) {
		want[KindMemset] = count(`SELECT count(*) FROM ` + tableMemset)
	}
	if has(tableRuntime) {
		want[KindRuntime] = count(`SELECT count(*) FROM ` + tableRuntime)
	}
	if has(tableSync) && has(tableRuntime) {
		want[KindSync] = count(`SELECT count(*) FROM ` + tableSync + ` WHERE correlationId IN (SELECT correlationId FROM ` + tableRuntime + `)`)
	}
	if has(tableNVTX) {
		want[KindNVTX] = count(`SELECT count(*) FROM NVTX_EVENTS WHERE eventType IN (59,60,70,71) AND end IS NOT NULL`)
		want[KindNVTXMark] = count(`SELECT count(*) FROM NVTX_EVENTS WHERE eventType = 34`)
	}
	for k, n := range want {
		if tr.Counts[k] != n {
			t.Errorf("%s: loaded %d, the table has %d", k, tr.Counts[k], n)
		}
	}
	if len(tr.Warnings) != 0 {
		t.Errorf("warnings: %v", tr.Warnings)
	}
	if tr.Counts[KindKernel] == 0 || tr.Counts[KindRuntime] == 0 || tr.Counts[KindNVTX] == 0 {
		t.Errorf("expected kernels, API calls and NVTX ranges: %v", tr.Counts)
	}
}

// Every name is resolved from StringIds, every lane is sorted and free of
// overlap, and the prefix sums add up.
func TestRealTraceStructure(t *testing.T) {
	tr := loadReal(t)
	for _, n := range tr.Names {
		if strings.HasPrefix(n, "<string ") {
			t.Fatalf("unresolved name %q", n)
		}
	}
	events := 0
	for _, tk := range tr.Tracks {
		for li, l := range tk.Lanes {
			for i := range l.Events {
				e := &l.Events[i]
				if e.End < e.Start {
					t.Fatalf("%s lane %d: event ends before it starts", tk.Name, li)
				}
				if i > 0 && e.Start < l.Events[i-1].End {
					t.Fatalf("%s lane %d: events %d and %d overlap", tk.Name, li, i-1, i)
				}
			}
			if l.BusyBetween(0, l.Len()) < 0 {
				t.Fatalf("%s: negative busy time", tk.Name)
			}
			events += l.Len()
		}
	}
	if events != tr.EventCount() {
		t.Errorf("lanes hold %d events, tracks count %d", events, tr.EventCount())
	}
}

// Kernels on the GPUs that ran them; GPUs listed without activity are idle.
func TestRealTraceIdleDevices(t *testing.T) {
	tr := loadReal(t)
	active := map[int32]bool{}
	for _, tk := range tr.Tracks {
		if tk.Kind == TrackStream {
			active[tk.Device] = true
		}
	}
	idle := tr.IdleDevices()
	if len(idle)+len(active) < len(tr.Devices) {
		t.Errorf("%d idle + %d active GPUs, %d listed", len(idle), len(active), len(tr.Devices))
	}
	for _, id := range idle {
		if active[id] {
			t.Errorf("GPU %d is active but reported idle", id)
		}
	}
}

// Most GPU work links back to the API call that launched it.
func TestRealTraceCorrelation(t *testing.T) {
	tr := loadReal(t)
	linked, total := 0, 0
	for ti, tk := range tr.Tracks {
		if tk.Kind != TrackStream {
			continue
		}
		for li, l := range tk.Lanes {
			for i := 0; i < l.Len(); i += 97 { // a sample
				total++
				r := Ref{int32(ti), int32(li), int32(i)}
				if api, ok := tr.Correlated(r); ok {
					if tr.Event(api).Kind != KindRuntime {
						t.Fatal("a kernel correlates to something other than an API call")
					}
					if back, ok := tr.Correlated(api); !ok || tr.Event(back).Corr != tr.Event(r).Corr {
						t.Fatal("the API call does not correlate back")
					}
					linked++
				}
			}
		}
	}
	if total > 0 && linked*10 < total*9 {
		t.Errorf("only %d of %d sampled GPU events link to an API call", linked, total)
	}
}

// LOD accounts for every event of the real trace's busiest lanes, at every
// zoom from the whole trace down to microseconds.
func TestRealTraceLOD(t *testing.T) {
	tr := loadReal(t)
	for _, tk := range tr.Tracks {
		if tk.Count < 50_000 {
			continue
		}
		l := tk.Lanes[0]
		for span := float64(tr.Duration()); span > 1000; span /= 7 {
			mid := float64(l.Events[l.Len()/2].Start)
			t0, t1 := int64(mid-span/2), int64(mid+span/2)
			i, j := l.Visible(t0, t1)
			n, busy := 0, int64(0)
			for _, b := range l.LOD(t0, t1, span/1600, 1, 5, nil) {
				n += b.Count
				busy += b.Busy
			}
			if n != j-i || busy != l.BusyBetween(i, j) {
				t.Fatalf("%s at %.0f ns: LOD covers %d events (%d ns), want %d (%d ns)", tk.Name, span, n, busy, j-i, l.BusyBetween(i, j))
			}
		}
	}
}

func BenchmarkLoadRealTrace(b *testing.B) {
	if _, err := os.Stat(realPath()); err != nil {
		b.Skip("no real trace")
	}
	for b.Loop() {
		if _, err := LoadSQLite(realPath()); err != nil {
			b.Fatal(err)
		}
	}
}
