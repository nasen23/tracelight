package viewer

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// Tabs keep timelines open side by side: Ctrl/⌘+T opens an empty one, a
// trace opened there loads even while another tab is shown, and lands in
// its own tab; switching (Ctrl+Tab, Ctrl/⌘+digit, Alt+[ ]) keeps each tab's
// view; Ctrl/⌘+W closes; files from outside open in a new tab, or show the
// tab that has them.
func TestTabs(t *testing.T) {
	dir := jobDir(t, 3)
	a, b, c := filepath.Join(dir, "rank0.sqlite"), filepath.Join(dir, "rank1.sqlite"), filepath.Join(dir, "rank2.sqlite")
	s := New(nil)
	s.SetAnimations(false)
	var titles []string
	s.OnTitle(func(v string) { titles = append(titles, v) })
	tt := ui.NewTester(s.View, w, h)
	s.OpenLocal(a)
	waitFor(t, tt, "the first trace", func() bool { return s.tr != nil && s.loading == "" })
	tt.Frame()
	if n, _ := s.Tabs(); n != 1 || !tt.HasText("rank0") {
		t.Fatalf("%d tabs, texts %q", n, tt.Texts())
	}
	s.ZoomTo(1_000_000, 2_000_000)
	tt.Frame()
	viewA := s.tl.cur

	// Ctrl/⌘+T: an empty tab, shown.
	tt.Key(ui.Cmd, ui.KeyT)
	tt.Frame()
	if n, cur := s.Tabs(); n != 2 || cur != 1 || s.tr != nil || !tt.HasText("New tab") {
		t.Fatalf("Ctrl/⌘+T: %d tabs, shown %d, trace %v", n, cur, s.tr != nil)
	}
	// Open b here, and go back to the first tab before it is loaded: it
	// lands in its own tab, the first one untouched.
	s.OpenPath(b)
	s.ShowTab(0)
	tt.Frame()
	if s.tr == nil || s.tr.Path != a || s.tl.cur != viewA {
		t.Fatalf("back on the first tab: %v, view %+v (was %+v)", s.tr != nil, s.tl.cur, viewA)
	}
	waitFor(t, tt, "the second tab's trace, in the background", func() bool {
		tt.Frame()
		tb := s.ctl.tabs[1].app
		return tb.tr != nil && tb.loading == ""
	})
	if s.tr.Path != a || s.tl.cur != viewA || s.ctl.tabs[1].app.tr.Path != b {
		t.Fatalf("after the background load: shown %s, second tab %s", s.tr.Path, s.ctl.tabs[1].app.tr.Path)
	}
	if got := s.tabTitles(); !slices.Equal(got, []string{"rank0", "rank1"}) {
		t.Errorf("tab titles %q", got)
	}
	// Ctrl+Tab: the second; Ctrl/⌘+1: the first, its view as it was.
	tt.Key(ui.Ctrl, ui.KeyTab)
	tt.Frame()
	if _, cur := s.Tabs(); cur != 1 || s.tr.Path != b || titles[len(titles)-1] != Title(b) {
		t.Fatalf("Ctrl+Tab: shown %d (%s), title %q", cur, s.tr.Path, titles[len(titles)-1])
	}
	s.fit(false)
	viewB := s.tl.cur
	tt.Key(ui.Cmd, ui.Key1)
	tt.Frame()
	if _, cur := s.Tabs(); cur != 0 || s.tl.cur != viewA {
		t.Fatalf("Ctrl/⌘+1: shown %d, view %+v, want %+v", cur, s.tl.cur, viewA)
	}
	tt.Key(ui.Alt, ui.KeyBracketRight)
	tt.Frame()
	if _, cur := s.Tabs(); cur != 1 || s.tl.cur != viewB {
		t.Fatalf("Alt+]: shown %d, view %+v, want %+v", cur, s.tl.cur, viewB)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyTab)
	tt.Frame()
	if _, cur := s.Tabs(); cur != 0 {
		t.Fatalf("Ctrl+Shift+Tab: shown %d", cur)
	}

	// From outside: a file a tab has shows that tab; another opens in a
	// new tab, after the one shown.
	s.OpenLocal(b)
	if n, cur := s.Tabs(); n != 2 || cur != 1 {
		t.Fatalf("opening b again: %d tabs, shown %d", n, cur)
	}
	s.OpenLocal(c)
	waitFor(t, tt, "the third trace", func() bool { return s.tr != nil && s.loading == "" })
	if n, cur := s.Tabs(); n != 3 || cur != 2 || s.tr.Path != c {
		t.Fatalf("opening c: %d tabs, shown %d", n, cur)
	}
	tt.Key(ui.Cmd, ui.Key9)
	tt.Frame()
	if _, cur := s.Tabs(); cur != 2 {
		t.Fatalf("Ctrl/⌘+9: shown %d, want the last", cur)
	}
	// Ctrl/⌘+W: closes it, the one before shown; Alt+W the next.
	tt.Key(ui.Cmd, ui.KeyW)
	tt.Frame()
	if n, cur := s.Tabs(); n != 2 || cur != 1 || s.tr.Path != b {
		t.Fatalf("Ctrl/⌘+W: %d tabs, shown %d", n, cur)
	}
	// The ×: closes the tab it is on.
	if err := tt.Click("Close tab rank0"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if n, cur := s.Tabs(); n != 1 || cur != 0 || s.tr.Path != b {
		t.Fatalf("× on the first: %d tabs, shown %d", n, cur)
	}
	// The last one closed leaves an empty tab.
	tt.Key(ui.Alt, ui.KeyW)
	tt.Frame()
	if n, _ := s.Tabs(); n != 1 || s.tr != nil || !tt.HasText("New tab") {
		t.Fatalf("closing the last: %d tabs, trace %v", n, s.tr != nil)
	}
	// + opens a tab; the path box's Ctrl/⌘+Enter opens there in a new one.
	s.OpenLocal(a)
	waitFor(t, tt, "a again", func() bool { return s.tr != nil && s.loading == "" })
	if err := tt.Click("New tab"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if n, cur := s.Tabs(); n != 2 || cur != 1 {
		t.Fatalf("+: %d tabs, shown %d", n, cur)
	}
	s.ShowTab(0)
	tt.Frame()
	s.ctl.focusPath = true
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type(b)
	tt.Key(ui.Cmd, ui.KeyEnter)
	waitFor(t, tt, "b in a new tab", func() bool { tt.Frame(); return s.tr != nil && s.tr.Path == b && s.loading == "" })
	if n, cur := s.Tabs(); n != 3 || cur != 1 || s.ctl.tabs[0].app.tr.Path != a {
		t.Fatalf("Ctrl/⌘+Enter: %d tabs, shown %d", n, cur)
	}
}

// Disconnecting closes the tabs streamed from the server, and only those;
// an open from it still on its way lands nowhere.
func TestTabsDisconnect(t *testing.T) {
	root := jobDir(t, 2)
	url := testServer(t, root, "", 50*time.Millisecond)
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	local := filepath.Join(root, "rank0.sqlite")
	s.OpenLocal(local)
	waitFor(t, tt, "the local trace", func() bool { return s.tr != nil && s.loading == "" })
	s.NewTab()
	s.Connect(url, "", filepath.Join(root, "rank1.sqlite"))
	waitFor(t, tt, "the streamed trace", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" && s.streamed() })
	// Hidden, a streamed tab keeps only its most recently drawn tiles.
	rs := s.src.(*remoteSource)
	waitFor(t, tt, "tiles", func() bool {
		tt.Frame()
		rs.mu.Lock()
		defer rs.mu.Unlock()
		if len(rs.tiles) <= 3 {
			return false
		}
		for _, x := range rs.tiles {
			if !x.ready {
				return false
			}
		}
		return true
	})
	rs.keep(3)
	rs.mu.Lock()
	kept := len(rs.tiles)
	rs.mu.Unlock()
	if kept > 3 {
		t.Errorf("keep(3) kept %d tiles", kept)
	}
	s.NewTab()
	s.OpenPath(filepath.Join(root, "rank0.sqlite")) // on the server, on its way
	s.ShowTab(0)
	s.Disconnect()
	time.Sleep(300 * time.Millisecond)
	tt.Frame()
	if s.tr == nil || s.tr.Path != local {
		t.Fatalf("the local tab: %v", s.tr)
	}
	for i := 1; i < 3; i++ {
		if a := s.ctl.tabs[i].app; a.tr != nil || a.loading != "" {
			t.Errorf("tab %d still has %v (loading %q) after disconnecting", i, a.tr != nil, a.loading)
		}
	}
}
