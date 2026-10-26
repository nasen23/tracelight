package viewer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/torchgen"
	"tracelight/internal/trace"
)

// partsShown are the session's parts with rows laid out, and how many
// rank blocks the overview has.
func partsShown(s *App) []int {
	ss := s.session()
	var out []int
	for _, r := range s.tl.rows {
		if r.kind == rowTrack {
			if pi := ss.trackPart[r.track]; !slices.Contains(out, pi) {
				out = append(out, pi)
			}
		}
	}
	return out
}

func openSession(t *testing.T, path string, n int) (*App, *ui.Tester) {
	t.Helper()
	s := New(nil)
	s.SetAnimations(false)
	s.SetCompact("off")
	tt := ui.NewTester(s.View, 1600, 1000)
	s.OpenLocal(path)
	waitFor(t, tt, "the session", func() bool { ss := s.session(); return ss != nil && len(ss.parts) == n && s.loading == "" })
	tt.Frame()
	return s, tt
}

// A session's traces can be left out and shown again from the sidebar's
// TRACES list: a box each, "only" (or Alt-click) for one alone, Show all.
// The others keep their place; the link says which show.
func TestTracesShown(t *testing.T) {
	dir := jobDir(t, 4)
	s, tt := openSession(t, dir, 4)
	if got := partsShown(s); !slices.Equal(got, []int{0, 1, 2, 3}) {
		t.Fatalf("at first, rows of parts %v", got)
	}
	off2 := s.session().parts[2].offset
	if !tt.HasText("4 of 4 shown") {
		t.Error("the sidebar does not list the traces")
	}
	if err := tt.Click("Show rank1 (node0)"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if got := partsShown(s); !slices.Equal(got, []int{0, 2, 3}) {
		t.Errorf("rank1 left out: rows of parts %v", got)
	}
	if !tt.HasText("3 of 4 shown") || !tt.HasText("Show all") {
		t.Error("the list does not say 3 of 4 shown, with Show all")
	}
	if l, _ := s.Link(); !slices.Equal(l.Omit, []string{"rank1"}) || l.Only != nil {
		t.Errorf("link: only %q omit %q", l.Only, l.Omit)
	}
	if s.session().parts[2].offset != off2 {
		t.Error("leaving a trace out moved the others")
	}
	// Alt-click: that one alone; then again, all.
	if err := tt.ClickWith(ui.Alt, "Show rank3 (node1)"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if got := partsShown(s); !slices.Equal(got, []int{3}) {
		t.Errorf("rank3 alone: rows of parts %v", got)
	}
	if l, _ := s.Link(); !slices.Equal(l.Only, []string{"rank3"}) {
		t.Errorf("link: only %q omit %q", l.Only, l.Omit)
	}
	// The last one shown stays.
	s.SetTraceShown(3, false)
	tt.Frame()
	if got := partsShown(s); !slices.Equal(got, []int{3}) {
		t.Errorf("the last trace shown went: %v", got)
	}
	// Fit shows what is shown.
	s.fit(false)
	if a, b := s.shownExtent(); a != float64(s.session().parts[3].offset) || b <= a {
		t.Errorf("extent of rank3: %v–%v", a, b)
	}
	if err := tt.Click("Show all traces"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if got := partsShown(s); len(got) != 4 {
		t.Errorf("Show all: rows of parts %v", got)
	}
	// "only", then the overview: one block a trace shown.
	if err := tt.Click("Show only rank0 (node0)"); err != nil {
		t.Fatal(err)
	}
	s.SetTraceShown(2, true)
	s.SetCompact("on")
	tt.Frame()
	var blocks []string
	for _, r := range s.rankInfos() {
		blocks = append(blocks, r.label)
	}
	if !slices.Equal(blocks, []string{"rank0", "rank2"}) || len(s.tl.ranks) != 2 {
		t.Errorf("overview blocks %v (%d)", blocks, len(s.tl.ranks))
	}
	// A selection in a trace left out goes; the focus leaves it.
	ss := s.session()
	ref, ok := firstEvent(s, 2)
	if !ok {
		t.Fatal("no event in rank2")
	}
	s.Select(ref)
	s.SetTraceShown(2, false)
	if s.selected != nil || ss.focus != 0 {
		t.Errorf("after leaving rank2 out: selected %v, focus %d", s.selected, ss.focus)
	}
}

// firstEvent is the first event of a part's first track with events.
func firstEvent(s *App, pi int) (ref trace.Ref, ok bool) {
	ss := s.session()
	p := ss.parts[pi]
	for i, tk := range p.tr.Tracks {
		if len(tk.Lanes) > 0 && len(tk.Lanes[0].Events) > 0 {
			ref.Track = int32(p.first + i)
			return ref, true
		}
	}
	return ref, false
}

// A link's #only= / #omit= leaves traces out, by name, as the session
// opens on the server; the ranks of a merged PyTorch file too.
func TestTracesLink(t *testing.T) {
	dir := jobDir(t, 4)
	torchgen.WriteMerged(filepath.Join(dir, "merged.json.gz"), torchgen.Options{Ranks: 4, NoStack: true}, true)
	url := testServer(t, dir, "", 0)
	for _, c := range []struct {
		link string
		n    int
		want []int
	}{
		{"/?path=" + dir + "/rank*.sqlite#only=rank1,rank2", 4, []int{1, 2}},
		{"/?path=" + dir + "/rank*.sqlite#omit=rank0", 4, []int{1, 2, 3}},
		{"/?path=" + dir + "/rank*.sqlite#only=nothing", 4, []int{0, 1, 2, 3}},
		{"/?path=" + dir + "/merged.json.gz#omit=rank1,rank3", 4, []int{0, 2}},
	} {
		s := New(nil)
		s.SetAnimations(false)
		s.SetCompact("off")
		tt := ui.NewTester(s.View, 1600, 1000)
		s.Connect(url+c.link, "", "")
		waitFor(t, tt, c.link, func() bool { ss := s.session(); return ss != nil && len(ss.parts) == c.n && s.loading == "" })
		tt.Frame()
		if got := partsShown(s); !slices.Equal(got, c.want) {
			t.Errorf("%s: rows of parts %v, want %v", c.link, got, c.want)
		}
	}
}

// Some of a folder's traces, picked: in the server's browser, a box each
// and "Open N picked side by side"; on this machine, the Open dialog's
// several files (OpenLocalPaths).
func TestPickTraces(t *testing.T) {
	dir := jobDir(t, 4)
	url := testServer(t, dir, "", 0)
	s := New(nil)
	s.SetAnimations(false)
	tt := ui.NewTester(s.View, 1600, 1000)
	s.Connect(url, "", "")
	waitFor(t, tt, "the server's traces", func() bool { return s.picker != nil && s.picker.shown && len(s.picker.entries) == 4 })
	tt.Frame()
	for _, n := range []string{"Pick rank2.sqlite", "Pick rank0.sqlite"} {
		if err := tt.Click(n); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
	}
	if err := tt.Click("Open 2 picked side by side"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "the picked traces", func() bool { ss := s.session(); return ss != nil && len(ss.parts) == 2 && s.loading == "" })
	if ss := s.session(); ss.parts[0].key != "rank0" || ss.parts[1].key != "rank2" {
		t.Errorf("opened %s and %s", ss.parts[0].key, ss.parts[1].key)
	}

	l := New(nil)
	l.SetAnimations(false)
	tl := ui.NewTester(l.View, 1600, 1000)
	l.OpenLocalPaths([]string{filepath.Join(dir, "rank3.sqlite"), filepath.Join(dir, "rank1.sqlite")})
	waitFor(t, tl, "the files chosen", func() bool { ss := l.session(); return ss != nil && len(ss.parts) == 2 && l.loading == "" })
	if ss := l.session(); ss.parts[0].key != "rank1" || ss.parts[1].key != "rank3" { // in natural order, as a folder
		t.Errorf("opened %s and %s", ss.parts[0].key, ss.parts[1].key)
	}
}
