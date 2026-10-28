//go:build js

// Command webui is Tracelight's web client in WebAssembly: the desktop
// app's views (MyGo draws them on a canvas of the page, with WebGL 2, or
// on the CPU where the browser has none), streaming from the server that
// serves the page, wherever it is mounted (its API is relative to the
// page). The page's address follows the tab shown (?path=…#v=…), as the
// JavaScript client's does, and the tabs come back after a reload; the
// settings are kept in the browser.
//
// tracelight serve embeds it (internal/server/webui/app.wasm.gz, built by
// go generate ./internal/server).
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"tracelight/internal/remote"
	"tracelight/internal/theme"
	"tracelight/internal/viewer"
)

var (
	window  = js.Global()
	local   = window.Get("localStorage")
	session = window.Get("sessionStorage")
)

func main() {
	app := viewer.New(nil)
	app.SetWeb(true)
	loadSettings(app)
	href := window.Get("location").Get("href").String()
	link, _ := remote.ParseLink(href)
	// The token: the address's (#token=, kept there), else the tab's.
	tokenInURL := link.Token != ""
	token := link.Token
	if tokenInURL {
		session.Call("setItem", "nsa-token", token)
	} else if v := session.Call("getItem", "nsa-token"); v.Type() == js.TypeString {
		token = v.String()
	}
	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{Title: "Tracelight", Content: ui.View(app.View)})
		app.SetWake(func() { win.Update(func() {}) })
		app.OnTitle(win.SetTitle)
		app.SetSignIn(signIn)
		bridge(app, win)
		app.Connect(href, token, "")
		restoreTabs(app, link.Paths)
		go func() {
			for range time.Tick(400 * time.Millisecond) {
				win.Update(func() { follow(app, tokenInURL, token) })
			}
		}()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// signIn answers a gateway that wants the user to sign in again (its
// session ended): the page reloads, through the gateway's sign-in; once
// a minute at most, so that a gateway that never lets the API through
// says so instead of reloading on and on.
func signIn(base string, check func(string) error, done func(string, error)) {
	now := time.Now().Unix()
	if v := session.Call("getItem", "tl-signin"); v.Type() == js.TypeString {
		if t, _ := strconv.ParseInt(v.String(), 10, 64); now-t < 60 {
			done("", errors.New("the sign-in gateway in front of the server does not let its API through: reload the page to sign in again"))
			return
		}
	}
	session.Call("setItem", "tl-signin", strconv.FormatInt(now, 10))
	window.Get("location").Call("reload")
}

// follow keeps the page's address on the tab shown (its paths, view and
// alignment, as the JavaScript client writes them) and the tabs for a
// reload.
func follow(app *viewer.App, tokenInURL bool, token string) {
	links, cur := app.TabLinks()
	if b, err := json.Marshal(map[string]any{"tabs": links, "cur": cur}); err == nil {
		session.Call("setItem", "tl-tabs", string(b))
	}
	l := links[cur]
	var q []string
	for _, p := range l.Paths {
		q = append(q, "path="+strings.ReplaceAll(url.QueryEscape(p), "%2F", "/"))
	}
	frag := url.Values{}
	if tokenInURL {
		frag.Set("token", token)
	}
	if l.View[1] > l.View[0] {
		frag.Set("v", strconv.FormatFloat(l.View[0], 'f', 0, 64)+","+strconv.FormatFloat(l.View[1], 'f', 0, 64))
	}
	if l.Align != "" && l.Align != "collectives" {
		frag.Set("align", l.Align)
	}
	if len(l.Only) > 0 {
		frag.Set("only", strings.Join(l.Only, ","))
	} else if len(l.Omit) > 0 {
		frag.Set("omit", strings.Join(l.Omit, ","))
	}
	loc := window.Get("location")
	next := loc.Get("pathname").String()
	if len(q) > 0 {
		next += "?" + strings.Join(q, "&")
	}
	if len(frag) > 0 {
		next += "#" + frag.Encode()
	}
	if next != loc.Get("pathname").String()+loc.Get("search").String()+loc.Get("hash").String() {
		window.Get("history").Call("replaceState", nil, "", next)
	}
}

// restoreTabs brings back the tabs kept for the browser tab, if the
// address is the one they were kept with (a reload, not another link).
func restoreTabs(app *viewer.App, paths []string) {
	v := session.Call("getItem", "tl-tabs")
	if v.Type() != js.TypeString {
		return
	}
	var kept struct {
		Tabs []viewer.TabLink `json:"tabs"`
		Cur  int              `json:"cur"`
	}
	if json.Unmarshal([]byte(v.String()), &kept) != nil || len(kept.Tabs) < 2 || kept.Cur < 0 || kept.Cur >= len(kept.Tabs) ||
		!slices.Equal(kept.Tabs[kept.Cur].Paths, paths) {
		return
	}
	app.RestoreTabs(kept.Tabs, kept.Cur)
	_, cur := app.Tabs()
	app.RenameTab(cur, kept.Tabs[kept.Cur].Name) // the tab the address opens
}

// webSettings are what the browser keeps: the appearance, the navigation
// speed and the app's settings (the density, the lanes, ...).
type webSettings struct {
	Theme    string  `json:"theme,omitempty"`
	NavSpeed float64 `json:"navSpeed,omitempty"`
	viewer.Settings
}

func loadSettings(app *viewer.App) {
	var st webSettings
	if v := local.Call("getItem", "tl-app"); v.Type() == js.TypeString {
		json.Unmarshal([]byte(v.String()), &st)
	}
	save := func() {
		if b, err := json.Marshal(st); err == nil {
			local.Call("setItem", "tl-app", string(b))
		}
	}
	mode := theme.ParseMode(st.Theme)
	app.SetThemeMode(mode)
	setTheme(mode)
	app.SetNavSpeed(st.NavSpeed)
	app.SetSettings(st.Settings)
	app.OnSettings(func(v viewer.Settings) { st.Settings = v; save() })
	app.OnNavSpeed = func(v float64) { st.NavSpeed = v; save() }
	app.OnThemeMode = func(m theme.Mode) { st.Theme = string(m); setTheme(m); save() }
}

// setTheme makes MyGo's widgets follow the appearance chosen.
func setTheme(m theme.Mode) {
	switch m {
	case theme.ModeLight:
		mygo.Theme.SetSource(mygo.ThemeLight)
	case theme.ModeDark:
		mygo.Theme.SetSource(mygo.ThemeDark)
	default:
		mygo.Theme.SetSource(mygo.ThemeSystem)
	}
}
