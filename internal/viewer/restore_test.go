package viewer

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// roundTrip passes links through JSON, as session.json keeps them.
func roundTrip(t *testing.T, links []TabLink) []TabLink {
	t.Helper()
	data, err := json.Marshal(links)
	if err != nil {
		t.Fatal(err)
	}
	var out []TabLink
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func near(a, b [2]float64) bool {
	span := b[1] - b[0]
	return math.Abs(a[0]-b[0]) < span*1e-3 && math.Abs(a[1]-b[1]) < span*1e-3
}

func opened(s *App) bool { return s.tr != nil && s.loading == "" }

// The tabs of a session restored: in order, the one shown opened at once
// at its view, alignment and traces shown, the others when first shown,
// each at its own.
func TestRestoreSession(t *testing.T) {
	dir := jobDir(t, 3)
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	s.OpenPath(filepath.Join(dir, "rank0.sqlite"))
	waitFor(t, tt, "rank0", func() bool { return opened(s) })
	s.ZoomTo(1_000_000, 3_000_000)
	tt.Frame()
	s.NewTab()
	s.OpenPath(dir)
	waitFor(t, tt, "the session", func() bool { return opened(s) })
	s.SetAlign("start")
	s.SetTraceShown(1, false)
	s.ZoomTo(2_000_000, 2_500_000)
	tt.Frame()
	links, cur := s.TabLinks()
	if len(links) != 2 || cur != 1 || links[1].Align != "start" || len(links[1].Only)+len(links[1].Omit) == 0 || links[0].Server != "" {
		t.Fatalf("links %+v, cur %d", links, cur)
	}
	links = roundTrip(t, links)

	r := New(nil)
	r.SetAnimations(false)
	rt := ui.NewTester(r.View, w, h)
	rt.Frame()
	r.RestoreSession(links, cur, true)
	waitFor(t, rt, "the tab shown", func() bool { return opened(r) })
	rt.Frame()
	if n, c := r.Tabs(); n != 2 || c != 1 {
		t.Fatalf("restored %d tabs, %d shown", n, c)
	}
	a, b, _ := r.Shown()
	if !near([2]float64{a, b}, links[1].View) {
		t.Errorf("the session at %v, kept %v", [2]float64{a, b}, links[1].View)
	}
	if shown, all := r.TracesShown(); shown != 2 || all != 3 || r.session().alignMode() != "start" {
		t.Errorf("session: %d of %d shown, aligned %q", shown, all, r.session().alignMode())
	}
	if r.ctl.tabs[0].app.tr != nil {
		t.Error("the tab not shown opened before it was shown")
	}
	// What it keeps meanwhile is what it was.
	if l2, _ := r.TabLinks(); !near(l2[0].View, links[0].View) || l2[0].Paths[0] != links[0].Paths[0] {
		t.Errorf("kept before opening: %+v", l2[0])
	}
	r.ShowTab(0)
	waitFor(t, rt, "the other tab", func() bool { return opened(r) })
	rt.Frame()
	a, b, _ = r.Shown()
	if !near([2]float64{a, b}, links[0].View) {
		t.Errorf("rank0 at %v, kept %v", [2]float64{a, b}, links[0].View)
	}

	// Not opened now (the command line names a file): the tabs come back
	// before an empty tab, shown, which the file then takes.
	q := New(nil)
	qt := ui.NewTester(q.View, w, h)
	qt.Frame()
	q.RestoreSession(links, cur, false)
	if n, c := q.Tabs(); n != 3 || c != 2 || !q.Empty() {
		t.Fatalf("restored for later: %d tabs, %d shown, empty %v", n, c, q.Empty())
	}
	q.OpenLocal(filepath.Join(dir, "rank2.sqlite"))
	waitFor(t, qt, "the file named", func() bool { return opened(q) })
	if n, _ := q.Tabs(); n != 3 || filepath.Base(q.inputs) != "rank2.sqlite" {
		t.Errorf("the file opened in %d tabs, %q", n, q.inputs)
	}
}

// A tab whose files are gone says so when it opens, and is kept as it was
// for the next start; the other tabs open as they were.
func TestRestoreMissingFiles(t *testing.T) {
	dir := jobDir(t, 2)
	links := []TabLink{
		{Paths: []string{filepath.Join(dir, "rank0.sqlite")}, View: [2]float64{1e6, 2e6}},
		{Paths: []string{filepath.Join(t.TempDir(), "moved", "rank1.sqlite")}, View: [2]float64{3e6, 4e6}},
	}
	r := New(nil)
	r.SetAnimations(false)
	rt := ui.NewTester(r.View, w, h)
	rt.Frame()
	r.RestoreSession(links, 1, true)
	waitFor(t, rt, "the error", func() bool { rt.Frame(); return r.loadErr != nil })
	if !strings.Contains(r.loadErr.Error(), "no such file") || !strings.Contains(strings.Join(rt.Texts(), " "), "Could not open") {
		t.Errorf("error %v; showing %q", r.loadErr, rt.Texts())
	}
	if l, _ := r.TabLinks(); l[1].Paths[0] != links[1].Paths[0] || l[1].View != links[1].View {
		t.Errorf("the missing tab kept as %+v", l[1])
	}
	r.ShowTab(0)
	waitFor(t, rt, "the other tab", func() bool { return opened(r) })
}

// A tab of a server comes back on it: the app connects (with no token, as
// none is kept) and opens it at its view; a server that cannot be reached
// says so in the tab, whose path box holds the server's link to try again.
func TestRestoreRemote(t *testing.T) {
	root := jobDir(t, 2)
	url := testServer(t, root, "", 0)
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(url, "", root)
	waitFor(t, tt, "the session", func() bool { return opened(s) })
	s.ZoomTo(1_500_000, 2_500_000)
	tt.Frame()
	links, cur := s.TabLinks()
	if links[0].Server != url+"/" || links[0].Paths[0] != root {
		t.Fatalf("link %+v", links[0])
	}
	links = roundTrip(t, links)
	r := New(nil)
	r.SetAnimations(false)
	rt := ui.NewTester(r.View, w, h)
	rt.Frame()
	r.RestoreSession(links, cur, true)
	waitFor(t, rt, "the session from the server", func() bool { return opened(r) })
	rt.Frame()
	if _, ok := r.Connected(); !ok {
		t.Error("not connected")
	}
	if _, ok := r.src.(*sessionSource); !ok {
		t.Errorf("source %T", r.src)
	}
	a, b, _ := r.Shown()
	if !near([2]float64{a, b}, links[0].View) {
		t.Errorf("at %v, kept %v", [2]float64{a, b}, links[0].View)
	}

	// A server gone.
	gone := []TabLink{{Server: "http://127.0.0.1:1/", Paths: []string{"/data/job"}, View: [2]float64{1, 2}}}
	g := New(nil)
	gt := ui.NewTester(g.View, w, h)
	gt.Frame()
	g.RestoreSession(gone, 0, true)
	waitFor(t, gt, "the error", func() bool { gt.Frame(); return g.loadErr != nil })
	if !strings.Contains(g.loadErr.Error(), "could not reach") || g.ctl.pathText != "http://127.0.0.1:1/?path=/data/job" {
		t.Errorf("error %v, path box %q", g.loadErr, g.ctl.pathText)
	}
	if l, _ := g.TabLinks(); l[0].Server != gone[0].Server || l[0].View != gone[0].View {
		t.Errorf("the tab kept as %+v", l[0])
	}
}

// Empty tabs are not kept; nothing to restore leaves the window as it was.
func TestRestoreNothing(t *testing.T) {
	r := New(nil)
	rt := ui.NewTester(r.View, w, h)
	rt.Frame()
	r.RestoreSession([]TabLink{{}, {}}, 1, true)
	r.RestoreSession(nil, 0, true)
	if n, _ := r.Tabs(); n != 1 || !r.Empty() {
		t.Errorf("%d tabs", n)
	}
}
