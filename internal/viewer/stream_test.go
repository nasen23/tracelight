package viewer

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	_ "modernc.org/sqlite" // writes the test traces

	"tracelight/internal/nsysrep"
	"tracelight/internal/server"
	"tracelight/internal/trace"
)

// writeSQLite writes a small nsys-like export: kernels on two streams, the
// API calls that launched them, and the capture's clock and host.
func writeSQLite(t *testing.T, path string, n int, clock int64, host string) {
	t.Helper()
	writeSQLiteClocks(t, path, n, clock, 0, host)
}

// writeSQLiteClocks is writeSQLite with the capture's start on the host's
// monotonic clock too (systemClockNs; 0 for none).
func writeSQLiteClocks(t *testing.T, path string, n int, clock, mono int64, host string) {
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
		`CREATE TABLE TARGET_INFO_SESSION_START_TIME (utcEpochNs INTEGER, systemClockNs INTEGER)`,
		fmt.Sprintf(`INSERT INTO TARGET_INFO_SESSION_START_TIME VALUES (%d, %s)`, clock, map[bool]string{true: "NULL", false: fmt.Sprint(mono)}[mono == 0]),
		`CREATE TABLE TARGET_INFO_SYSTEM_ENV (name TEXT, value TEXT)`,
		fmt.Sprintf(`INSERT INTO TARGET_INFO_SYSTEM_ENV VALUES ('Hostname', '%s')`, host),
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

// delayed delays every request by d, as a network would.
func delayed(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		h.ServeHTTP(w, r)
	})
}

// testServer serves root, each request delayed by delay.
func testServer(t *testing.T, root, token string, delay time.Duration) string {
	t.Helper()
	srv, err := server.New(server.Config{Root: root, Token: token, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(delayed(srv, delay))
	t.Cleanup(ts.Close)
	return ts.URL
}

// waitFor frames the window until cond holds.
func waitFor(t *testing.T, tt *ui.Tester, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; showing %q", what, tt.Texts())
		}
		time.Sleep(3 * time.Millisecond)
		tt.Frame()
	}
}

func jobDir(t *testing.T, ranks int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "job")
	os.MkdirAll(dir, 0o755)
	for r := range ranks {
		// rank r's capture starts r ms after rank 0's.
		writeSQLite(t, filepath.Join(dir, fmt.Sprintf("rank%d.sqlite", r)), 300, 1792411200_000_000_000+int64(r)*1_000_000, fmt.Sprintf("node%d", r/2))
	}
	return dir
}

