package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tracelight/internal/theme"
	"tracelight/internal/version"
	"tracelight/internal/viewer"
)

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if s := loadSettingsFrom(path); theme.ParseMode(s.Theme) != theme.ModeSystem {
		t.Errorf("no file: %+v", s)
	}
	if err := saveSettingsTo(path, settings{Theme: "light"}); err != nil {
		t.Fatal(err)
	}
	if s := loadSettingsFrom(path); theme.ParseMode(s.Theme) != theme.ModeLight {
		t.Errorf("saved light, read %+v", s)
	}
	if err := saveSettingsTo(path, settings{Theme: "dark", NavSpeed: 2}); err != nil {
		t.Fatal(err)
	}
	if s := loadSettingsFrom(path); s.NavSpeed != 2 || theme.ParseNavSpeed(s.NavSpeed) != 2 {
		t.Errorf("saved navigation speed 2, read %+v", s)
	}
	if theme.ParseNavSpeed(0) != 1 || theme.ParseNavSpeed(7) != 1 {
		t.Error("an unset or unknown navigation speed is not Normal")
	}
	want := settings{Theme: "light", Settings: viewer.Settings{Servers: []string{"http://gpu-box:8737/"},
		RecentPaths: []string{"/data/job7"}, RecentServerPaths: map[string][]string{"http://gpu-box:8737/": {"/runs/a.sqlite"}}}}
	if err := saveSettingsTo(path, want); err != nil {
		t.Fatal(err)
	}
	if s := loadSettingsFrom(path); s.Servers[0] != "http://gpu-box:8737/" || s.RecentPaths[0] != "/data/job7" ||
		s.RecentServerPaths["http://gpu-box:8737/"][0] != "/runs/a.sqlite" {
		t.Errorf("servers and recent paths did not round-trip: %+v", s)
	}
	os.WriteFile(path, []byte("{broken"), 0o644)
	if s := loadSettingsFrom(path); theme.ParseMode(s.Theme) != theme.ModeSystem {
		t.Errorf("a broken file: %+v", s)
	}
}

// Settings kept before the rename move to the new data directory once.
func TestMigrateSettings(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "nsight-alt", "settings.json")
	path := filepath.Join(dir, "Tracelight", "settings.json")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.MkdirAll(filepath.Dir(path), 0o755)
	saveSettingsTo(old, settings{Theme: "light"})
	migrateSettings(path, old)
	if s := loadSettingsFrom(path); s.Theme != "light" {
		t.Fatalf("not migrated: %+v", s)
	}
	saveSettingsTo(path, settings{Theme: "dark"})
	migrateSettings(path, old)
	if s := loadSettingsFrom(path); s.Theme != "dark" {
		t.Error("migration overwrote newer settings")
	}
}

// The version and names of the packaging are the app's.
func TestPackaging(t *testing.T) {
	data, err := os.ReadFile("mygo.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name, Identifier, Version string
		Macos                     struct{ DmgTitle string }
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "Tracelight" || m.Identifier != "dev.tracelight.viewer" || m.Version != version.Version || m.Macos.DmgTitle != "Tracelight "+version.Version {
		t.Errorf("mygo.json: %+v, version %s", m, version.Version)
	}
}

// The settings file, which can hold gateways' sessions, is the user's only.
func TestSettingsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte("{}"), 0o644) // as older versions left it
	if err := saveSettingsTo(path, settings{Settings: viewer.Settings{ServerCookies: map[string]string{"https://gateway.test/": "s=1"}}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("settings file mode %v, %v", fi.Mode().Perm(), err)
	}
	if s := loadSettingsFrom(path); s.ServerCookies["https://gateway.test/"] != "s=1" {
		t.Errorf("cookies did not round-trip: %+v", s)
	}
}
