package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"

	"tracelight/internal/theme"
	"tracelight/internal/viewer"
)

// settings are the user's choices that outlast the app, kept as JSON in
// MyGo's per-user data directory.
type settings struct {
	// Theme is the appearance: "system", "light" or "dark".
	Theme string `json:"theme,omitempty"`
	// NavSpeed multiplies the W/A/S/D rates: one of theme.NavSpeeds'.
	NavSpeed float64 `json:"navSpeed,omitempty"`
	// The servers and paths of late (tokens are not kept).
	viewer.Settings
}

func settingsPath() (string, error) {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "settings.json")
	migrateSettings(path, filepath.Join(filepath.Dir(dir), "nsight-alt", "settings.json"))
	return path, nil
}

// migrateSettings copies the settings of before the rename (kept in the
// data directory of "nsight-alt") to path, if path has none yet.
func migrateSettings(path, old string) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	if data, err := os.ReadFile(old); err == nil {
		os.WriteFile(path, data, 0o600)
	}
}

func loadSettingsFrom(path string) settings {
	var s settings
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &s) // a broken file is as no file
	}
	return s
}

// saveSettingsTo writes s to path, through a temporary file so that a crash
// never leaves half a file, readable by the user only: it can hold the
// sessions of sign-in gateways (Settings.ServerCookies).
func saveSettingsTo(path string, s settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	os.Chmod(tmp, 0o600) // whatever the umask
	return os.Rename(tmp, path)
}

// themeSource is MyGo's appearance for a mode, so that the window's own
// parts (the title bar on macOS) follow the choice too.
func themeSource(m theme.Mode) mygo.ThemeSource {
	switch m {
	case theme.ModeLight:
		return mygo.ThemeLight
	case theme.ModeDark:
		return mygo.ThemeDark
	}
	return mygo.ThemeSystem
}

// setupSettings applies the saved settings to app and the window, and
// keeps the user's next choices. Call it once the app is ready.
func setupSettings(app *viewer.App) {
	path, err := settingsPath()
	var st settings
	if err == nil {
		st = loadSettingsFrom(path)
	}
	mode := theme.ParseMode(st.Theme)
	app.SetThemeMode(mode)
	mygo.Theme.SetSource(themeSource(mode))
	app.SetNavSpeed(st.NavSpeed)
	app.SetSettings(st.Settings)
	app.OnSettings(func(v viewer.Settings) {
		st.Settings = v
		if path != "" {
			saveSettingsTo(path, st)
		}
	})
	app.OnNavSpeed = func(v float64) {
		st.NavSpeed = v
		if path != "" {
			saveSettingsTo(path, st)
		}
	}
	app.OnThemeMode = func(m theme.Mode) {
		mygo.Theme.SetSource(themeSource(m))
		st.Theme = string(m)
		if path != "" {
			saveSettingsTo(path, st)
		}
	}
}