// The connect dialog: a URL, Connect, the server's state; it is kept among
// the recent servers; a server that needs a token says so; Disconnect.
func TestConnectDialog(t *testing.T) {
	root := jobDir(t, 2)
	url := testServer(t, root, "", 0)
	s := New(nil)
	var kept Settings
	s.OnSettings(func(st Settings) { kept = st })
	tt := ui.NewTester(s.View, w, h)
	if err := tt.Click("Connect to server…"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("Connect to a server") || !tt.HasText("Local") {
		t.Fatalf("no dialog: %q", tt.Texts())
	}
	tt.Type(url)
	tt.Key(0, ui.KeyEnter)
	waitFor(t, tt, "the connection", func() bool { _, ok := s.Connected(); return ok })
	tt.Frame()
	host := strings.TrimPrefix(url, "http://")
	if !tt.HasText(host) || len(kept.Servers) != 1 || kept.Servers[0] != url+"/" {
		t.Errorf("connected: chip %v, kept %+v", tt.HasText(host), kept)
	}
	// Its traces are listed.
	waitFor(t, tt, "the server's listing", func() bool { return tt.HasText("rank1.sqlite") })

	// Disconnect.
	tt.Key(ui.Cmd, ui.KeyK)
	tt.Frame()
	if err := tt.Click("Disconnect"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if _, ok := s.Connected(); ok || !tt.HasText("Local") {
		t.Error("still connected after Disconnect")
	}

	// A server with a token, without it.
	secret := testServer(t, root, "s3cret", 0)
	s.Connect(secret, "", "")
	waitFor(t, tt, "the refusal", func() bool { return s.ctl.state == connFailed })
	if !strings.Contains(s.ctl.err.Error(), "needs a token") {
		t.Errorf("error %v", s.ctl.err)
	}
	s.Connect(secret, "s3cret", "")
	waitFor(t, tt, "the connection with the token", func() bool { _, ok := s.Connected(); return ok })
}

// The path box: Ctrl/⌘+L and / focus it; a folder opens its traces side by
// side, in natural order, aligned on their clocks; a file opens alone; a
// bad path says why; what opened is remembered.
func TestPathBoxLocal(t *testing.T) {
	dir := jobDir(t, 3)
	os.Rename(filepath.Join(dir, "rank2.sqlite"), filepath.Join(dir, "rank10.sqlite"))
	s := New(nil)
	var kept Settings
	s.OnSettings(func(st Settings) { kept = st })
	tt := ui.NewTester(s.View, w, h)
	tt.Key(ui.Cmd, ui.KeyL)
	tt.Frame()
	tt.Frame()
	if !tt.Focused("Path") {
		t.Fatal("Ctrl/⌘+L did not focus the path box")
	}
	tt.Type(dir + "/nope")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if !tt.HasText(dir + "/nope: no such file or folder") {
		t.Errorf("a bad path does not say why: %q", tt.Texts())
	}
	s.ctl.pathText = ""
	tt.Frame()
	if !tt.Focused("Path") {
		t.Error("the path box lost the focus after an error")
	}
	tt.Type(dir)
	if s.ctl.pathText != dir {
		t.Fatalf("typed %q", s.ctl.pathText)
	}
	tt.Key(0, ui.KeyEnter)
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	ss, ok := s.src.(*sessionSource)
	if !ok || len(ss.parts) != 3 {
		t.Fatalf("a folder of 3 traces opened as %T", s.src)
	}
	var labels []string
	for _, p := range ss.parts {
		labels = append(labels, p.label)
	}
	if want := []string{"rank0 (node0)", "rank1 (node0)", "rank10 (node1)"}; !slices.Equal(labels, want) {
		t.Errorf("parts %q, want %q", labels, want)
	}
	if ss.parts[1].offset != 1_000_000 || ss.parts[2].offset != 2_000_000 {
		t.Errorf("offsets %d, %d: not aligned on the capture clocks", ss.parts[1].offset, ss.parts[2].offset)
	}
	tt.Frame()
	if !tt.HasText("rank10 (node1) · GPU 0") || s.perf.blocks == 0 {
		t.Errorf("the session's rows do not show: %q", tt.Texts())
	}
	if len(kept.RecentPaths) == 0 || kept.RecentPaths[0] != dir {
		t.Errorf("recent paths %q", kept.RecentPaths)
	}
	// A rank's event: the details are its, shifted onto the session's axis.
	var ref trace.Ref
	for ti, tk := range s.tr.Tracks {
		if strings.HasPrefix(tk.Group, "rank1 ") && tk.Kind == trace.TrackStream {
			ref = trace.Ref{Track: int32(ti), Lane: 0, Index: 3}
			break
		}
	}
	in, ok := s.src.event(ref)
	local := ss.parts[1].tr
	e := local.Event(trace.Ref{Track: ref.Track - int32(ss.parts[1].first), Index: 3})
	if !ok || in.start != e.Start-local.Start+1_000_000 || in.linked == nil || in.linked.name != "cudaLaunchKernel" {
		t.Errorf("event %+v", in)
	}

	// "/" focuses the box again; a file opens alone.
	tt.Click("Timeline")
	tt.Key(0, ui.KeySlash)
	tt.Frame()
	tt.Frame()
	if !tt.Focused("Path") {
		t.Error("/ did not focus the path box")
	}
	s.OpenPath(filepath.Join(dir, "rank1.sqlite"))
	waitFor(t, tt, "the file", func() bool { _, ok := s.src.(*localSource); return ok && s.loading == "" })
	// The folder is suggested, with its traces.
	s.ctl.pathText = dir + "/"
	if sg := s.suggestions(); !slices.Contains(sg, dir+"/rank10.sqlite") || !slices.Contains(sg, dir) {
		t.Errorf("suggestions %q", sg)
	}
}

// A trace on a server streams: its rows at once, then tiles as they show,
// coarser ones standing in while finer ones come; what it draws hits the
// events the trace in memory has; details, summaries and summaries of a
// range come from the server; Download for offline keeps it.
func TestStreamFromServer(t *testing.T) {
	root := jobDir(t, 1)
	path := filepath.Join(root, "rank0.sqlite")
	url := testServer(t, root, "", 20*time.Millisecond)
	local, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(url, "", path)
	waitFor(t, tt, "the rows", func() bool { return s.tr != nil && s.loading == "" })
	rs, ok := s.src.(*remoteSource)
	if !ok {
		t.Fatalf("source %T", s.src)
	}
	if len(s.tr.Tracks) != len(local.Tracks) || s.tr.Duration() != local.Duration() || s.tr.EventCount() != local.EventCount() {
		t.Fatalf("skeleton: %d rows, %d ns, %d events", len(s.tr.Tracks), s.tr.Duration(), s.tr.EventCount())
	}
	waitFor(t, tt, "the first tiles", func() bool { return s.perf.blocks > 0 })
	waitFor(t, tt, "every tile", func() bool { _, done, _ := rs.status(); return done && rs.loading() == 0 })

	// Zoomed in, the coarse tiles stand in at once.
	mid := float64(local.Duration()) / 2
	s.ZoomTo(mid-40_000, mid+40_000)
	tt.Frame()
	if _, done, _ := rs.status(); done || s.perf.blocks == 0 {
		t.Errorf("zoomed in: complete %v, %d blocks (want the coarser tiles meanwhile)", done, s.perf.blocks)
	}
	waitFor(t, tt, "the finer tiles", func() bool { _, done, _ := rs.status(); return done })

	// What it hits is what the trace in memory has there.
	ls := newLocalSource(local)
	hits := 0
	for ti, tk := range s.tr.Tracks {
		for li := range tk.Lanes {
			for k := range 40 {
				at := mid - 40_000 + float64(k)*2000
				rb, rok := s.src.hit(ti, li, at, 3*s.tl.cur.scale, s.tl.cur.scale)
				lb, lok := ls.hit(ti, li, at, 3*s.tl.cur.scale, s.tl.cur.scale)
				if rok != lok || rok && (rb.index != lb.index || rb.start != lb.start || rb.name != lb.name) {
					t.Fatalf("row %d lane %d at %.0f: server %+v %v, memory %+v %v", ti, li, at, rb, rok, lb, lok)
				}
				if rok {
					hits++
				}
			}
		}
	}
	if hits == 0 {
		t.Fatal("nothing hit")
	}

	// Details, from the server.
	ref := trace.Ref{Track: 0, Lane: 0, Index: int32(local.Tracks[0].Lanes[0].FirstEnding(local.Start + int64(mid)))}
	s.Select(ref)
	tt.Frame()
	waitFor(t, tt, "the details", func() bool { return tt.HasText("Launched by") })
	if !tt.HasText("cudaLaunchKernel") {
		t.Errorf("details: %q", tt.Texts())
	}
	// Summaries, of the trace and of a range.
	if err := tt.Click("Top kernels"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "the summary", func() bool { return tt.HasText("gemm") })
	s.tl.rangeA, s.tl.rangeB, s.tl.hasRange = mid-40_000, mid+40_000, true
	s.rangeEnded()
	waitFor(t, tt, "the summary of the range", func() bool { st, ok := s.stats(trace.KindKernel); return ok && len(st) > 0 })
	st, _ := s.stats(trace.KindKernel)
	want := local.TopInRange(100, local.Start+s.sumRange.from, local.Start+s.sumRange.to, trace.KindKernel)
	if !slices.Equal(st, want) {
		t.Errorf("summary of the range: %+v, want %+v", st, want)
	}

	// Download for offline: the trace from memory, the view kept.
	view := s.tl.cur
	if err := tt.Click("Download for offline"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "the download", func() bool { _, ok := s.src.(*localSource); return ok })
	if s.tl.cur != view || s.tr.EventCount() != local.EventCount() || tt.HasText("Download for offline") {
		t.Error("the download lost the view, or events")
	}
}

// A folder on the server opens as a session of streamed traces; the path
// box opens paths on the server when connected, and remembers them there.
func TestRemoteSession(t *testing.T) {
	root := jobDir(t, 4)
	url := testServer(t, root, "", 5*time.Millisecond)
	s := New(nil)
	var kept Settings
	s.OnSettings(func(st Settings) { kept = st })
	tt := ui.NewTester(s.View, w, h)
	s.Connect(url, "", "")
	waitFor(t, tt, "the connection", func() bool { _, ok := s.Connected(); return ok })
	s.ctl.focusPath = true
	tt.Frame()
	tt.Frame()
	tt.Type(root)
	tt.Key(0, ui.KeyEnter)
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	ss, ok := s.src.(*sessionSource)
	if !ok || len(ss.parts) != 4 {
		t.Fatalf("source %T", s.src)
	}
	for _, p := range ss.parts {
		if _, ok := p.src.(*remoteSource); !ok {
			t.Fatalf("part %s is %T", p.label, p.src)
		}
	}
	if ss.parts[3].offset != 3_000_000 {
		t.Errorf("rank3 at %d", ss.parts[3].offset)
	}
	waitFor(t, tt, "the tiles", func() bool { return s.perf.blocks > 0 && s.src.loading() == 0 })
	if got := kept.RecentServerPaths[url+"/"]; len(got) == 0 || got[0] != root {
		t.Errorf("recent server paths %+v", kept.RecentServerPaths)
	}
	// Suggestions come from the server's folder.
	s.ctl.pathText = root + "/"
	s.suggestions()
	waitFor(t, tt, "the server's suggestions", func() bool { return slices.Contains(s.suggestions(), root+"/rank3.sqlite") })
}

// A URL copied from the browser opens what it links to: in the connect
// dialog, in the path box (connecting first), with its token; the web
// page's own URL connects.
func TestDeepLinks(t *testing.T) {
	root := jobDir(t, 3)
	url := testServer(t, root, "", 0)
	opened := func(s *App, n int) func() bool {
		return func() bool {
			ss, ok := s.src.(*sessionSource)
			return ok && len(ss.parts) == n && s.loading == ""
		}
	}

	// The connect dialog's field, a link to a folder.
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(url+"/?path="+root+"#v=1000000,3000000&align=start", "", "")
	waitFor(t, tt, "the linked folder", opened(s, 3))
	if srv, ok := s.Connected(); !ok || srv != url+"/" {
		t.Errorf("connected to %q", srv)
	}
	// Its view and alignment, as the link says, and back as a link.
	tt.Frame()
	if a, b, ok := s.Shown(); !ok || math.Abs(a-1e6) > 1 || math.Abs(b-3e6) > 1 || s.src.(*sessionSource).alignMode() != "start" {
		t.Errorf("the link's view: %v–%v (%v), aligned on %q", a, b, ok, s.src.(*sessionSource).alignMode())
	}
	if l, ok := s.Link(); !ok || !slices.Equal(l.Paths, []string{root}) || math.Abs(l.View[0]-1e6) > 1 || l.Align != "start" {
		t.Errorf("Link() = %+v %v", l, ok)
	}

	// The path box, not connected: a link to two files connects and opens them.
	s = New(nil)
	tt = ui.NewTester(s.View, w, h)
	s.OpenPath(url + "/?path=" + root + "/rank0.sqlite&path=" + root + "/rank2.sqlite")
	waitFor(t, tt, "the linked files", opened(s, 2))
	// Connected: another link to the same server opens there; a glob too.
	s.OpenPath(url + "/index.html?path=" + root + "/rank%5B01%5D.sqlite")
	waitFor(t, tt, "the linked glob", func() bool {
		ss, ok := s.src.(*sessionSource)
		return ok && len(ss.parts) == 2 && ss.parts[1].label == "rank1 (node0)" && s.loading == ""
	})
	// The web page's own URL, connected: the server's traces.
	s.OpenPath(url + "/")
	if s.picker == nil || !s.picker.shown {
		t.Error("the server's URL did not list its traces")
	}

	// A token in the link's fragment.
	secret := testServer(t, root, "s3cret", 0)
	s = New(nil)
	tt = ui.NewTester(s.View, w, h)
	s.Connect(secret+"/index.html?path="+root+"/rank1.sqlite#token=s3cret", "", "")
	waitFor(t, tt, "the linked file, with the token", func() bool { _, ok := s.src.(*remoteSource); return ok && s.loading == "" })

	// Something that is not Tracelight's API says so.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><p>login"))
	}))
	defer html.Close()
	s.Connect(html.URL, "", "")
	waitFor(t, tt, "the refusal", func() bool { return s.ctl.state == connFailed })
	if !strings.Contains(s.ctl.err.Error(), "answered /api/info with HTML (200 OK), not Tracelight's API") {
		t.Errorf("error %v", s.ctl.err)
	}
}

