package viewer

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// threeTabs opens rank0, rank1 and rank2 of a job, a tab each.
func threeTabs(t *testing.T) (*App, *ui.Tester) {
	t.Helper()
	dir := jobDir(t, 3)
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, w, h)
	for i, f := range []string{"rank0.sqlite", "rank1.sqlite", "rank2.sqlite"} {
		if i > 0 {
			s.NewTab()
		}
		s.OpenPath(filepath.Join(dir, f))
		waitFor(t, tt, f, func() bool { return s.tr != nil && s.loading == "" })
	}
	tt.Frame()
	return s, tt
}

// dragTab presses tab title, moves the pointer to x by small steps, a
// frame each, and lets go there.
func dragTab(t *testing.T, tt *ui.Tester, title string, x float32) {
	t.Helper()
	r, ok := tt.Find(title)
	if !ok {
		t.Fatalf("no tab %q in %q", title, tt.Texts())
	}
	y := r.Y + r.H/2
	x0 := r.X + r.W/2
	tt.Press(x0, y)
	for k := 1; k <= 8; k++ {
		tt.Move(x0+(x-x0)*float32(k)/8, y)
		tt.Advance(16 * time.Millisecond)
	}
	tt.Release(x, y)
	tt.Advance(time.Second)
}

// Dragging a tab along the bar moves it there and shows it; the tab shown
// before stays open where it went; a click still only shows a tab.
func TestDragTab(t *testing.T) {
	s, tt := threeTabs(t)
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank0", "rank1", "rank2"}) {
		t.Fatalf("tabs %q", got)
	}
	// rank0 past rank2's middle: last.
	r2, _ := tt.Find("rank2")
	dragTab(t, tt, "rank0", r2.X+r2.W+40)
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank1", "rank2", "rank0"}) {
		t.Fatalf("after dragging rank0 to the end: %q", got)
	}
	if _, cur := s.Tabs(); cur != 2 || s.inputs == "" || filepath.Base(s.inputs) != "rank0.sqlite" {
		t.Errorf("shown: tab %d, %q", cur, s.inputs)
	}
	// rank2 to the front, just past rank1's middle.
	r1, _ := tt.Find("rank1")
	dragTab(t, tt, "rank2", r1.X+10)
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank2", "rank1", "rank0"}) {
		t.Fatalf("after dragging rank2 to the front: %q", got)
	}
	// A press that hardly moves is a click: it shows the tab, moves none.
	r1, _ = tt.Find("rank1")
	tt.Press(r1.X+r1.W/2, r1.Y+r1.H/2)
	tt.Move(r1.X+r1.W/2+1, r1.Y+r1.H/2)
	tt.Release(r1.X+r1.W/2+1, r1.Y+r1.H/2)
	tt.Advance(time.Second)
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank2", "rank1", "rank0"}) {
		t.Errorf("a click moved tabs: %q", got)
	}
	if _, cur := s.Tabs(); cur != 1 {
		t.Errorf("a click showed tab %d, want 1", cur)
	}
	// Each tab kept its own trace through the moves.
	for i, want := range []string{"rank2", "rank1", "rank0"} {
		s.ShowTab(i)
		tt.Frame()
		if filepath.Base(s.inputs) != want+".sqlite" || s.tr == nil {
			t.Errorf("tab %d shows %q", i, s.inputs)
		}
	}
}

