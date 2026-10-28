package trace

import (
	"cmp"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tracelight/internal/nsysrep"
)

// The real report: the .nsys-rep the real trace (realPath) was exported
// from, named by TRACELIGHT_REPORT; none is in the repository.
func realReportPath() string { return os.Getenv("TRACELIGHT_REPORT") }

// flatEvent is an event with its names and details resolved, to compare
// traces whose name indexes differ.
type flatEvent struct {
	start, end int64
	name, full string
	corr       int32
	kind       Kind
	device     int32
	stream     int32
	bytes      int64
	copyKind   int32
	grid       [3]int32
	block      [3]int32
	regs       int32
}

func flatten(tr *Trace) map[trackKey][]flatEvent {
	out := map[trackKey][]flatEvent{}
	for _, track := range tr.Tracks {
		var evs []flatEvent
		for _, l := range track.Lanes {
			for i := range l.Events {
				e := &l.Events[i]
				f := flatEvent{start: e.Start, end: e.End, name: tr.Name(e), corr: e.Corr, kind: e.Kind}
				switch e.Kind {
				case KindKernel:
					k := tr.Kernels[e.Extra]
					f.full = tr.Names[k.FullName]
					f.device, f.stream = k.Device, k.Stream
					f.grid = [3]int32{k.GridX, k.GridY, k.GridZ}
					f.block = [3]int32{k.BlockX, k.BlockY, k.BlockZ}
					f.regs = k.Registers
				case KindMemcpy, KindMemset:
					m := tr.MemOps[e.Extra]
					f.device, f.stream, f.bytes, f.copyKind = m.Device, m.Stream, m.Bytes, m.CopyKind
				}
				evs = append(evs, f)
			}
		}
		slices.SortFunc(evs, func(a, b flatEvent) int {
			return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end), cmp.Compare(a.corr, b.corr),
				cmp.Compare(a.kind, b.kind), cmp.Compare(a.name, b.name))
		})
		out[track.k] = evs
	}
	return out
}

// The native decoder makes the same trace of the real report as the SQLite
// loader makes of its export: same tracks, and in each the same events with
// the same times, names, correlation ids and details. Failures report
// counts and positions only, never names: the trace is private.
func TestNativeMatchesExport(t *testing.T) {
	if _, err := os.Stat(realReportPath()); err != nil {
		t.Skipf("no real report at %s (set TRACELIGHT_REPORT)", realReportPath())
	}
	want := loadReal(t)
	got, err := LoadReport(realReportPath(), nsysrep.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for k := Kind(1); int(k) < kindCount; k++ {
		if got.Counts[k] != want.Counts[k] {
			t.Errorf("%v: %d events natively, %d from the export", k, got.Counts[k], want.Counts[k])
		}
	}
	if got.Start != want.Start || got.End != want.End {
		t.Errorf("bounds differ by %d and %d ns", got.Start-want.Start, got.End-want.End)
	}
	gf, wf := flatten(got), flatten(want)
	if len(gf) != len(wf) {
		t.Errorf("%d tracks natively, %d from the export", len(gf), len(wf))
	}
	compared := 0
	for key, w := range wf {
		g, ok := gf[key]
		if !ok {
			t.Errorf("track kind %d missing natively (%d events)", key.kind, len(w))
			continue
		}
		if len(g) != len(w) {
			t.Errorf("track kind %d: %d events natively, %d from the export", key.kind, len(g), len(w))
			continue
		}
		bad, first := 0, -1
		for i := range w {
			if g[i] != w[i] {
				if first < 0 {
					first = i
				}
				bad++
			}
		}
		if bad > 0 {
			t.Errorf("track kind %d: %d of %d events differ, the first at %d", key.kind, bad, len(w), first)
		}
		compared += len(w)
	}
	t.Logf("compared %d events on %d tracks", compared, len(wf))

	sameMap := func(what string, n int, eq bool) {
		if !eq {
			t.Errorf("%s differ (%d natively)", what, n)
		}
	}
	sameMap("device names", len(got.Devices), mapsEqual(got.Devices, want.Devices))
	sameMap("thread names", len(got.ThreadNames), mapsEqual(got.ThreadNames, want.ThreadNames))
	sameMap("process names", len(got.ProcessNames), mapsEqual(got.ProcessNames, want.ProcessNames))
	if len(got.Warnings) != len(want.Warnings) {
		t.Errorf("%d warnings natively, %d from the export", len(got.Warnings), len(want.Warnings))
	}
}

func mapsEqual[K comparable](a, b map[K]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// The report's string table is the export's StringIds: same ids, same
// strings.
func TestNativeStringIds(t *testing.T) {
	if _, err := os.Stat(realReportPath()); err != nil {
		t.Skipf("no real report at %s (set TRACELIGHT_REPORT)", realReportPath())
	}
	if _, err := os.Stat(realPath()); err != nil {
		t.Skipf("no real trace at %s", realPath())
	}
	f, err := nsysrep.Open(realReportPath(), nsysrep.Options{})
	if err != nil {
		t.Fatal(err)
	}
	strs, err := f.Strings()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+realPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, value FROM StringIds`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n, bad := 0, 0
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		n++
		if id < 0 || id >= int64(strs.Len()) || strs.At(int(id)) != v {
			bad++
		}
	}
	if bad > 0 || n != strs.Len() {
		t.Errorf("%d of %d StringIds differ; %d strings natively", bad, n, strs.Len())
	}
}

// Reports the native decoder cannot read go through export, and
// TRACELIGHT_NATIVE=off skips the decoder.
func TestReportFallback(t *testing.T) {
	p := filepath.Join(t.TempDir(), "odd.nsys-rep")
	if err := os.WriteFile(p, []byte("NVIDIA Tegra Profiler Report 1999@1@0@1v0.garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	exported := 0
	export := func(string) (string, error) { exported++; return "", errors.New("no nsys") }
	if _, err := loadReport(p, export, nil); err == nil || exported != 1 || !strings.Contains(err.Error(), "native decoder") {
		t.Errorf("unverified version: exported %d times, err %v", exported, err)
	}
	t.Setenv(NativeEnv, "off")
	if _, err := loadReport(p, export, nil); err == nil || exported != 2 || strings.Contains(err.Error(), "native decoder") {
		t.Errorf("native off: exported %d times, err %v", exported, err)
	}
	if _, err := loadReport(filepath.Join(t.TempDir(), "missing.nsys-rep"), export, nil); err == nil {
		t.Error("missing report opened")
	}
}
