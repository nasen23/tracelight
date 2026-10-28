package viewer

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/trace"
)

// The loading screen lists each trace loading, with how far it is, and
// how far they all are, built anew as they go.
func TestLoadingScreen(t *testing.T) {
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	items := []loadItem{{name: "rank0.sqlite", size: 100}, {name: "rank1.sqlite", size: 100}, {name: "rank2.sqlite", size: 200}}
	for i := range items {
		items[i].prog = &trace.Progress{}
	}
	s.SetLoading("/runs/job", nil)
	s.loadItems = items
	items[0].prog.Finish(nil)
	items[1].prog.Report(trace.PhaseReading, 50, 100)
	tt.Frame()
	has := func(want ...string) {
		t.Helper()
		texts := strings.Join(tt.Texts(), " | ")
		for _, w := range want {
			if !strings.Contains(texts, w) {
				t.Errorf("no %q in %s", w, texts)
			}
		}
	}
	// 100 done, 100 at 0.475, 200 queued: 147.5 of 400.
	has("Loading 3 traces…", "1 of 3 done · 36%", "rank0.sqlite", "done", "reading 47%", "queued")
	items[1].prog.Finish(nil)
	items[2].prog.Finish(errBoom)
	tt.Frame()
	has("2 of 3 done · 50% · 1 failed", "failed: boom")

	// One trace: its bar and where it is.
	s.loadItems = items[1:2]
	s.SetLoading("/runs/rank1.sqlite", nil)
	items[1].prog.Report(trace.PhaseBuilding, 0, 0)
	tt.Frame()
	has("Loading rank1.sqlite…", "building")
}

type boom struct{}

func (boom) Error() string { return "boom" }

var errBoom error = boom{}

// Opening a folder on this machine lists its traces on the loading screen
// at once, each done once loaded; braces pick some of them.
func TestOpenFolderShowsEachTrace(t *testing.T) {
	dir := jobDir(t, 4)
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	s.OpenPath(dir)
	items := s.loadItems
	var names []string
	for _, it := range items {
		names = append(names, it.name)
	}
	if !slices.Equal(names, []string{"rank0.sqlite", "rank1.sqlite", "rank2.sqlite", "rank3.sqlite"}) {
		t.Fatalf("loading %q", names)
	}
	tt.Frame()
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	for _, it := range items {
		if it.prog.Phase() != trace.PhaseDone || it.prog.Fraction() != 1 {
			t.Errorf("%s: %v %.2f after loading", it.name, it.prog.Phase(), it.prog.Fraction())
		}
	}
	if s.loadItems != nil {
		t.Error("the loading lines outlive the load")
	}
	for pattern, want := range map[string]int{"rank{0,2}.sqlite": 2, "rank{1..3}.sqlite": 3, "rank{0,3}*": 2} {
		s.NewTab()
		s.OpenPath(filepath.Join(dir, pattern))
		waitFor(t, tt, pattern, func() bool { return s.tr != nil && s.loading == "" })
		if ss, ok := s.src.(*sessionSource); !ok || len(ss.parts) != want {
			t.Errorf("%s: %T, want a session of %d", pattern, s.src, want)
		}
	}
}

// From a server: a line a path while it lists them, then a line a trace,
// each done as its rows come; braces work there too.
func TestRemoteLoadingShowsEachTrace(t *testing.T) {
	root := jobDir(t, 4)
	url := testServer(t, root, "", 20*time.Millisecond)
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(url, "", "")
	waitFor(t, tt, "the connection", func() bool { _, ok := s.Connected(); return ok })
	s.OpenPath(root)
	if len(s.loadItems) != 1 || !s.loadItems[0].listing {
		t.Fatalf("while listing: %+v", s.loadItems)
	}
	var items []loadItem
	waitFor(t, tt, "a line a trace", func() bool {
		if len(s.loadItems) == 4 {
			items = s.loadItems
		}
		return items != nil || s.loading == ""
	})
	if len(items) != 4 || items[2].name != "rank2.sqlite" || items[2].size == 0 {
		t.Fatalf("loading %+v", items)
	}
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	for _, it := range items {
		if it.prog.Phase() != trace.PhaseDone {
			t.Errorf("%s: %v after loading", it.name, it.prog.Phase())
		}
	}
	s.NewTab()
	s.OpenPath(root + "/rank{1,3}.sqlite")
	waitFor(t, tt, "braces", func() bool { return s.tr != nil && s.loading == "" })
	if ss, ok := s.src.(*sessionSource); !ok || len(ss.parts) != 2 {
		t.Errorf("rank{1,3}: %T", s.src)
	}
}

// Opening traces on a server, as it streams them: the loading screen from
// the moment the app connects (a line a path), then the timeline, which
// says while its tiles have not come that it is loading (a bar along its
// top; a rank without tiles "loading…", not "GPU idle 100%"), not that it
// is done and idle.
func TestRemoteViewLoading(t *testing.T) {
	root := jobDir(t, 6)
	url := testServer(t, root, "", 300*time.Millisecond)
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	s.Connect(url, "", root)
	if s.loading == "" || len(s.loadItems) != 1 || !s.loadItems[0].listing {
		t.Fatalf("while connecting: loading %q, lines %+v", s.loading, s.loadItems)
	}
	tt.Frame()
	if !strings.Contains(strings.Join(tt.Texts(), " "), "listing…") {
		t.Errorf("while connecting, the window shows %q", tt.Texts())
	}
	waitFor(t, tt, "the session", func() bool { return s.tr != nil && s.loading == "" })
	tt.Frame()
	if !s.viewLoading() {
		t.Fatal("the view's tiles not come, and nothing says so")
	}
	waiting := 0
	for _, rk := range s.tl.ranks {
		if rk.idle == rankLoading {
			waiting++
		}
		if rk.idle == 1 {
			t.Errorf("%s: GPU idle 100%% before its tiles came", rk.label)
		}
	}
	if waiting == 0 {
		t.Errorf("no rank says it is loading: %+v", s.tl.ranks)
	}
	waitFor(t, tt, "the tiles", func() bool { tt.Frame(); return !s.viewLoading() })
	tt.Frame()
	for _, rk := range s.tl.ranks {
		if rk.idle == rankLoading {
			t.Errorf("%s still loading once its tiles came", rk.label)
		}
	}
}

// A server's link typed that cannot be reached: the tab says so, the link
// kept in the path box to try again.
func TestRemoteLinkUnreachable(t *testing.T) {
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	s.OpenPath("http://127.0.0.1:1/?path=/data/job")
	if s.loading == "" {
		t.Fatal("no loading screen while connecting")
	}
	waitFor(t, tt, "the error", func() bool { tt.Frame(); return s.loadErr != nil })
	if !strings.Contains(s.loadErr.Error(), "could not reach") || s.ctl.pathText != "http://127.0.0.1:1/?path=/data/job" {
		t.Errorf("error %v, path box %q", s.loadErr, s.ctl.pathText)
	}
}
