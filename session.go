package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"

	"tracelight/internal/trace"
	"tracelight/internal/viewer"
)

// The desktop app reopens its tabs at its next start as they were when it
// closed: each tab's paths (files here, or on a server: its URL), the time
// window it showed, the traces' alignment and those left out, in order,
// the one shown showing (and opened at once, the others as they are first
// shown). They are kept in session.json, beside settings.json in MyGo's
// per-user data directory, readable by the user only: written when they
// change (looked at every saveEvery) and as the window closes. No token is
// kept, as in settings.json. Turned off by the sidebar's "Reopen these
// tabs" (which deletes the file), for one start by --fresh, and by
// TRACELIGHT_REOPEN=off.

// session is the tabs as they were.
type session struct {
	Tabs []viewer.TabLink `json:"tabs"`
	Cur  int              `json:"cur"`
}

func sessionPath() (string, error) {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "session.json"), nil
}

// loadSessionFrom reads the tabs kept at path; false if there are none (no
// file, a broken one, no tab with paths).
func loadSessionFrom(path string) (session, bool) {
	var s session
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &s) != nil {
		return session{}, false
	}
	for _, t := range s.Tabs {
		if len(t.Paths) > 0 {
			return s, true
		}
	}
	return session{}, false
}

// encodeSession is the file's content for the tabs of app.
func encodeSession(app *viewer.App) []byte {
	links, cur := app.TabLinks()
	data, _ := json.MarshalIndent(session{Tabs: links, Cur: cur}, "", "  ")
	return data
}

// reopenWanted reports whether this start reopens the last tabs.
func reopenWanted(st settings, fresh bool) bool {
	return !fresh && !st.NoReopen && trace.Getenv("REOPEN") != "off"
}

// sessionKeeper writes the tabs of app to path when they change.
type sessionKeeper struct {
	path string
	app  *viewer.App
	last []byte
}

// keep writes the tabs if they changed since last written, or deletes the
// file if the user turned reopening off. It runs on the UI's thread.
func (k *sessionKeeper) keep() {
	if k.path == "" {
		return
	}
	if !k.app.ReopenTabs() {
		if k.last != nil || fileExists(k.path) {
			os.Remove(k.path)
			k.last = nil
		}
		return
	}
	data := encodeSession(k.app)
	if bytes.Equal(data, k.last) {
		return
	}
	if writeFileAtomic(k.path, data) == nil {
		k.last = data
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
