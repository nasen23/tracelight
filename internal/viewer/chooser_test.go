package viewer

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/theme"
)

func TestShouldChoose(t *testing.T) {
	for _, c := range []struct {
		count  int
		bytes  int64
		inputs int
		want   bool
	}{
		{16, 1 << 20, 1, false}, // 16 is not more than 16
		{17, 1 << 20, 1, true},
		{17, 1 << 20, 17, false}, // each named
		{3, 3 << 30, 1, true},    // few, but big
		{1, 9 << 30, 1, false},   // one: nothing to choose
		{40, 1, 2, true},
	} {
		if got := shouldChoose(c.count, c.bytes, c.inputs); got != c.want {
			t.Errorf("shouldChoose(%d, %d, %d) = %v", c.count, c.bytes, c.inputs, got)
		}
	}
	for _, c := range []struct {
		n, k int
		want []int
	}{
		{64, 8, []int{0, 9, 18, 27, 36, 45, 54, 63}}, {5, 8, []int{0, 1, 2, 3, 4}}, {10, 2, []int{0, 9}}, {10, 1, []int{0}}, {3, 3, []int{0, 1, 2}},
	} {
		if got := spreadPick(c.n, c.k); !slices.Equal(got, c.want) {
			t.Errorf("spreadPick(%d, %d) = %v, want %v", c.n, c.k, got, c.want)
		}
	}
}

// A folder of more than theme.ChooseOver traces asks which to open: the
// first ticked; the ticked ones open side by side, or all of them; files
// each named open without asking.
func TestChooseTraces(t *testing.T) {
	dir := jobDir(t, theme.ChooseOver+4)
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	tt.Frame()
	s.OpenPath(dir)
	waitFor(t, tt, "the chooser", func() bool { tt.Frame(); return s.choosing != nil })
	ch := s.choosing
	if len(ch.files) != theme.ChooseOver+4 || s.loading != "" || s.tr != nil {
		t.Fatalf("chooser of %d, loading %q", len(ch.files), s.loading)
	}
	texts := strings.Join(tt.Texts(), " | ")
	for _, want := range []string{"20 traces in job", "8 ticked", "Open 8 ticked", "Open all 20", "Spread 8"} {
		if !strings.Contains(texts, want) {
			t.Errorf("no %q in %s", want, texts)
		}
	}
	if err := tt.Click("Spread 8"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	var ticked []string
	for i, on := range ch.ticked {
		if on {
			ticked = append(ticked, ch.files[i].name)
		}
	}
	if len(ticked) != 8 || ticked[0] != "rank0.sqlite" || ticked[7] != "rank19.sqlite" {
		t.Errorf("spread: %q", ticked)
	}
	if err := tt.Click("Open 8 ticked"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "the ticked ones", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" })
	if ss, ok := s.src.(*sessionSource); !ok || len(ss.parts) != 8 || s.choosing != nil {
		t.Fatalf("opened %T", s.src)
	}
	// Open all, in a new tab.
	s.NewTab()
	s.OpenPath(dir)
	waitFor(t, tt, "the chooser again", func() bool { tt.Frame(); return s.choosing != nil })
	if err := tt.Click("Open all 20"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "all of them", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" })
	if ss, ok := s.src.(*sessionSource); !ok || len(ss.parts) != 20 {
		t.Fatalf("opened %T", s.src)
	}
	// Named each: no asking.
	var named []string
	for r := range 18 {
		named = append(named, filepath.Join(dir, fmt.Sprintf("rank%d.sqlite", r)))
	}
	s.NewTab()
	s.OpenPath(strings.Join(named, " "))
	waitFor(t, tt, "the files named", func() bool { tt.Frame(); return (s.tr != nil && s.loading == "") || s.choosing != nil })
	if s.choosing != nil {
		t.Error("files each named asked which")
	}
	// Cancel leaves the tab empty.
	s.NewTab()
	s.OpenPath(dir)
	waitFor(t, tt, "the chooser once more", func() bool { tt.Frame(); return s.choosing != nil })
	tt.Click("Cancel")
	tt.Frame()
	tt.Frame()
	if s.choosing != nil || !s.Empty() {
		t.Error("Cancel")
	}
}

// From a server: the same, the server listing them, loading none until
// they are chosen.
func TestChooseTracesRemote(t *testing.T) {
	root := jobDir(t, theme.ChooseOver+1)
	url := testServer(t, root, "", 0)
	s := New(nil)
	tt := ui.NewTester(s.View, w, h)
	s.Connect(url, "", root)
	waitFor(t, tt, "the chooser", func() bool { tt.Frame(); return s.choosing != nil })
	if !s.choosing.remote || len(s.choosing.files) != theme.ChooseOver+1 || s.choosing.files[3].size == 0 {
		t.Fatalf("chooser %+v", s.choosing)
	}
	if err := tt.Click("Open 8 ticked"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "the ticked ones", func() bool { tt.Frame(); return s.tr != nil && s.loading == "" })
	if ss, ok := s.src.(*sessionSource); !ok || len(ss.parts) != 8 {
		t.Fatalf("opened %T", s.src)
	}
}
