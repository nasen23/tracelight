package server

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // writes the test traces

	"tracelight/internal/trace"
	"tracelight/internal/version"
)

// writeTrace writes a small nsys-like export: kernels on two streams and
// the API calls that launched them.
func writeTrace(t *testing.T, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE StringIds (id INTEGER PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO StringIds VALUES (1, 'gemm'), (2, 'relu'), (3, 'cudaLaunchKernel')`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_KERNEL (start INTEGER, end INTEGER, deviceId INTEGER, streamId INTEGER,
			correlationId INTEGER, shortName INTEGER, demangledName INTEGER)`,
		`CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (start INTEGER, end INTEGER, globalTid INTEGER, correlationId INTEGER, nameId INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := db.Begin()
	for i := range n {
		start := int64(i) * 10_000
		if _, err := tx.Exec(`INSERT INTO CUPTI_ACTIVITY_KIND_RUNTIME VALUES (?, ?, 72057595715649637, ?, 3)`, start, start+2000, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO CUPTI_ACTIVITY_KIND_KERNEL VALUES (?, ?, 0, ?, ?, ?, ?)`,
			start+3000, start+3000+int64(1+i%7)*900, 7+i%2*6, i+1, 1+i%2, 1+i%2); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// fixture makes a root with traces, a subdirectory, a file that is not a
// trace, links that stay inside and links that escape it.
func fixture(t *testing.T) (rootDir, outside string) {
	t.Helper()
	base := t.TempDir()
	rootDir = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{rootDir, filepath.Join(rootDir, "runs", "a"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTrace(t, filepath.Join(rootDir, "runs", "a", "train.sqlite"), 2000)
	writeTrace(t, filepath.Join(outside, "secret.sqlite"), 10)
	os.WriteFile(filepath.Join(rootDir, "notes.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(rootDir, ".hidden.sqlite"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(outside, "passwd"), []byte("root:x:0:0"), 0o644)
	links := map[string]string{
		filepath.Join(rootDir, "escape.sqlite"):      filepath.Join(outside, "secret.sqlite"),
		filepath.Join(rootDir, "escape-dir"):         outside,
		filepath.Join(rootDir, "inside-link.sqlite"): filepath.Join(rootDir, "runs", "a", "train.sqlite"),
		filepath.Join(rootDir, "runs", "up"):         filepath.Join(rootDir, ".."),
		filepath.Join(rootDir, "dangling.sqlite"):    filepath.Join(rootDir, "nope.sqlite"),
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks: %v", err)
		}
	}
	return rootDir, outside
}

func TestClean(t *testing.T) {
	ok := map[string]string{"": "", ".": "", "/": "", "a": "a", "a/b": "a/b", "a/./b": "a/b", "a//b/": "a/b"}
	for in, want := range ok {
		if got, err := clean(in); err != nil || got != want {
			t.Errorf("clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../x", "a/../../x", "a/..", "/etc/passwd", "//etc", `a\..\b`, "a\x00b", `C:\x`, "a/b/../../.."} {
		if got, err := clean(in); err == nil {
			t.Errorf("clean(%q) = %q, want an error", in, got)
		}
	}
}

func TestResolve(t *testing.T) {
	rootDir, _ := fixture(t)
	r, err := newRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"runs/a/train.sqlite", "inside-link.sqlite", "runs", ""} {
		if real, err := r.resolve(p); err != nil || !r.contains(real) {
			t.Errorf("resolve(%q) = %q, %v", p, real, err)
		}
	}
	cases := map[string]error{
		"escape.sqlite":          errOutside,
		"escape-dir/passwd":      errOutside,
		"escape-dir":             errOutside,
		"runs/up":                errOutside,
		"runs/up/outside/passwd": errOutside,
		"../outside/passwd":      errOutside,
		"dangling.sqlite":        errNotFound,
		"missing.sqlite":         errNotFound,
	}
	for p, want := range cases {
		if _, err := r.resolve(p); !errors.Is(err, want) {
			t.Errorf("resolve(%q): %v, want %v", p, err, want)
		}
	}
}

// A root that is itself reached through a symbolic link still works, and
// still confines.
func TestRootThroughSymlink(t *testing.T) {
	rootDir, _ := fixture(t)
	link := filepath.Join(t.TempDir(), "link-to-root")
	if err := os.Symlink(rootDir, link); err != nil {
		t.Skip(err)
	}
	r, err := newRoot(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.resolve("runs/a/train.sqlite"); err != nil {
		t.Error(err)
	}
	if _, err := r.resolve("escape.sqlite"); !errors.Is(err, errOutside) {
		t.Errorf("escape through a linked root: %v", err)
	}
}

func newTestServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	rootDir, _ := fixture(t)
	s, err := New(Config{Root: rootDir, Token: token, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, rootDir
}

func get(t *testing.T, ts *httptest.Server, path, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

func TestHTTPPathRestriction(t *testing.T) {
	ts, _ := newTestServer(t, "")
	q := url.QueryEscape
	cases := []struct {
		path string
		code int
	}{
		{"/api/files", 200},
		{"/api/files?dir=runs", 200},
		{"/api/files?dir=" + q(".."), 403},
		{"/api/files?dir=" + q("runs/../.."), 403},
		{"/api/files?dir=" + q("/etc"), 403},
		{"/api/files?dir=escape-dir", 403},
		{"/api/files?dir=runs/up", 403},
		{"/api/files?dir=%2e%2e", 403},
		{"/api/files?dir=" + q("runs/a/train.sqlite"), 400},
		{"/api/trace?path=" + q("runs/a/train.sqlite"), 200},
		{"/api/trace?path=inside-link.sqlite", 200},
		{"/api/trace?path=escape.sqlite", 403},
		{"/api/trace?path=" + q("escape-dir/secret.sqlite"), 403},
		{"/api/trace?path=" + q("../outside/secret.sqlite"), 403},
		{"/api/trace?path=" + q("/etc/passwd"), 403},
		{"/api/trace?path=" + q(`..\outside\secret.sqlite`), 400},
		{"/api/trace?path=" + q("runs/up/outside/secret.sqlite"), 403},
		{"/api/trace?path=notes.txt", 400},
		{"/api/trace?path=runs", 400},
		{"/api/trace?path=missing.sqlite", 404},
		{"/api/trace?path=dangling.sqlite", 404},
		{"/api/snapshot?path=escape.sqlite", 403},
		{"/api/tile?path=escape.sqlite&level=10&index=0", 403},
		{"/api/event?path=escape.sqlite&row=0&lane=0&index=0", 403},
		{"/api/summary?path=escape.sqlite", 403},
	}
	for _, c := range cases {
		if code, body := get(t, ts, c.path, ""); code != c.code {
			t.Errorf("GET %s: %d %s, want %d", c.path, code, body, c.code)
		}
	}
}

// Listings show traces and directories inside the root only.
func TestHTTPListing(t *testing.T) {
	ts, _ := newTestServer(t, "")
	_, body := get(t, ts, "/api/files", "")
	var res struct {
		Dir     string  `json:"dir"`
		Entries []entry `json:"entries"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Name+":"+e.Type)
	}
	got := strings.Join(names, " ")
	if want := "runs:dir inside-link.sqlite:sqlite"; got != want {
		t.Errorf("listing %q, want %q", got, want)
	}
	_, body = get(t, ts, "/api/files?dir=runs/a", "")
	if !strings.Contains(string(body), `"path":"runs/a/train.sqlite"`) {
		t.Errorf("listing of runs/a: %s", body)
	}
}

func TestHTTPToken(t *testing.T) {
	ts, _ := newTestServer(t, "s3cret")
	for _, tok := range []string{"", "wrong", "s3cret2", "S3CRET"} {
		if code, _ := get(t, ts, "/api/files", tok); code != 401 {
			t.Errorf("token %q: %d, want 401", tok, code)
		}
	}
	if code, _ := get(t, ts, "/api/snapshot?path=inside-link.sqlite", ""); code != 401 {
		t.Errorf("snapshot without a token: %d", code)
	}
	if code, _ := get(t, ts, "/api/files", "s3cret"); code != 200 {
		t.Errorf("right token: %d", code)
	}
	// The page itself holds no data, and asks for the token.
	if code, body := get(t, ts, "/", ""); code != 200 || !strings.Contains(string(body), "<") {
		t.Errorf("index without a token: %d", code)
	}
	// A token in the query string is not accepted (it would end up in logs).
	if code, _ := get(t, ts, "/api/files?token=s3cret", ""); code != 401 {
		t.Errorf("token in the query: %d", code)
	}
}

func TestHTTPReportWithoutNsys(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	os.WriteFile(filepath.Join(rootDir, "run.nsys-rep"), []byte("not really"), 0o644)
	code, body := get(t, ts, "/api/trace?path=run.nsys-rep", "")
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "nsys") {
		t.Errorf("report without nsys: %d %s", code, body)
	}
	_, list := get(t, ts, "/api/files", "")
	if !strings.Contains(string(list), `"type":"report"`) {
		t.Errorf("the report is not listed: %s", list)
	}
}

func decodeTile(t *testing.T, body []byte) Tile {
	t.Helper()
	var tile Tile
	if err := json.Unmarshal(body, &tile); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return tile
}

// Every tile holds exactly the events visible in its window, each lane's
// blocks summing to them, at every level from the whole trace down.
func TestTilesCoverEvents(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	tr, err := trace.LoadSQLite(filepath.Join(rootDir, "runs", "a", "train.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []int{3, 8, 12, 16, 20, 25} {
		_, t1 := TileWindow(level, 0)
		tiles := int(tr.Duration()/max(t1, 1)) + 1
		for index := 0; index < min(tiles, 6); index++ {
			code, body := get(t, ts, fmt.Sprintf("/api/tile?path=runs/a/train.sqlite&level=%d&index=%d", level, index), "")
			if code != 200 {
				t.Fatalf("tile %d/%d: %d %s", level, index, code, body)
			}
			tile := decodeTile(t, body)
			a, b := TileWindow(level, index)
			if tile.T0 != a || tile.T1 != b {
				t.Fatalf("tile window %d..%d, want %d..%d", tile.T0, tile.T1, a, b)
			}
			got := map[[2]int]int{}
			for _, row := range tile.Rows {
				for li, flat := range row.Lanes {
					if len(flat)%BlockFields != 0 {
						t.Fatal("a lane's values are not whole blocks")
					}
					for k := 0; k < len(flat); k += BlockFields {
						s, e, name, count := flat[k], flat[k+1], flat[k+2], flat[k+4]
						if count > 1 && (s < a || e > b) {
							t.Fatalf("merged block %d..%d outside the tile %d..%d", s, e, a, b)
						}
						if _, ok := tile.Names[int32(name)]; !ok {
							t.Fatalf("name %d of a block is not in the tile's names", name)
						}
						got[[2]int{row.Row, li}] += int(count)
					}
				}
			}
			for ri, tk := range tr.Tracks {
				for li, l := range tk.Lanes {
					i, j := l.Visible(tr.Start+a, tr.Start+b)
					if got[[2]int{ri, li}] != j-i {
						t.Fatalf("level %d tile %d row %d lane %d: %d events in blocks, %d visible", level, index, ri, li, got[[2]int{ri, li}], j-i)
					}
				}
			}
		}
	}
}

func TestTileParams(t *testing.T) {
	ts, _ := newTestServer(t, "")
	for _, q := range []string{"level=x&index=0", "level=1&index=-1", "level=99&index=0", "level=-20&index=0", "index=0"} {
		if code, _ := get(t, ts, "/api/tile?path=inside-link.sqlite&"+q, ""); code != 400 {
			t.Errorf("tile ?%s: %d, want 400", q, code)
		}
	}
	// Past the end: an empty tile.
	code, body := get(t, ts, "/api/tile?path=inside-link.sqlite&level=20&index=99999", "")
	if code != 200 || len(decodeTile(t, body).Rows) != 0 {
		t.Errorf("tile past the end: %d %s", code, body)
	}
}

func TestEventSummarySnapshot(t *testing.T) {
	ts, _ := newTestServer(t, "")
	code, body := get(t, ts, "/api/event?path=inside-link.sqlite&row=0&lane=0&index=5", "")
	if code != 200 {
		t.Fatalf("event: %d %s", code, body)
	}
	var ev eventJSON
	json.Unmarshal(body, &ev)
	if ev.Kind != "Kernel" || ev.Linked == nil || ev.Linked.Name != "cudaLaunchKernel" {
		t.Errorf("event %+v", ev)
	}
	if code, _ := get(t, ts, "/api/event?path=inside-link.sqlite&row=0&lane=0&index=999999", ""); code != 404 {
		t.Errorf("missing event: %d", code)
	}
	code, body = get(t, ts, "/api/summary?path=inside-link.sqlite&kind=kernel&n=5", "")
	var stats []statJSON
	if json.Unmarshal(body, &stats); code != 200 || len(stats) != 2 || stats[0].Count+stats[1].Count != 2000 {
		t.Errorf("summary: %d %s", code, body)
	}
	if code, _ := get(t, ts, "/api/summary?path=inside-link.sqlite&kind=bogus", ""); code != 400 {
		t.Errorf("bad kind: %d", code)
	}
	for _, q := range []string{"&from=5", "&from=a&to=9", "&from=9&to=5"} {
		if code, _ := get(t, ts, "/api/summary?path=inside-link.sqlite&kind=kernel"+q, ""); code != 400 {
			t.Errorf("bad range %q: %d", q, code)
		}
	}
	// The snapshot comes gzipped when asked, and reads back.
	req, _ := http.NewRequest("GET", ts.URL+"/api/snapshot?path=inside-link.sqlite", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := ts.Client().Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("snapshot not gzipped")
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := trace.ReadSnapshot(zr)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Counts[trace.KindKernel] != 2000 || tr.Counts[trace.KindRuntime] != 2000 {
		t.Errorf("snapshot counts %v", tr.Counts)
	}
}

// The cache loads a trace again when its file changes.
func TestCacheReloads(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	_, body := get(t, ts, "/api/trace?path=runs/a/train.sqlite", "")
	if !strings.Contains(string(body), `"events":4000`) {
		t.Fatalf("first load: %s", body)
	}
	p := filepath.Join(rootDir, "runs", "a", "train.sqlite")
	os.Remove(p)
	writeTrace(t, p, 100)
	_, body = get(t, ts, "/api/trace?path=runs/a/train.sqlite", "")
	if !strings.Contains(string(body), `"events":200`) {
		t.Errorf("after the file changed: %s", body)
	}
}

// A report converts with the nsys the server is given (here a stand-in that
// writes a known export), once: the export is reused while it is newer.
func TestHTTPReportConversion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in nsys is a shell script")
	}
	rootDir, _ := fixture(t)
	bin := t.TempDir()
	fakeExport := filepath.Join(bin, "export.sqlite")
	writeTrace(t, fakeExport, 50)
	calls := filepath.Join(bin, "calls")
	nsys := filepath.Join(bin, "nsys")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + calls + "\n" +
		"while [ $# -gt 0 ]; do [ \"$1\" = --output ] && out=$2; shift; done\n" +
		"cp " + fakeExport + " \"$out\"\n"
	if err := os.WriteFile(nsys, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(rootDir, "runs", "profile.nsys-rep")
	os.WriteFile(report, []byte("report"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(report, old, old)
	s, err := New(Config{Root: rootDir, Open: trace.OpenOptions{Nsys: nsys, CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	_, list := get(t, ts, "/api/files?dir=runs", "")
	if !strings.Contains(string(list), `"type":"report"`) || !strings.Contains(string(list), `"ready":true`) {
		t.Errorf("the report is not listed as ready to open: %s", list)
	}
	code, body := get(t, ts, "/api/trace?path=runs/profile.nsys-rep", "")
	if code != 200 || !strings.Contains(string(body), `"events":100`) {
		t.Fatalf("converted report: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "runs", "profile.sqlite")); err != nil {
		t.Errorf("the export is not next to the report: %v", err)
	}
	args, _ := os.ReadFile(calls)
	if !strings.Contains(string(args), "export --type sqlite") {
		t.Errorf("nsys called with %q", args)
	}
	// A new server (an empty memory cache) reuses the export.
	s2, _ := New(Config{Root: rootDir, Open: trace.OpenOptions{Nsys: nsys, CacheDir: t.TempDir()}})
	ts2 := httptest.NewServer(s2)
	defer ts2.Close()
	if code, _ := get(t, ts2, "/api/trace?path=runs/profile.nsys-rep", ""); code != 200 {
		t.Fatalf("second open: %d", code)
	}
	if args, _ := os.ReadFile(calls); strings.Count(string(args), "\n") != 1 {
		t.Errorf("nsys ran %d times, want once", strings.Count(string(args), "\n"))
	}
}

// The server reads files only through trace.OpenWith, and knows no formats
// of its own: what trace reads, it lists and opens, whatever its name.
func TestServerDefersToTrace(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	data, err := os.ReadFile(filepath.Join(rootDir, "runs", "a", "train.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rootDir, "runs", "export.db"), data, 0o644)
	if _, list := get(t, ts, "/api/files?dir=runs", ""); !strings.Contains(string(list), `"name":"export.db"`) {
		t.Errorf("a SQLite export named .db is not listed: %s", list)
	}
	if code, body := get(t, ts, "/api/trace?path=runs/export.db", ""); code != 200 || !strings.Contains(string(body), `"events":4000`) {
		t.Errorf("a SQLite export named .db: %d %s", code, body)
	}
}

// Keep trace.OpenWith the single place that turns files into traces: the
// server's code calls no reader or converter of a format itself.
func TestServerHasNoFormatPaths(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	files = append(files, filepath.Join("..", "..", "serve.go"))
	forbidden := []string{"LoadSQLite", "ExportReport", "ReadSnapshot", "IsReport", "FindNsys", "Converted(", `".sqlite"`, `".nsys-rep"`, "exec.Command"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s uses %s: open traces with trace.OpenWith, ask trace.Detect/Probe about files", f, bad)
			}
		}
	}
}

// Absolute paths are resolved on the server and accepted only inside the
// root, once symbolic links are followed; a root of "/" accepts any.
func TestHTTPAbsolutePaths(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	realRoot, _ := filepath.EvalSymlinks(rootDir)
	base := filepath.Dir(realRoot)
	q := url.QueryEscape
	inside := filepath.Join(realRoot, "runs", "a", "train.sqlite")
	cases := []struct {
		path string
		code int
		msg  string
	}{
		{inside, 200, ""},
		{filepath.Join(realRoot, "inside-link.sqlite"), 200, ""},
		{realRoot + "/./runs//a/train.sqlite", 200, ""},
		{filepath.Join(base, "outside", "secret.sqlite"), 403, "outside"},
		{realRoot + "/../outside/secret.sqlite", 403, "outside"},
		{filepath.Join(realRoot, "escape.sqlite"), 403, "outside"},               // a link out of the root
		{filepath.Join(realRoot, "escape-dir", "secret.sqlite"), 403, "outside"}, // through a linked directory
		{filepath.Join(realRoot, "runs", "up", "outside", "secret.sqlite"), 403, "outside"},
		{"/etc/passwd", 403, "outside"},
		{filepath.Join(realRoot, "missing.sqlite"), 404, "no such file"},
		{filepath.Join(realRoot, "dangling.sqlite"), 404, "no such file"},
		{filepath.Join(realRoot, "runs"), 400, "directory"},
		{filepath.Join(realRoot, "notes.txt"), 400, "not a trace"},
		{realRoot + "/runs/a/train.sqlite\x00x", 400, ""},
	}
	for _, c := range cases {
		code, body := get(t, ts, "/api/trace?path="+q(c.path), "")
		if code != c.code || !strings.Contains(string(body), c.msg) {
			t.Errorf("trace %s: %d %s, want %d %q", strings.TrimPrefix(c.path, base), code, body, c.code, c.msg)
		}
	}
	// The response gives the trace's absolute path, for the address bar.
	_, body := get(t, ts, "/api/trace?path="+q(inside), "")
	var meta struct{ Abs, Path string }
	json.Unmarshal(body, &meta)
	if meta.Abs != inside || meta.Path != "runs/a/train.sqlite" {
		t.Errorf("abs %q path %q", meta.Abs, meta.Path)
	}
	// Tiles take the absolute path as well; listings an absolute directory.
	if code, _ := get(t, ts, "/api/tile?level=20&index=0&path="+q(inside), ""); code != 200 {
		t.Errorf("tile by absolute path: %d", code)
	}
	if code, body := get(t, ts, "/api/files?dir="+q(filepath.Join(realRoot, "runs")), ""); code != 200 || !strings.Contains(string(body), `"dir":"runs"`) {
		t.Errorf("listing by absolute path: %d %s", code, body)
	}
	if code, _ := get(t, ts, "/api/files?dir="+q(base), ""); code != 403 {
		t.Errorf("listing above the root: %d", code)
	}
}

// --root / serves the whole file system.
func TestHTTPRootSlash(t *testing.T) {
	rootDir, outside := fixture(t)
	s, err := New(Config{Root: "/", Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()
	for _, p := range []string{filepath.Join(outside, "secret.sqlite"), filepath.Join(rootDir, "escape.sqlite")} {
		if code, body := get(t, ts, "/api/trace?path="+url.QueryEscape(p), ""); code != 200 {
			t.Errorf("with --root /, %s: %d %s", filepath.Base(p), code, body)
		}
	}
}

// A root reached through a symbolic link still confines absolute paths, in
// either spelling of the root.
func TestAbsoluteThroughLinkedRoot(t *testing.T) {
	rootDir, outside := fixture(t)
	link := filepath.Join(t.TempDir(), "link-to-root")
	if err := os.Symlink(rootDir, link); err != nil {
		t.Skip(err)
	}
	r, err := newRoot(link)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(link, "runs", "a", "train.sqlite"), filepath.Join(rootDir, "runs", "a", "train.sqlite")} {
		if _, err := r.resolve(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := r.resolve(filepath.Join(outside, "secret.sqlite")); !errors.Is(err, errOutside) {
		t.Errorf("outside through a linked root: %v", err)
	}
}

// The page's icons are served, with image types, without a token.
func TestServesIcons(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	for path, ctype := range map[string]string{
		"/favicon.svg": "image/svg+xml", "/favicon-32.png": "image/png", "/apple-touch-icon.png": "image/png",
	} {
		res, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
}

// writeRank writes a small trace captured at start on host.
func writeRank(t *testing.T, path string, start int64, host string) {
	t.Helper()
	writeTrace(t, path, 20)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE TARGET_INFO_SESSION_START_TIME (utcEpochNs INTEGER)`,
		fmt.Sprintf(`INSERT INTO TARGET_INFO_SESSION_START_TIME VALUES (%d)`, start),
		`CREATE TABLE TARGET_INFO_SYSTEM_ENV (name TEXT, value TEXT)`,
		fmt.Sprintf(`INSERT INTO TARGET_INFO_SYSTEM_ENV VALUES ('Hostname', '%s')`, host),
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
}

type sessionResp struct {
	Traces   []sessionTrace `json:"traces"`
	Warnings []string       `json:"warnings"`
}

func getSession(t *testing.T, ts *httptest.Server, paths ...string) (int, sessionResp, string) {
	t.Helper()
	q := url.Values{"path": paths}
	code, body := get(t, ts, "/api/session?"+q.Encode(), "")
	var r sessionResp
	json.Unmarshal(body, &r)
	return code, r, string(body)
}

func names(r sessionResp) string {
	var n []string
	for _, tr := range r.Traces {
		n = append(n, tr.Name)
	}
	return strings.Join(n, " ")
}

// A directory opens its traces, naturally sorted, one of a report and its
// export, not its subdirectories; globs and lists of paths work too.
func TestSession(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	job := filepath.Join(rootDir, "job")
	os.MkdirAll(filepath.Join(job, "sub"), 0o755)
	base := int64(1792411200000000000)
	for _, r := range []int{0, 1, 2, 10} {
		writeRank(t, filepath.Join(job, fmt.Sprintf("rank%d.sqlite", r)), base+int64(r)*1e6, fmt.Sprintf("node%d", r/2))
	}
	// rank3 as a link inside the root, a report beside its export, a link out of the root.
	os.Symlink(filepath.Join(job, "rank2.sqlite"), filepath.Join(job, "rank3.sqlite"))
	os.WriteFile(filepath.Join(job, "rank1.nsys-rep"), []byte("not a decodable report"), 0o644)
	os.Symlink(filepath.Join(filepath.Dir(rootDir), "outside", "secret.sqlite"), filepath.Join(job, "rank9.sqlite"))
	writeRank(t, filepath.Join(job, "sub", "rank4.sqlite"), base, "node9") // not opened: not recursive
	os.WriteFile(filepath.Join(job, "notes.txt"), []byte("x"), 0o644)

	code, r, body := getSession(t, ts, "job")
	if code != 200 {
		t.Fatalf("session of a directory: %d %s", code, body)
	}
	if got := names(r); got != "rank0.sqlite rank1.sqlite rank2.sqlite rank3.sqlite rank10.sqlite" {
		t.Errorf("traces %q", got)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "outside") {
		t.Errorf("warnings %q", r.Warnings)
	}
	if tr := r.Traces[4]; tr.Rank != 10 || tr.Host != "node5" || tr.SessionStart != base+10e6 || !tr.Ready || !filepath.IsAbs(tr.Path) {
		t.Errorf("rank10: %+v", tr)
	}
	// Globs, relative and absolute; several paths; duplicates once.
	realJob, _ := filepath.EvalSymlinks(job)
	for _, c := range []struct {
		paths []string
		want  string
	}{
		{[]string{"job/rank1*.sqlite"}, "rank1.sqlite rank10.sqlite"},
		{[]string{realJob + "/rank?.sqlite"}, "rank0.sqlite rank1.sqlite rank2.sqlite rank3.sqlite"},
		{[]string{"job/rank10.sqlite", "job/rank0.sqlite", realJob + "/rank0.sqlite"}, "rank0.sqlite rank10.sqlite"},
		{[]string{"job/sub", "job/rank2.sqlite"}, "rank2.sqlite rank4.sqlite"},
	} {
		code, r, body := getSession(t, ts, c.paths...)
		if code != 200 || names(r) != c.want {
			t.Errorf("session %q: %d %q, want %q (%s)", c.paths, code, names(r), c.want, body)
		}
	}
	// Refusals.
	os.MkdirAll(filepath.Join(rootDir, "empty"), 0o755)
	for _, c := range []struct {
		paths []string
		code  int
	}{
		{[]string{"empty"}, 404},
		{[]string{"job/nothing*.sqlite"}, 404},
		{[]string{"job/notes.txt"}, 400},
		{[]string{"../outside/*.sqlite"}, 403},
		{[]string{"/etc"}, 403},
		{[]string{"job/missing.sqlite"}, 404},
		{nil, 400},
	} {
		if code, _, body := getSession(t, ts, c.paths...); code != c.code {
			t.Errorf("session %q: %d, want %d (%s)", c.paths, code, c.code, body)
		}
	}
}

// The traces of a session load, and report their capture's clock, which the
// client aligns them on.
func TestSessionTraceMeta(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	p := filepath.Join(rootDir, "rank5.sqlite")
	writeRank(t, p, 1792411200123000000, "nodeX")
	code, body := get(t, ts, "/api/trace?path="+url.QueryEscape(p), "")
	var meta struct {
		SessionStart, Start int64
		Host                string
	}
	json.Unmarshal(body, &meta)
	if code != 200 || meta.SessionStart != 1792411200123000000 || meta.Host != "nodeX" || meta.Start != 0 {
		t.Errorf("meta: %d %+v", code, meta)
	}
}

func TestNatCompare(t *testing.T) {
	in := []string{"rank10", "rank2", "rank1", "rank02", "rank", "rank1b", "node1-rank3", "node1-rank12", "a"}
	slices.SortFunc(in, natCompare)
	if got := strings.Join(in, " "); got != "a node1-rank3 node1-rank12 rank rank1 rank1b rank2 rank02 rank10" {
		t.Errorf("natural order: %s", got)
	}
	for name, want := range map[string]int{"rank3.nsys-rep": 3, "job_rank_12.sqlite": 12, "RANK-7.sqlite": 7, "worker.sqlite": -1} {
		if got := rankOf(name); got != want {
			t.Errorf("rankOf(%s) = %d, want %d", name, got, want)
		}
	}
}

// A range sums the events overlapping it, clipped to it, in the trace's
// times from its start; asked again, it comes from the cache.
func TestSummaryInRange(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	tr, err := trace.Open(filepath.Join(rootDir, "inside-link.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	d := tr.Duration()
	from, to := d/4+123, d/2
	path := fmt.Sprintf("/api/summary?path=inside-link.sqlite&kind=kernel&n=5&from=%d&to=%d", from, to)
	for range 2 {
		code, body := get(t, ts, path, "")
		var got []statJSON
		if json.Unmarshal(body, &got); code != 200 {
			t.Fatalf("summary in range: %d %s", code, body)
		}
		want := tr.TopInRange(5, tr.Start+from, tr.Start+to, trace.KindKernel)
		if len(got) != len(want) || len(got) == 0 {
			t.Fatalf("got %d names, want %d", len(got), len(want))
		}
		var count int
		for i, w := range want {
			g := got[i]
			if g.Name != w.Name || g.Count != w.Count || g.Total != w.Total || g.Min != w.Min || g.Max != w.Max {
				t.Errorf("%d: got %+v, want %+v", i, g, w)
			}
			count += g.Count
		}
		if count == 0 || count >= 2000 {
			t.Errorf("%d kernels in a quarter of the trace", count)
		}
	}
	code, body := get(t, ts, "/api/summary?path=inside-link.sqlite&kind=kernel&from=7&to=7", "")
	if code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("an empty range: %d %s", code, body)
	}
}

// /api/info says the app's version, for the web UI's header.
func TestInfoVersion(t *testing.T) {
	ts, _ := newTestServer(t, "")
	code, body := get(t, ts, "/api/info", "")
	var info struct{ App string }
	if json.Unmarshal(body, &info); code != 200 || info.App != version.Version {
		t.Errorf("info: %d %s", code, body)
	}
}

// Every request under /api that no endpoint takes answers in JSON, with
// the right status, never with the web UI's files.
func TestUnknownAPIIsJSON(t *testing.T) {
	ts, _ := newTestServer(t, "")
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/foo", 404}, {"GET", "/api/info/", 404}, {"GET", "/api", 404}, {"GET", "/api/", 404},
		{"POST", "/api/info", 405}, {"DELETE", "/api/trace", 405}, {"PUT", "/api/nope", 404}, {"GET", "/api/foo/bar?x=1", 404},
	} {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Error string }
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if res.StatusCode != c.code || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") || err != nil || body.Error == "" {
			t.Errorf("%s %s: %d %q %v %+v", c.method, c.path, res.StatusCode, res.Header.Get("Content-Type"), err, body)
		}
	}
	// The web UI still serves its pages.
	if code, body := get(t, ts, "/?path=/x", ""); code != 200 || !strings.Contains(string(body), "<!doctype html>") {
		t.Errorf("the web UI: %d", code)
	}
}

// Under a path prefix (a gateway mounting the server at /some/prefix/),
// the web UI's page, every file it refers to and its API calls resolve
// under the prefix: they are all relative.
func TestWebUnderPrefix(t *testing.T) {
	rootDir, _ := fixture(t)
	s, err := New(Config{Root: rootDir, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(http.StripPrefix("/some/prefix", s))
	defer gw.Close()
	page := gw.URL + "/some/prefix/?path=/x#token=t"
	res, err := http.Get(page)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("page: %v %v", err, res)
	}
	html, _ := io.ReadAll(res.Body)
	res.Body.Close()
	refs := regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(string(html), -1)
	if len(refs) < 6 {
		t.Fatalf("only %d references in the page", len(refs))
	}
	base, _ := url.Parse(page)
	for _, m := range refs {
		if strings.HasPrefix(m[1], "/") || strings.Contains(m[1], "://") {
			t.Errorf("absolute reference %q", m[1])
			continue
		}
		u := base.ResolveReference(&url.URL{Path: m[1]})
		r, err := http.Get(u.String())
		if err != nil || r.StatusCode != 200 {
			t.Errorf("%s: %v %v", u, err, r.Status)
		}
		r.Body.Close()
	}
	// The client's calls: relative "api/…", and the address it keeps
	// ("?path=…#…") relative to the page.
	js, _ := os.ReadFile("web/app.js")
	for _, bad := range []string{`fetch("/`, `fetch('/`, "\"/api/", "location.pathname =", "location.href ="} {
		if strings.Contains(string(js), bad) {
			t.Errorf("app.js has %q: an absolute URL", bad)
		}
	}
	if !strings.Contains(string(js), `fetch("api/"`) || !strings.Contains(string(js), `history.replaceState(null, "", "?" +`) {
		t.Error("app.js no longer fetches api/ relatively, or keeps the address relatively")
	}
	r, err := http.Get(base.ResolveReference(&url.URL{Path: "api/info"}).String())
	if err != nil || r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("api/info under the prefix: %v %v", err, r)
	}
}

// Tiles in batches: the same as one at a time, of several traces at once;
// with lanes, only the rows and lanes a rank overview draws; computed once
// and kept; bad requests say why.
func TestTileBatch(t *testing.T) {
	ts, rootDir := newTestServer(t, "")
	srv := ts.Config.Handler.(*Server)
	a, b := "inside-link.sqlite", "runs/a/train.sqlite"
	q := "/api/tiles?path=" + url.QueryEscape(a) + "&path=" + url.QueryEscape(b) + "&t=0:10:0&t=1:10:1&t=0:12:0"
	code, body := get(t, ts, q, "")
	var res struct {
		Tiles []struct {
			P    int  `json:"p"`
			Tile Tile `json:"tile"`
		}
	}
	if err := json.Unmarshal(body, &res); err != nil || code != 200 || len(res.Tiles) != 3 {
		t.Fatalf("batch: %d %v %.200s", code, err, body)
	}
	for i, want := range []struct{ p, level, index int }{{0, 10, 0}, {1, 10, 1}, {0, 12, 0}} {
		got := res.Tiles[i]
		_, one := get(t, ts, fmt.Sprintf("/api/tile?path=%s&level=%d&index=%d", url.QueryEscape([]string{a, b}[want.p]), want.level, want.index), "")
		var single Tile
		json.Unmarshal(one, &single)
		if got.P != want.p || got.Tile.Level != want.level || got.Tile.Index != want.index || len(got.Tile.Rows) == 0 || fmt.Sprint(got.Tile.Rows) != fmt.Sprint(single.Rows) {
			t.Errorf("tile %d: %+v, alone %d rows", i, got.P, len(single.Rows))
		}
	}
	computed := srv.tiles.computed
	get(t, ts, q, "")
	if srv.tiles.computed != computed {
		t.Errorf("asked again, %d tiles computed again", srv.tiles.computed-computed)
	}
	// Lanes: the GPU rows (all lanes), no others unless asked for.
	tr, err := trace.Open(filepath.Join(rootDir, a))
	if err != nil {
		t.Fatal(err)
	}
	code, body = get(t, ts, "/api/tiles?path="+url.QueryEscape(a)+"&t=0:10:0&lanes=nvtx,gpu", "")
	json.Unmarshal(body, &res)
	if code != 200 || len(res.Tiles) != 1 {
		t.Fatalf("lanes: %d %.200s", code, body)
	}
	for _, r := range res.Tiles[0].Tile.Rows {
		if k := tr.Tracks[r.Row].Kind; k != trace.TrackStream || len(r.Lanes) != len(tr.Tracks[r.Row].Lanes) {
			t.Errorf("lanes gpu,nvtx: row %d (%v) with %d lanes", r.Row, k, len(r.Lanes))
		}
	}
	// The CUDA API rows' outermost few (trace.OverviewLanes); the meta says
	// which sub-lane of the overview each draws into.
	code, body = get(t, ts, "/api/tiles?path="+url.QueryEscape(a)+"&t=0:10:0&lanes=api", "")
	json.Unmarshal(body, &res)
	if code != 200 || len(res.Tiles) != 1 || len(res.Tiles[0].Tile.Rows) == 0 {
		t.Fatalf("lanes api: %d %.200s", code, body)
	}
	for _, r := range res.Tiles[0].Tile.Rows {
		tk := tr.Tracks[r.Row]
		if tk.Kind != trace.TrackRuntime || len(r.Lanes) != trace.OverviewLanes(tk.Kind, len(tk.Lanes)) {
			t.Errorf("lanes api: row %d (%v) with %d of %d lanes", r.Row, tk.Kind, len(r.Lanes), len(tk.Lanes))
		}
	}
	var meta struct {
		Rows []struct {
			Kind     string `json:"kind"`
			Lanes    int    `json:"lanes"`
			Overview []int  `json:"overview"`
		} `json:"rows"`
	}
	_, body = get(t, ts, "/api/trace?path="+url.QueryEscape(a), "")
	json.Unmarshal(body, &meta)
	for i, r := range meta.Rows {
		if len(r.Overview) != trace.OverviewLanes(tr.Tracks[i].Kind, r.Lanes) {
			t.Errorf("row %d (%s, %d lanes): overview %v", i, r.Kind, r.Lanes, r.Overview)
		}
	}
	for _, bad := range []string{"/api/tiles?path=a", "/api/tiles?t=0:1:0", "/api/tiles?path=" + a + "&t=1:10:0",
		"/api/tiles?path=" + a + "&t=0:x:0", "/api/tiles?path=" + a + "&t=0:10:0&lanes=disk", "/api/tiles?path=escape.sqlite&t=0:10:0"} {
		if code, body := get(t, ts, bad, ""); code < 400 || !strings.Contains(string(body), "error") {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
}
