package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tracelight/internal/torchgen"
	"tracelight/internal/trace"
)

// PyTorch profiles on the server: listed (a JSON config beside them is
// not), opened as a session of ranks (their rank from the file), with
// their CPU ops and Python rows; an event's details carry its Python
// stack; summaries of CPU ops; the overview's tiles hold the Python depths
// it draws.
func TestTorchAPI(t *testing.T) {
	root := t.TempDir()
	job := filepath.Join(root, "job")
	if _, err := torchgen.WriteDir(job, torchgen.Options{Ranks: 2, Steps: 2, Layers: 2}, true); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(job, "config.json"), []byte(`{"lr": 3e-4}`), 0o644)
	srv, err := New(Config{Root: root, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	decode := func(path string, v any) {
		t.Helper()
		code, body := get(t, ts, path, "")
		if code != 200 {
			t.Fatalf("%s: %d %.300s", path, code, body)
		}
		if err := json.Unmarshal(body, v); err != nil {
			t.Fatal(err)
		}
	}
	var files struct {
		Entries []struct{ Name, Type, Note string }
	}
	decode("/api/files?dir=job", &files)
	var listed []string
	for _, e := range files.Entries {
		listed = append(listed, e.Name+":"+e.Note)
	}
	if strings.Join(listed, ",") != "rank0.pt.trace.json.gz:PyTorch profiler,rank1.pt.trace.json.gz:PyTorch profiler" {
		t.Errorf("listed %q", listed)
	}
	var sess struct {
		Traces []struct {
			Path, Format string
			Rank         int
			SessionStart int64
		}
	}
	decode("/api/session?path=job", &sess)
	if len(sess.Traces) != 2 || sess.Traces[1].Format != "torch" || sess.Traces[1].Rank != 1 || sess.Traces[1].SessionStart == 0 {
		t.Fatalf("session %+v", sess.Traces)
	}
	p := url.QueryEscape(sess.Traces[1].Path)
	var meta struct {
		Rows []struct {
			Name, Kind string
			Lanes      int
			Overview   []int
		}
		Counts map[string]int
	}
	decode("/api/trace?path="+p, &meta)
	py, kernels := -1, map[string]int{}
	for i, r := range meta.Rows {
		kernels[r.Kind]++
		if r.Kind == "python" {
			py = i
		}
	}
	if kernels["cpu"] != 2 || kernels["python"] != 1 || meta.Counts["Python"] == 0 || meta.Counts["CPU op"] == 0 {
		t.Fatalf("rows %+v counts %v", meta.Rows, meta.Counts)
	}
	// The script's frames are left out of the overview.
	ov := meta.Rows[py].Overview
	if len(ov) < 6 || ov[0] != -1 || ov[4] != -1 || ov[5] != 0 {
		t.Errorf("Python row's overview %v", ov)
	}
	// An event's Python stack: the first Python call of the deepest drawn
	// depth there is, its own last.
	var ev struct {
		Name   string
		Python bool
		Stack  []struct {
			Name             string
			Row, Lane, Index int
		}
	}
	decode("/api/event?path="+p+"&row="+itoa(py)+"&lane=8&index=0", &ev)
	if !ev.Python || len(ev.Stack) != 9 || ev.Stack[0].Name != "runpy.py(198): _run_module_as_main" || ev.Stack[8].Name != ev.Name || ev.Stack[8].Row != py || ev.Stack[8].Lane != 8 {
		t.Errorf("event %s, stack %+v", ev.Name, ev.Stack)
	}
	var top []struct{ Name string }
	decode("/api/summary?path="+p+"&kind=cpu&n=5", &top)
	if len(top) == 0 || !strings.Contains(top[0].Name, "::") {
		t.Errorf("top CPU ops %+v", top)
	}
	// Overview tiles of the Python lane: its rows, up to the depths drawn.
	var tiles struct {
		Tiles []struct {
			Tile struct {
				Rows []struct {
					Row   int       `json:"r"`
					Lanes [][]int64 `json:"l"`
				} `json:"rows"`
			} `json:"tile"`
		} `json:"tiles"`
	}
	decode("/api/tiles?path="+p+"&t=0:20:0&lanes=python", &tiles)
	rows := tiles.Tiles[0].Tile.Rows
	if len(rows) != 1 || rows[0].Row != py || len(rows[0].Lanes) != len(ov) {
		t.Errorf("python tiles: %d rows (%+v), want row %d of %d lanes", len(rows), rows, py, len(ov))
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// A file of several ranks merged opens as a session of its ranks: each a
// path of its own (file#rank<N>), named after its rank, on the file's
// clock; an event's details carry its operator stack.
func TestTorchMergedAPI(t *testing.T) {
	root := t.TempDir()
	if err := torchgen.WriteMerged(filepath.Join(root, "job.json.gz"), torchgen.Options{Ranks: 3, Steps: 2, Layers: 1, NoStack: true}, true); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Root: root, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	decode := func(path string, v any) {
		t.Helper()
		code, body := get(t, ts, path, "")
		if code != 200 {
			t.Fatalf("%s: %d %.300s", path, code, body)
		}
		json.Unmarshal(body, v)
	}
	var sess struct {
		Traces []struct {
			Path, Name, Clock string
			Rank              int
			SessionMono       int64
		}
	}
	decode("/api/session?path=job.json.gz", &sess)
	if len(sess.Traces) != 3 {
		t.Fatalf("session %+v", sess.Traces)
	}
	for r, tr := range sess.Traces {
		if tr.Name != "rank"+itoa(r) || tr.Rank != r || !strings.HasSuffix(tr.Path, "job.json.gz#rank"+itoa(r)) || tr.Clock == "" || tr.SessionMono == 0 {
			t.Errorf("trace %d: %+v", r, tr)
		}
	}
	p := url.QueryEscape(sess.Traces[2].Path)
	var meta struct {
		Path, Clock string
		Rows        []struct{ Kind string }
	}
	decode("/api/trace?path="+p, &meta)
	if !strings.HasSuffix(meta.Path, "job.json.gz#rank2") || meta.Clock == "" || len(meta.Rows) == 0 {
		t.Errorf("meta %+v", meta)
	}
	var ev struct {
		Name string
		Ops  []struct{ Name string }
	}
	decode("/api/event?path="+p+"&row=0&lane=0&index=3", &ev)
	if len(ev.Ops) == 0 {
		t.Errorf("event %s without operators", ev.Name)
	}
	// Outside the root, a rank of it is refused as the file is.
	if code, _ := get(t, ts, "/api/trace?path="+url.QueryEscape("/etc/hosts#rank1"), ""); code < 400 {
		t.Errorf("a rank of a file outside the root: %d", code)
	}
}
