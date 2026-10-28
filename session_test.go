package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/viewer"
)

// The tabs kept: read back as written; a broken file, or one without a tab
// of paths, is as none; the file is the user's alone.
func TestSessionFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if _, ok := loadSessionFrom(path); ok {
		t.Error("no file: a session")
	}
	links := []viewer.TabLink{
		{Paths: []string{filepath.Join(dir, "runs", "job", "rank0.sqlite")}, View: [2]float64{1e6, 2e6}},
		{Server: "http://gpu-server.example.com:8737/", Paths: []string{"/data/job"}, Align: "start", Omit: []string{"rank1"}},
	}
	app := viewer.New(nil)
	tt := ui.NewTester(app.View, 800, 600)
	tt.Frame()
	app.RestoreSession(links, 1, false) // kept, not opened
	k := &sessionKeeper{path: path, app: app}
	k.keep()
	s, ok := loadSessionFrom(path)
	if !ok || len(s.Tabs) != 3 || s.Cur != 2 || s.Tabs[0].View != links[0].View || s.Tabs[1].Server != links[1].Server || s.Tabs[1].Omit[0] != "rank1" {
		t.Fatalf("read back %+v, %v", s, ok)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", fi.Mode(), err)
	}
	// Written again only when the tabs change.
	before, _ := os.Stat(path)
	os.Chtimes(path, before.ModTime().Add(-3600e9), before.ModTime().Add(-3600e9))
	k.keep()
	if after, _ := os.Stat(path); !after.ModTime().Before(before.ModTime()) {
		t.Error("written again with nothing changed")
	}
	// Turned off: the file goes.
	app.SetSettings(viewer.Settings{NoReopen: true})
	k.keep()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("reopening off, the file stays: %v", err)
	}
	for _, bad := range []string{"{", `{"tabs":[{"paths":[]}],"cur":0}`, `{"tabs":[]}`} {
		os.WriteFile(path, []byte(bad), 0o600)
		if _, ok := loadSessionFrom(path); ok {
			t.Errorf("%q: a session", bad)
		}
	}
}

func TestReopenWanted(t *testing.T) {
	if !reopenWanted(settings{}, false) {
		t.Error("by default, tabs reopen")
	}
	if reopenWanted(settings{}, true) {
		t.Error("--fresh")
	}
	if reopenWanted(settings{Settings: viewer.Settings{NoReopen: true}}, false) {
		t.Error("turned off in the sidebar")
	}
	t.Setenv("TRACELIGHT_REOPEN", "off")
	if reopenWanted(settings{}, false) {
		t.Error("TRACELIGHT_REOPEN=off")
	}
}