// Ctrl/⌘+Shift+Page Up and Page Down (and Alt+Shift+) move the tab shown.
func TestMoveTabKeys(t *testing.T) {
	s, tt := threeTabs(t) // rank2 shown, last
	// The timeline has the keys (the path box keeps Page Up for its text).
	r := s.tl.size
	tt.ClickAt(r.X+s.labelW()+300, r.Y+r.H-20)
	tt.Frame()
	tt.Key(ui.Cmd|ui.Shift, ui.KeyPageUp)
	tt.Frame()
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank0", "rank2", "rank1"}) {
		t.Fatalf("after Shift+Page Up: %q", got)
	}
	tt.Key(ui.Alt|ui.Shift, ui.KeyPageUp)
	tt.Frame()
	tt.Key(ui.Alt|ui.Shift, ui.KeyPageUp) // at the front already: stays
	tt.Frame()
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank2", "rank0", "rank1"}) {
		t.Fatalf("after two more: %q", got)
	}
	if _, cur := s.Tabs(); cur != 0 || filepath.Base(s.inputs) != "rank2.sqlite" {
		t.Errorf("shown: %d, %q", cur, s.inputs)
	}
	tt.Key(ui.Cmd|ui.Shift, ui.KeyPageDown)
	tt.Frame()
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank0", "rank2", "rank1"}) {
		t.Errorf("after Shift+Page Down: %q", got)
	}
}

func TestTabDropIndex(t *testing.T) {
	lefts, widths := []float64{0, 100, 200, 300}, []float64{98, 98, 98, 98}
	for _, c := range []struct {
		from   int
		middle float64
		want   int
	}{
		{0, 49, 0}, {0, 140, 0}, {0, 160, 1}, {0, 260, 2}, {0, 400, 3}, {3, 340, 3}, {3, 240, 2}, {3, 10, 0}, {2, 249, 2}, {1, -50, 0},
	} {
		if got := tabDropIndex(lefts, widths, c.from, c.middle); got != c.want {
			t.Errorf("tab %d at %v: %d, want %d", c.from, c.middle, got, c.want)
		}
	}
}

// A tab renamed: by a double-click on it, or its menu; Enter keeps the
// name, Esc the one it had, an empty name gives back its paths'; the name
// is the window's title, and is kept with the tab (TabLinks, restored).
func TestRenameTab(t *testing.T) {
	s, tt := threeTabs(t)
	var title string
	s.OnTitle(func(v string) { title = v })
	r, _ := tt.Find("rank1")
	tt.ClickAt(r.X+r.W/2, r.Y+r.H/2)
	tt.ClickAt(r.X+r.W/2, r.Y+r.H/2) // a double click
	tt.Frame()
	if s.ctl.renaming == 0 {
		t.Fatalf("a double click did not edit the name; showing %q", tt.Texts())
	}
	tt.Type("baseline run")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Frame()
	if got := s.TabTitles(); !slices.Equal(got, []string{"rank0", "baseline run", "rank2"}) {
		t.Fatalf("after renaming: %q", got)
	}
	if _, cur := s.Tabs(); cur != 1 || title != "Tracelight — baseline run" {
		t.Errorf("tab %d shown, title %q", cur, title)
	}
	// Its menu, then Esc: the name stays.
	if err := tt.RightClick("baseline run"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Rename tab…"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tt.Type(" ignored")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if got := s.TabTitles()[1]; got != "baseline run" || s.ctl.renaming != 0 {
		t.Errorf("after Esc: %q, editing %v", got, s.ctl.renaming)
	}
	// Kept with the tab, and restored with it.
	links, cur := s.TabLinks()
	if links[1].Name != "baseline run" || links[0].Name != "" {
		t.Fatalf("links %+v", links)
	}
	r2 := New(nil)
	rt := ui.NewTester(r2.View, w, h)
	rt.Frame()
	r2.RestoreSession(roundTrip(t, links), cur, true)
	if got := r2.TabTitles(); !slices.Equal(got, []string{"rank0", "baseline run", "rank2"}) {
		t.Errorf("restored: %q", got)
	}
	// An empty name: its paths' again.
	s.RenameTab(1, "  ")
	if got := s.TabTitles()[1]; got != "rank1" {
		t.Errorf("an empty name: %q", got)
	}
	s.RenameTab(0, strings.Repeat("x", 200))
	if n := len([]rune(s.TabTitles()[0])); n != maxTabName {
		t.Errorf("a long name: %d characters", n)
	}
}
