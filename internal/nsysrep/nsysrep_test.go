package nsysrep

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// synthetic builds a report of the given header version (Synthetic).
func synthetic(version string) []byte { return Synthetic(version, Session{}) }

func TestSynthetic(t *testing.T) {
	f, err := Parse(synthetic("2026@3@7@1-2"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != "2026.3.7" {
		t.Errorf("version %q", f.Version)
	}
	var (
		kernels []Kernel
		memcpys []Memcpy
		memsets []Memset
		syncs   []Sync
		apis    []API
		nvtx    []NVTX
		procs   = map[int64]string{}
	)
	v := &Visitor{
		Kernel: func(k *Kernel) { kernels = append(kernels, *k) },
		Memcpy: func(m *Memcpy) { memcpys = append(memcpys, *m) },
		Memset: func(m *Memset) { memsets = append(memsets, *m) },
		Sync:   func(s *Sync) { syncs = append(syncs, *s) },
		API:    func(a *API) { apis = append(apis, *a) },
		NVTX: func(n *NVTX) {
			c := *n
			c.Text = slices.Clone(n.Text)
			nvtx = append(nvtx, c)
		},
		Process: func(p *Process) { procs[p.GlobalPid] = string(p.Name) },
	}
	var buf []byte
	for _, i := range f.EventFrames() {
		if buf, err = f.DecodeEvents(i, buf, v); err != nil {
			t.Fatal(err)
		}
	}
	wantK := Kernel{GPU: GPU{Start: 100, End: 200, Correlation: 5, Device: 1, Context: 1, Stream: 7, GlobalPid: 0x10 << 24},
		DemangledName: 2, ShortName: 1, MangledName: 3, GridX: 64, GridY: 1, GridZ: 1, BlockX: 128, BlockY: 1, BlockZ: 1,
		DynamicShared: 48, Registers: 32}
	if len(kernels) != 1 || kernels[0] != wantK {
		t.Errorf("kernels %+v", kernels)
	}
	if len(memcpys) != 1 || memcpys[0].Bytes != 4096 || memcpys[0].CopyKind != 1 || memcpys[0].DstKind != 2 || memcpys[0].Correlation != 6 {
		t.Errorf("memcpys %+v", memcpys)
	}
	if len(memsets) != 1 || memsets[0].Bytes != 512 || memsets[0].Value != 9 || memsets[0].MemKind != 2 {
		t.Errorf("memsets %+v", memsets)
	}
	if len(syncs) != 1 || syncs[0].SyncType != 3 || syncs[0].EventID != 11 || syncs[0].Correlation != 8 {
		t.Errorf("syncs %+v", syncs)
	}
	if len(apis) != 1 || apis[0] != (API{Start: 90, End: 95, Correlation: 5, GlobalTid: 0x10<<24 | 0x11, NameID: 4}) {
		t.Errorf("API calls %+v (overhead must be skipped)", apis)
	}
	if len(nvtx) != 2 {
		t.Fatalf("%d NVTX events", len(nvtx))
	}
	if r := nvtx[0]; r.EventType != NVTXPushPopRange || r.Start != -50 || !r.HasEnd || r.End != 300 || string(r.Text) != "range" || r.TextID != -1 {
		t.Errorf("range %+v", r)
	}
	if m := nvtx[1]; m.EventType != NVTXMark || m.HasEnd || m.TextID != 5 || m.Text != nil {
		t.Errorf("mark %+v", m)
	}
	if procs[0x10<<24] != "proc" || len(procs) != 1 {
		t.Errorf("processes %v", procs)
	}

	strs, err := f.Strings()
	if err != nil {
		t.Fatal(err)
	}
	if strs.Len() != 6 || strs.At(4) != "cudaLaunchKernel" || strs.At(0) != "" {
		t.Errorf("%d strings", strs.Len())
	}
	m, err := f.Meta()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Threads) != 1 || m.Threads[0] != (Thread{GlobalTid: 0x10<<24 | 0x11, NameID: 4}) {
		t.Errorf("threads %+v", m.Threads)
	}
	if len(m.GPUs) != 1 || m.GPUs[0] != (GPUInfo{ID: 1, Name: "Some GPU"}) {
		t.Errorf("GPUs %+v", m.GPUs)
	}
}

func TestVersions(t *testing.T) {
	if _, err := Parse(synthetic("2025@1@0@1"), Options{}); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unverified version: %v", err)
	}
	if _, err := Parse(synthetic("2025@1@0@1"), Options{AnyVersion: true}); err != nil {
		t.Errorf("unverified version, forced: %v", err)
	}
	if _, err := Parse([]byte("SQLite format 3\x00"), Options{}); !errors.Is(err, ErrNotReport) {
		t.Errorf("not a report: %v", err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.nsys-rep")
	if err := os.WriteFile(p, synthetic("2026@3@1@1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(p, Options{}); err != nil {
		t.Errorf("Check: %v", err)
	}
	if v, _ := ParseVersion([]byte(headerPrefix + "2026@3@1@117-1v0.\x0a")); v != "2026.3.1" {
		t.Errorf("ParseVersion %q", v)
	}
}

// Damaged reports fail with an error rather than a panic, so that callers
// can fall back to nsys.
func TestDamaged(t *testing.T) {
	good := synthetic("2026@3@1@1")
	for cut := 0; cut < len(good); cut += 7 {
		data := slices.Clone(good)
		data[cut] ^= 0x5a
		f, err := Parse(data, Options{})
		if err != nil {
			continue
		}
		var buf []byte
		for _, i := range f.EventFrames() {
			buf, _ = f.DecodeEvents(i, buf, &Visitor{NVTX: func(*NVTX) {}, Kernel: func(*Kernel) {}})
		}
		f.Strings()
		f.Meta()
	}
	if _, err := Parse(good[:len(good)-3], Options{}); err == nil {
		t.Error("truncated report parsed")
	}
}

func TestLZ4(t *testing.T) {
	// "abcabcabcabc!" as literals "abc", a match of 9 at offset 3
	// (overlapping), and the last literals "!".
	block := []byte{0x35, 'a', 'b', 'c', 3, 0, 0x10, '!'}
	out, err := lz4Block(nil, block, 13)
	if err != nil || string(out) != "abcabcabcabc!" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := lz4Block(nil, block, 12); err == nil {
		t.Error("wrong length accepted")
	}
	if _, err := lz4Block(nil, []byte{0x15, 'a', 9, 0, 0}, 10); err == nil {
		t.Error("offset before the start accepted")
	}
	long := bytes.Repeat([]byte("x"), 300)
	if out, err := lz4Block(nil, lz4Literal(long), 300); err != nil || !bytes.Equal(out, long) {
		t.Errorf("long literals: %v", err)
	}
}

// The session: from a loaded report, and read cheaply from the file.
func TestSession(t *testing.T) {
	want := Session{MonoStart: 1_515_000_123_456_789, WallStart: 1_792_411_200_987_654_321, Host: "node3"}
	data := Synthetic("2026@3@7@1-2", want)
	f, err := Parse(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Session(); got != want {
		t.Errorf("Session() = %+v, want %+v", got, want)
	}
	path := filepath.Join(t.TempDir(), "r.nsys-rep")
	os.WriteFile(path, data, 0o644)
	if got, err := ReadSession(path); err != nil || got != want {
		t.Errorf("ReadSession = %+v, %v; want %+v", got, err, want)
	}
	if got, _ := ReadSession(writeTemp(t, synthetic("2026@3@7@1-2"))); got != (Session{}) {
		t.Errorf("a report without a session: %+v", got)
	}
}

func writeTemp(t *testing.T, data []byte) string {
	p := filepath.Join(t.TempDir(), "x.nsys-rep")
	os.WriteFile(p, data, 0o644)
	return p
}