// testGateway is a sign-in gateway in front of a server serving root: it
// redirects requests without its session cookie to a sign-in host (which
// counts the requests it gets), and serves the server under /tl/.
func testGateway(t *testing.T, root string) (gw string, ssoHits *int) {
	gw, ssoHits, _ = testGatewaySessions(t, root)
	return gw, ssoHits
}

// testGatewaySessions is testGateway with the session cookie it takes
// (gw_session=ok at first), to end sessions.
func testGatewaySessions(t *testing.T, root string) (gw string, ssoHits *int, session *atomic.Value) {
	t.Helper()
	srv, err := server.New(server.Config{Root: root, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	session = &atomic.Value{}
	session.Store("ok")
	sso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><title>Sign in</title>"))
	}))
	t.Cleanup(sso.Close)
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("gw_session"); err != nil || c.Value != session.Load().(string) {
			http.Redirect(w, r, sso.URL+"/cas/login?service=x", http.StatusFound)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/tl/") {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix("/tl", srv).ServeHTTP(w, r)
	}))
	t.Cleanup(g.Close)
	return g.URL, &hits, session
}

// A gateway session that ends while traces stream: the sign-in window
// opens again, and once signed in, the traces keep streaming with the same
// client (nothing reopens).
func TestSignInAgain(t *testing.T) {
	root := jobDir(t, 2)
	gw, _, session := testGatewaySessions(t, root)
	signIns := 0
	s := New(nil)
	s.SetAnimations(false)
	s.SetSignIn(func(b string, check func(string) error, done func(string, error)) {
		signIns++
		cookie := "gw_session=" + session.Load().(string)
		go done(cookie, check(cookie))
	})
	tt := ui.NewTester(s.View, w, h)
	s.Connect(gw+"/tl/?path="+root+"/rank0.sqlite", "", "")
	waitFor(t, tt, "the trace through the gateway", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" && s.perf.blocks > 0 })
	client, src := s.ctl.client, s.src
	session.Store("renewed")       // the gateway's session ends
	s.ZoomTo(1_000_000, 1_050_000) // finer tiles: new requests
	waitFor(t, tt, "the sign-in again, and the tiles after it", func() bool {
		tt.Frame()
		_, _, blank := s.StreamSlots()
		return signIns == 2 && s.ctl.state == connConnected && blank == 0 && s.perf.blocks > 0
	})
	if s.ctl.client != client || s.src != src || s.ctl.settings.ServerCookies[gw+"/tl/"] != "gw_session=renewed" {
		t.Errorf("after signing in again: same client %v, same source %v, cookie %q", s.ctl.client == client, s.src == src, s.ctl.settings.ServerCookies[gw+"/tl/"])
	}
}

// Through a sign-in gateway: pasting its address signs in in the sign-in
// window at once, keeps the session for the server and opens the link's
// path; the session kept connects the next time without signing in; an
// expired one is forgotten and asked again; a Cookie header pasted works.
func TestSignInGateway(t *testing.T) {
	root := jobDir(t, 2)
	gw, ssoHits := testGateway(t, root)
	base := gw + "/tl/"
	var kept Settings
	signIns := 0
	window := func(b string, check func(string) error, done func(string, error)) {
		signIns++
		if b != base {
			t.Errorf("signing in at %q", b)
		}
		go func() {
			if check("gw_session=no") == nil {
				t.Error("a wrong session got through")
			}
			if err := check("gw_session=ok"); err != nil {
				t.Errorf("check: %v", err)
			}
			done("gw_session=ok", nil)
		}()
	}

	s := New(nil)
	s.OnSettings(func(st Settings) { kept = st })
	s.SetSignIn(window)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(gw+"/tl?path="+root, "", "") // the prefix without its "/"
	waitFor(t, tt, "the linked folder through the gateway", func() bool {
		ss, ok := s.src.(*sessionSource)
		return ok && len(ss.parts) == 2 && s.loading == ""
	})
	waitFor(t, tt, "the tiles through the gateway", func() bool { return s.perf.blocks > 0 && s.src.loading() == 0 })
	if signIns != 1 || kept.ServerCookies[base] != "gw_session=ok" {
		t.Errorf("%d sign-ins, kept %v", signIns, kept.ServerCookies)
	}

	// The next time: the session kept, no sign-in.
	s2 := New(nil)
	s2.SetSettings(kept)
	s2.SetSignIn(func(string, func(string) error, func(string, error)) { t.Error("signed in again") })
	tt2 := ui.NewTester(s2.View, w, h)
	s2.Connect(base, "", "")
	waitFor(t, tt2, "the connection with the session kept", func() bool { _, ok := s2.Connected(); return ok })

	// An expired session, no sign-in window: forgotten, and asked for.
	s3 := New(nil)
	var kept3 Settings
	s3.SetSettings(Settings{ServerCookies: map[string]string{base: "gw_session=expired"}})
	s3.OnSettings(func(st Settings) { kept3 = st })
	tt3 := ui.NewTester(s3.View, w, h)
	s3.Connect(base+"?path="+root+"/rank1.sqlite", "", "")
	waitFor(t, tt3, "the sign-in request", func() bool { return s3.ctl.state == connSignIn })
	if _, ok := kept3.ServerCookies[base]; ok || !strings.Contains(s3.ctl.err.Error(), "behind a sign-in page") {
		t.Errorf("expired session: kept %v, error %v", kept3.ServerCookies, s3.ctl.err)
	}
	s3.openConnect()
	tt3.Frame()
	if !tt3.HasText("This server is behind a sign-in page.") || !tt3.HasText("Sign-in needed") {
		t.Errorf("the dialog: %q", tt3.Texts())
	}
	// A Cookie header pasted from a browser: connects, opens the link's file.
	s3.UseCookie("Cookie: gw_session=ok")
	waitFor(t, tt3, "the linked file with the pasted cookie", func() bool { _, ok := s3.src.(*remoteSource); return ok && s3.loading == "" })
	if kept3.ServerCookies[base] != "gw_session=ok" {
		t.Errorf("the pasted cookie was not kept: %v", kept3.ServerCookies)
	}
	if *ssoHits != 0 {
		t.Errorf("the sign-in host got %d requests from the app", *ssoHits)
	}
}

// Ranks that started capturing at different moments show it: a rank's
// export by its wall clock; a report of the same host (reports store no
// wall clock) by the host's monotonic clock against that export; reports
// of a host without an export by its clock among themselves; "Starts"
// puts every first event at 0.
func TestRankClocks(t *testing.T) {
	dir := t.TempDir()
	const utc0, mono0, mono7 = 1_792_411_200_000_000_000, 1_515_000_000_000_000, 2_000_000_000_000_000
	writeSQLiteClocks(t, filepath.Join(dir, "rank0.sqlite"), 300, utc0, mono0, "node0")
	report := func(name string, mono int64, host string) {
		data := nsysrep.Synthetic("2026@3@7@1-2", nsysrep.Session{MonoStart: mono, Host: host})
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	report("rank1.nsys-rep", mono0+120_000_000, "node0") // 120 ms after rank0, same host
	report("rank2.nsys-rep", mono7+40_000_000, "node7")  // another host, no export
	report("rank3.nsys-rep", mono7, "node7")

	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	s.OpenPath(dir)
	waitFor(t, tt, "the session", func() bool { ss, ok := s.src.(*sessionSource); return ok && len(ss.parts) == 4 && s.loading == "" })
	ss := s.src.(*sessionSource)
	p := ss.parts
	if p[1].tr.SessionStartMono != mono0+120_000_000 || p[1].tr.Host != "node0" || p[1].tr.SessionStartUTC != 0 {
		t.Fatalf("the report's session: mono %d host %q utc %d", p[1].tr.SessionStartMono, p[1].tr.Host, p[1].tr.SessionStartUTC)
	}
	// Offsets are of first events: each trace's Start counts from its
	// capture's start.
	want := []struct {
		offset int64
		basis  string
	}{
		{0, "wall"},
		{120_000_000 + p[1].tr.Start - p[0].tr.Start, "host"},
		{40_000_000 + p[2].tr.Start - p[3].tr.Start, "monotonic"},
		{0, "monotonic"},
	}
	for i, w := range want {
		if p[i].offset != w.offset || p[i].basis != w.basis {
			t.Errorf("%s: offset %d (%s), want %d (%s)", p[i].label, p[i].offset, p[i].basis, w.offset, w.basis)
		}
	}
	tt.Frame()
	if !tt.HasText("Collectives") || !tt.HasText("Clocks") {
		t.Error("no alignment control")
	}
	s.SetAlignStarts(true)
	for _, q := range ss.parts {
		if q.offset != 0 || q.basis != "start" {
			t.Errorf("Starts: %s at %d (%s)", q.label, q.offset, q.basis)
		}
	}
}

// A session streamed from a server, in the rank overview: tiles come in
// batches of several traces, holding only the overview's lanes; traces
// whose rows do not show fetch nothing until they do.
func TestStreamBatches(t *testing.T) {
	root := jobDir(t, 20)
	srv, err := server.New(server.Config{Root: root, Open: trace.OpenOptions{Nsys: "/nonexistent/nsys", CacheDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	tilesAsked := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		if r.URL.Path == "/api/tiles" {
			counts["lanes="+r.URL.Query().Get("lanes")]++
			tilesAsked += len(r.URL.Query()["t"])
		}
		mu.Unlock()
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(ts.URL+"/?path="+root, "", "")
	waitFor(t, tt, "the session", func() bool { ss, ok := s.src.(*sessionSource); return ok && len(ss.parts) == 20 && s.loading == "" })
	waitFor(t, tt, "the tiles", func() bool { _, done, n := s.StreamStats(); return done && n == 0 && s.perf.blocks > 0 })
	ss := s.src.(*sessionSource)
	if !s.compactOn() {
		t.Fatal("20 traces did not open in the rank overview")
	}
	mu.Lock()
	if counts["/api/tile"] != 0 || counts["/api/tiles"] == 0 || counts["/api/tiles"] >= tilesAsked || counts["lanes=gpu,nvtx"] != counts["/api/tiles"] {
		t.Errorf("requests %v for %d tiles", counts, tilesAsked)
	}
	mu.Unlock()
	last := ss.parts[19].src.(*remoteSource)
	last.mu.Lock()
	n := len(last.tiles)
	last.mu.Unlock()
	if !slices.Contains(ss.visible, false) || ss.visible[19] || n != 0 {
		t.Fatalf("the last trace, off screen: visible %v, %d tiles", ss.visible, n)
	}
	// Scrolled to it, it fetches.
	s.ScrollTo(1e6)
	waitFor(t, tt, "the last trace's tiles", func() bool {
		last.mu.Lock()
		defer last.mu.Unlock()
		return len(last.tiles) > 0 && last.slots[0] > 0
	})
}
