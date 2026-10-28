package viewer

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"tracelight/internal/remote"
	"tracelight/internal/trace"
)

// Everything the window does is done from the window: open a file, a
// folder of traces (side by side) or a glob by its path, connect to a
// server and open its traces the same way, keep a server's trace for
// offline. This file is that logic; the views of it are in controls.go.
//
// Work that takes time (loading, the network) runs in goroutines, which
// post what they bring back to the App; View applies it at the start of a
// frame, on the UI thread, after the window's Wake asked for one.

type connState uint8

const (
	connNone connState = iota
	connConnecting
	connConnected
	connFailed
	// connSignIn: a gateway in front of the server wants the user to sign
	// in (remote.SignInError); the sign-in window may be open.
	connSignIn
)

// Settings are the choices the window keeps (in settings.json, by main).
type Settings struct {
	// Servers are the servers connected to lately, newest first.
	Servers []string `json:"servers,omitempty"`
	// RecentPaths are local paths opened lately; RecentServerPaths, paths
	// on each server.
	RecentPaths       []string            `json:"recentPaths,omitempty"`
	RecentServerPaths map[string][]string `json:"recentServerPaths,omitempty"`
	// Compact is the density: "on", "off", or "auto" (or ""): on with more
	// than theme.CompactAuto traces open. CompactLanes are the summary
	// lanes it shows (theme.CompactLanes' keys; all if empty).
	Compact      string   `json:"compact,omitempty"`
	CompactLanes []string `json:"compactLanes,omitempty"`
	// CompactOverlaid draws the rows of a kind into one lane, over each
	// other, instead of stacking what runs in parallel.
	CompactOverlaid bool `json:"compactOverlaid,omitempty"`
	// LabelW is the width of the timeline's label gutter, as its edge was
	// dragged to (theme.LabelW if 0).
	LabelW float32 `json:"labelW,omitempty"`
	// ServerCookies are the sessions of sign-in gateways in front of
	// servers (Cookie headers, by server URL), from the sign-in window or
	// pasted; main keeps the settings file readable by the user only.
	ServerCookies map[string]string `json:"serverCookies,omitempty"`
}

const maxRecent = 12

// control is the state that outlasts the trace shown.
type control struct {
	mu     sync.Mutex
	posted []func(*App)
	wake   func()

	opts trace.OpenOptions

	// The server: the client once connected, its URL while connecting.
	client *remote.Client
	server string
	state  connState
	err    error
	root   string
	// gen counts connections and opens: what comes back for an older one
	// is dropped.
	connGen, openGen int

	settings   Settings
	onSettings func(Settings)
	onTitle    func(string)
	browse     func()

	// The path box.
	pathText    string
	pathErr     string
	focusPath   bool
	pathFocused bool
	// dirs caches the entries of directories for the box's suggestions.
	dirs map[string][]string

	// The connect dialog.
	showConnect        bool
	urlText, tokenText string
	cookieText         string

	// signIn opens a window to sign in to a gateway at base (main's, with
	// the system web view); check says whether a Cookie header gets
	// through; done is called once, from any goroutine.
	signIn    func(base string, check func(cookie string) error, done func(cookie string, err error))
	signingIn bool
	// resume: the sign-in is of a session that ended while connected (the
	// client stays, with the new session).
	resume bool
	// pending is what the connection that wants a sign-in was to open.
	pendingToken string
	pending      []string

	// The tabs (tabs.go): every tab's state but the shown one's (in the
	// App), cur the shown one's index; background is set while a tab not
	// shown has the App (App.inTab); menuTabs says the app's menu takes
	// ⌘T and ⌘W (macOS), not the window.
	tabs []*tab
	cur  int
	// web says the app runs in a browser (cmd/webui), connected to the
	// server of its page: no local files, no other server.
	web        bool
	tabIDs     int
	background bool
	menuTabs   bool
}

func newControl() *control {
	return &control{dirs: map[string][]string{}, tabs: []*tab{{id: 1}}, tabIDs: 1}
}

// post hands fn to the UI thread (UI thread only: goroutines use the
// control's, as switching tabs rewrites the App).
func (s *App) post(fn func(*App)) { s.ctl.post(fn) }

// post hands fn to the UI thread, from any goroutine.
func (c *control) post(fn func(*App)) {
	c.mu.Lock()
	c.posted = append(c.posted, fn)
	wake := c.wake
	c.mu.Unlock()
	if wake != nil {
		wake()
	}
}

// drain applies what goroutines posted.
func (s *App) drain() {
	c := s.ctl
	c.mu.Lock()
	fns := c.posted
	c.posted = nil
	c.mu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
}

func (s *App) wake() { s.ctl.wakeUp() }

// wakeUp asks the window for a frame, from any goroutine (bound to the
// control, which outlives the App's state, rewritten by show).
func (c *control) wakeUp() {
	c.mu.Lock()
	w := c.wake
	c.mu.Unlock()
	if w != nil {
		w()
	}
}

// SetWake sets how the window is asked for a frame when something comes
// back from a goroutine (win.Update). Without it (as in tests) nothing is
// applied until the next frame.
func (s *App) SetWake(fn func()) {
	s.ctl.mu.Lock()
	s.ctl.wake = fn
	s.ctl.mu.Unlock()
}

// SetOpenOptions sets how local files are opened.
func (s *App) SetOpenOptions(o trace.OpenOptions) { s.ctl.opts = o }

// SetSettings restores kept settings; OnSettings is told of each change,
// to keep them.
func (s *App) SetSettings(st Settings) { s.ctl.settings = st }

// OnSettings sets the function told of changed settings.
func (s *App) OnSettings(fn func(Settings)) { s.ctl.onSettings = fn }

// OnTitle sets the function told of the window's title.
func (s *App) OnTitle(fn func(string)) { s.ctl.onTitle = fn }

// SetBrowse sets what Open… does without a server: a file dialog.
func (s *App) SetBrowse(fn func()) { s.ctl.browse = fn }

// Browse is Open…: the server's traces when connected, else the file
// dialog (as Ctrl/⌘+O).
func (s *App) Browse() { s.browse() }

func (s *App) settingsChanged() {
	if s.ctl.onSettings != nil {
		s.ctl.onSettings(s.ctl.settings)
	}
}

func (s *App) setTitle(p string) {
	s.title = p
	if s.ctl.onTitle != nil && !s.ctl.background {
		s.ctl.onTitle(Title(p))
	}
}

// pushRecent puts v first in list, once, keeping maxRecent.
func pushRecent(list []string, v string) []string {
	out := []string{v}
	for _, x := range list {
		if x != v && len(out) < maxRecent {
			out = append(out, x)
		}
	}
	return out
}

func (s *App) addRecentPath(input string) {
	c := s.ctl
	if c.state == connConnected {
		if c.settings.RecentServerPaths == nil {
			c.settings.RecentServerPaths = map[string][]string{}
		}
		c.settings.RecentServerPaths[c.server] = pushRecent(c.settings.RecentServerPaths[c.server], input)
	} else {
		c.settings.RecentPaths = pushRecent(c.settings.RecentPaths, input)
	}
	s.settingsChanged()
}

// recentPaths are the paths opened lately where the box opens them.
func (s *App) recentPaths() []string {
	c := s.ctl
	if c.state == connConnected {
		return c.settings.RecentServerPaths[c.server]
	}
	return c.settings.RecentPaths
}

// ---------------------------------------------------------------- servers

// Connected reports the server connected to, if any.
func (s *App) Connected() (string, bool) {
	return s.ctl.server, s.ctl.state == connConnected
}

// Connect connects to the server at rawURL (with token, if it needs one),
// then opens then on it, if not empty, or lists its traces. rawURL may be
// anything copied from a browser (remote.ParseLink): the web page's URL,
// or a link to traces, whose paths (unless then says others) and token
// (unless token is given) it uses.
func (s *App) Connect(rawURL, token, then string) {
	s.connect(rawURL, token, splitPaths(then))
}

func (s *App) connect(rawURL, token string, then []string) {
	c := s.ctl
	c.connGen++
	gen := c.connGen
	c.resume = false
	c.server, c.err, c.urlText = strings.TrimSpace(rawURL), nil, strings.TrimSpace(rawURL)
	link, err := remote.ParseLink(rawURL)
	if err != nil {
		c.state, c.err, c.client = connFailed, err, nil
		return
	}
	if token = strings.TrimSpace(token); token == "" {
		token = link.Token
	}
	if len(then) == 0 {
		then = link.Paths
		s.linkNext = &link
	}
	c.urlText, c.tokenText = link.Base, token
	client, err := remote.New(link.Base, token)
	if err != nil {
		c.state, c.err, c.client = connFailed, err, nil
		return
	}
	c.server = client.String()
	cookie := c.settings.ServerCookies[c.server]
	client.SetCookie(cookie)
	c.state, c.client = connConnecting, nil
	pc := s.ctl // the App is rewritten as tabs switch
	go func() {
		info, err := client.Info()
		pc.post(func(s *App) {
			c := s.ctl
			if gen != c.connGen {
				return
			}
			var se *remote.SignInError
			if errors.As(err, &se) {
				if cookie != "" { // the session kept has expired
					delete(c.settings.ServerCookies, c.server)
					s.settingsChanged()
				}
				c.state, c.err, c.pendingToken, c.pending = connSignIn, err, token, then
				s.SignIn() // at once: the user pasted the address to get in
				return
			}
			if err != nil {
				c.state, c.err = connFailed, err
				c.showConnect = c.web // the page's server: ask for the token there
				return
			}
			c.state, c.client, c.root, c.showConnect = connConnected, client, info.Root, false
			c.tokenText = token
			client.OnSignIn(func(e *remote.SignInError) { pc.post(func(s *App) { s.signedOut(client, e) }) })
			c.settings.Servers = pushRecent(c.settings.Servers, c.server)
			s.settingsChanged()
			s.picker = &picker{url: c.server}
			if len(then) > 0 {
				c.pathText = strings.Join(then, " ")
				s.openRemote(then, c.pathText)
			} else if s.tr == nil || s.remotePath == "" {
				s.ShowRemote()
			}
		})
	}()
}

// SetSignIn sets how the app signs in to a gateway in front of a server
// (main: a window of the system web view, whose cookies it then uses).
func (s *App) SetSignIn(fn func(base string, check func(cookie string) error, done func(cookie string, err error))) {
	s.ctl.signIn = fn
}

// SignIn opens the sign-in window for the server that wants it; once the
// user has signed in, its session is kept for the server and the app
// connects, opening what the connection was to open.
func (s *App) SignIn() {
	c := s.ctl
	if c.state != connSignIn || c.signIn == nil || c.signingIn {
		return
	}
	c.signingIn = true
	base, token := c.server, c.pendingToken
	check := func(cookie string) error {
		cl, err := remote.New(base, token)
		if err != nil {
			return err
		}
		cl.SetCookie(cookie)
		_, err = cl.Info()
		return err
	}
	c.signIn(base, check, func(cookie string, err error) {
		s.post(func(s *App) {
			c := s.ctl
			c.signingIn = false
			if c.server != base || c.state != connSignIn {
				return
			}
			if err != nil {
				c.err = err
				return
			}
			s.UseCookie(cookie)
		})
	})
}

// signedOut follows a request to the server that found the session of the
// gateway in front of it ended: in a browser, the page goes through the
// gateway's sign-in again (the signIn of cmd/webui); here, the sign-in
// window opens, and once signed in, the traces open keep streaming.
func (s *App) signedOut(client *remote.Client, e *remote.SignInError) {
	c := s.ctl
	if c.client != client || c.state != connConnected {
		return
	}
	c.state, c.err, c.pendingToken, c.pending, c.resume = connSignIn, e, c.tokenText, nil, true
	if c.server != "" {
		delete(c.settings.ServerCookies, c.server)
		s.settingsChanged()
	}
	c.showConnect = !c.web
	s.SignIn()
}

// UseCookie keeps a Cookie header (from the sign-in window, or pasted from
// a signed-in browser's request) as the session of the gateway in front of
// the server being connected to, and connects with it.
func (s *App) UseCookie(header string) {
	c := s.ctl
	header = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Cookie:"))
	if header == "" || c.server == "" {
		return
	}
	if c.settings.ServerCookies == nil {
		c.settings.ServerCookies = map[string]string{}
	}
	c.settings.ServerCookies[c.server] = header
	c.cookieText = ""
	s.settingsChanged()
	if c.resume && c.client != nil {
		// Signed in again: the traces open keep their client.
		c.client.SetCookie(header)
		c.state, c.err, c.resume, c.showConnect = connConnected, nil, false, false
		return
	}
	s.connect(c.server, c.pendingToken, c.pending)
}

// ForgetCookie forgets the session kept for a server.
func (s *App) ForgetCookie(server string) {
	if _, ok := s.ctl.settings.ServerCookies[server]; ok {
		delete(s.ctl.settings.ServerCookies, server)
		s.settingsChanged()
	}
}

// Disconnect leaves the server; the traces streamed from it close, in
// every tab, and what was being opened from it is dropped.
func (s *App) Disconnect() {
	c := s.ctl
	c.connGen++
	c.client, c.state, c.err, c.root = nil, connNone, nil, ""
	s.picker = nil
	for i, t := range c.tabs {
		a := &t.app
		if i == c.cur {
			a = s
		}
		c.openGen++
		a.gen = c.openGen
		if a.remotePath != "" || a.streamed() || (a.loading != "" && a.tr == nil) {
			a.tr, a.src, a.loading, a.loadErr, a.remotePath, a.inputs, a.title = nil, nil, "", nil, "", "", ""
		}
	}
	s.setTitle(s.title)
}

// streamed reports whether the trace shown comes from a server.
func (s *App) streamed() bool {
	switch src := s.src.(type) {
	case *remoteSource:
		return true
	case *sessionSource:
		for _, p := range src.parts {
			if _, ok := p.src.(*remoteSource); ok {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- opening

// splitPaths reads the path box: one path, or several absolute ones
// separated by spaces (as the web client's box).
func splitPaths(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 1; i < len(v); i++ {
		if v[i] == '/' && v[i-1] == ' ' {
			if p := strings.TrimSpace(v[start:i]); p != "" {
				out = append(out, p)
			}
			start = i
		}
	}
	if p := strings.TrimSpace(v[start:]); p != "" {
		out = append(out, p)
	}
	return out
}

// OpenPath opens what the path box names: on the server when connected,
// else on this machine. A folder or a glob of several traces opens them
// side by side.
func (s *App) OpenPath(input string) {
	c := s.ctl
	c.pathErr = ""
	paths := splitPaths(input)
	if len(paths) == 0 {
		c.pathErr = "Type the path of a trace or a folder of traces."
		return
	}
	// A server's URL, as copied from the browser (the web page, or a link
	// to traces): connect to it and open what it links to, or, on the
	// server already connected, open it there.
	if len(paths) == 1 && isServerURL(paths[0]) {
		link, err := remote.ParseLink(paths[0])
		if err != nil {
			c.pathErr = err.Error()
			return
		}
		if c.state == connConnected && link.Base == c.server {
			if len(link.Paths) == 0 {
				s.ShowRemote()
				return
			}
			c.pathText = strings.Join(link.Paths, " ")
			s.openRemote(link.Paths, c.pathText)
			return
		}
		token := link.Token
		if token == "" && link.Base == c.server {
			token = c.tokenText
		}
		s.connect(link.Base, token, link.Paths)
		return
	}
	c.pathText = strings.Join(paths, " ")
	if c.state == connConnected {
		s.openRemote(paths, c.pathText)
		return
	}
	for i, p := range paths {
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				paths[i] = filepath.Join(home, p[2:])
			}
		}
	}
	s.openLocal(paths, c.pathText)
}

// isServerURL reports whether v is a server's URL rather than a path.
func isServerURL(v string) bool {
	l := strings.ToLower(v)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// OpenLocal opens a file on this machine (from the command line, a drop,
// the file manager or the Open dialog): in the tab shown if it is empty,
// else in a new tab; a tab that has it already is shown instead.
func (s *App) OpenLocal(path string) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	c := s.ctl
	for i, t := range c.tabs {
		a := &t.app
		if i == c.cur {
			a = s
		}
		if a.inputs == path && (a.tr != nil || a.loading != "") && a.loadErr == nil {
			s.ShowTab(i) // MyGo delivers the command line's file too
			return
		}
	}
	if !s.empty() && !s.NewTab() {
		c.pathErr = "Too many tabs: close one to open " + filepath.Base(path)
		return
	}
	s.ctl.pathText = path
	s.openLocal([]string{path}, path)
}

// OpenLocalPaths opens files on this machine (the Open dialog's, several
// chosen): one as OpenLocal does, several side by side, in the tab shown
// if it is empty, else in a new tab.
func (s *App) OpenLocalPaths(paths []string) {
	if len(paths) == 1 {
		s.OpenLocal(paths[0])
		return
	}
	for i, p := range paths {
		if abs, err := filepath.Abs(p); err == nil {
			paths[i] = abs
		}
	}
	if !s.empty() && !s.NewTab() {
		s.ctl.pathErr = "Too many tabs: close one to open these traces"
		return
	}
	s.ctl.pathText = strings.Join(paths, " ")
	s.openLocal(paths, s.ctl.pathText)
}

// expandLocal turns paths into the trace files they name: files,
// folders, globs and brace patterns (rank{0..7}: trace.ExpandBraces).
func (s *App) expandLocal(paths []string) ([]string, error) {
	var braced []string
	for _, p := range paths {
		ps, err := trace.ExpandBraces(p, nil)
		if err != nil {
			return nil, err
		}
		braced = append(braced, ps...)
	}
	paths = braced // several, if braces made them: sorted and deduplicated below
	var files []string
	for _, p := range paths {
		switch {
		case strings.ContainsAny(p, "*?["):
			m, err := filepath.Glob(p)
			if err != nil {
				return nil, err
			}
			var hits []string
			for _, f := range m {
				if fi, err := os.Stat(f); err == nil && !fi.IsDir() && trace.Detect(f) != "" {
					hits = append(hits, f)
				}
			}
			if len(hits) == 0 {
				return nil, fmt.Errorf("%s: no trace files match", p)
			}
			files = append(files, s.ctl.opts.Dedupe(hits)...)
		default:
			if _, _, ok := trace.SplitMember(p); ok && trace.Detect(p) != "" {
				files = append(files, p) // a rank of a merged file
				continue
			}
			fi, err := os.Stat(p)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, fmt.Errorf("%s: no such file or folder", p)
				}
				return nil, err
			}
			if fi.IsDir() {
				in, err := s.ctl.opts.DirTraces(p)
				if err != nil {
					return nil, err
				}
				if len(in) == 0 {
					return nil, fmt.Errorf("%s: no traces in this folder", p)
				}
				files = append(files, in...)
			} else {
				files = append(files, p)
			}
		}
	}
	if len(paths) > 1 || len(files) > 1 {
		trace.SortTraces(files)
		files = slices.Compact(files)
	}
	return files, nil
}

// beginOpen starts opening what input names, and returns its generation.
func (s *App) beginOpen(input, shown string) int {
	c := s.ctl
	c.openGen++
	s.gen = c.openGen
	s.inputs = input
	s.remotePath = ""
	s.SetLoading(shown, nil)
	if s.picker != nil {
		s.picker.shown = false
	}
	s.setTitle(shown)
	return s.gen
}

// failOpen says why input could not be opened, next to the path box.
func (s *App) failOpen(input string, err error) {
	s.ctl.pathErr = err.Error()
	s.ctl.focusPath = true
	s.SetLoading(input, err)
}

func (s *App) openLocal(paths []string, input string) {
	files, err := s.expandLocal(paths)
	if err != nil {
		s.ctl.pathErr = err.Error()
		s.ctl.focusPath = true
		if s.tr == nil {
			s.SetLoading(input, err)
		}
		return
	}
	gen, id := s.beginOpen(input, input), s.tabID
	opts := s.ctl.opts
	items := localItems(files)
	s.loadItems = items
	pc := s.ctl // the App is rewritten as tabs switch
	loading := make(chan struct{})
	go pc.redrawWhile(loading)
	go func() {
		defer close(loading)
		// A file of several ranks merged opens as a trace a rank
		// (trace.Members reads it: here, not on the UI's thread). Its
		// line on the loading screen is that read's; the ranks then open
		// from what it read, at once.
		var members []string
		var progs []*trace.Progress
		for i, f := range files {
			ms := trace.MembersProgress(f, items[i].prog)
			members = append(members, ms...)
			for range ms {
				if len(ms) == 1 {
					progs = append(progs, items[i].prog)
				} else {
					progs = append(progs, nil)
				}
			}
		}
		if len(members) == 1 {
			opts.Progress = progs[0]
			tr, err := trace.OpenWith(members[0], opts)
			pc.postTab(id, func(s *App) {
				if gen != s.gen {
					return
				}
				if err != nil {
					s.failOpen(input, err)
					return
				}
				s.SetTrace(tr)
				s.addRecentPath(input)
			})
			return
		}
		openParts(pc, id, gen, members, progs, input, opts)
	}()
}

// openParts opens files as a session, from a goroutine, in the tab of id,
// saying in progs (one a file, or nil) how far each has got.
func openParts(pc *control, id, gen int, files []string, progs []*trace.Progress, input string, opts trace.OpenOptions) {
	{
		trs := make([]*trace.Trace, len(files))
		errs := make([]error, len(files))
		parallel(len(files), 4, func(i int) {
			o := opts
			o.Progress = progs[i]
			trs[i], errs[i] = trace.OpenWith(files[i], o)
		})
		var parts []*part
		var failed []string
		for i, tr := range trs {
			if errs[i] != nil {
				failed = append(failed, filepath.Base(files[i])+": "+errs[i].Error())
				continue
			}
			parts = append(parts, &part{src: newLocalSource(tr), tr: tr, label: partLabel(files[i], tr.Host), key: traceBase(files[i]),
				utc: tr.SessionStartUTC, mono: tr.SessionStartMono, host: tr.Host, start: tr.Start, coll: collOf(tr.Collectives())})
		}
		pc.postTab(id, func(s *App) {
			if gen != s.gen {
				return
			}
			if len(parts) == 0 {
				s.failOpen(input, errors.New(strings.Join(failed, "; ")))
				return
			}
			ss, sk := newSession(parts, input)
			sk.Warnings = append(sk.Warnings, failed...)
			s.show(sk, ss, 0)
			s.addRecentPath(input)
		})
	}
}

// partLabel names a trace of a session: its file's name, and its host.
func partLabel(path, host string) string {
	l := traceBase(path)
	if host != "" {
		l += " (" + host + ")"
	}
	return l
}

// parallel runs fn(0..n-1), at most k at once.
func parallel(n, k int, fn func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, k)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}

// openRemote opens what paths name on the server: the metadata first (the
// timeline lays out its rows at once), then tiles as they show.
func (s *App) openRemote(paths []string, input string) {
	c := s.ctl
	client := c.client
	shown := strings.TrimSuffix(c.server, "/") + " " + input
	gen, id := s.beginOpen(input, shown), s.tabID
	lk := s.linkNext
	s.linkNext = nil
	wake := c.wakeUp
	pc := s.ctl // the App is rewritten as tabs switch
	// While the server lists what the paths name, a line a path (a merged
	// file's shows its read); then a line a trace, as each loads there.
	inputs := make([]loadItem, len(paths))
	for i, p := range paths {
		inputs[i] = loadItem{name: loadName(p), prog: &trace.Progress{}, listing: true}
	}
	s.loadItems = inputs
	listed, loading := make(chan struct{}), make(chan struct{})
	go pollProgress(client, paths, inputs, listed)
	go pc.redrawWhile(loading)
	go func() {
		defer close(loading)
		list, warnings, err := client.Session(paths)
		close(listed)
		if err != nil {
			pc.postTab(id, func(s *App) {
				if gen == s.gen {
					s.failOpen(shown, err)
				}
			})
			return
		}
		items := make([]loadItem, len(list))
		tracePaths := make([]string, len(list))
		for i, t := range list {
			items[i] = loadItem{name: t.Name, size: t.Size, prog: &trace.Progress{}, received: &atomic.Bool{}}
			tracePaths[i] = t.Path
		}
		pc.postTab(id, func(s *App) {
			if gen == s.gen {
				s.loadItems = items
			}
		})
		loaded := make(chan struct{})
		go pollProgress(client, tracePaths, items, loaded)
		metas := make([]*remote.Meta, len(list))
		errs := make([]error, len(list))
		parallel(len(list), 8, func(i int) {
			metas[i], errs[i] = client.Meta(list[i].Path)
			items[i].prog.Finish(errs[i])
		})
		close(loaded)
		pc.postTab(id, func(s *App) {
			c := s.ctl
			if gen != s.gen {
				return
			}
			if len(list) == 1 {
				if errs[0] != nil {
					s.failOpen(shown, errs[0])
					return
				}
				m := metas[0]
				sk := m.Skeleton(c.server + strings.TrimPrefix(list[0].Path, "/"))
				s.show(sk, newRemoteSource(client, nil, list[0].Path, m.Duration, wake), m.Size)
				s.remotePath = list[0].Path
				s.applyLink(lk)
				s.addRecentPath(input)
				return
			}
			var parts []*part
			var failed []string
			batch := newTileBatcher(client, wake) // one for the session's traces
			for i, m := range metas {
				if errs[i] != nil {
					failed = append(failed, list[i].Name+": "+errs[i].Error())
					continue
				}

				sk := m.Skeleton(list[i].Path)
				parts = append(parts, &part{src: newRemoteSource(client, batch, list[i].Path, m.Duration, wake), tr: sk,
					label: partLabel(list[i].Name, m.Host), key: traceBase(list[i].Path), utc: m.SessionStart, mono: m.SessionMono, host: m.Host, start: m.Start, coll: collOf(m.Collectives)})
			}
			if len(parts) == 0 {
				s.failOpen(shown, errors.New(strings.Join(failed, "; ")))
				return
			}
			ss, sk := newSession(parts, shown)
			sk.Warnings = append(append(sk.Warnings, warnings...), failed...)
			s.show(sk, ss, 0)
			s.applyLink(lk)
			s.addRecentPath(input)
		})
	}()
}

// DownloadOffline fetches the trace streamed from the server whole (a
// snapshot), to show it from memory, without the server.
func (s *App) DownloadOffline() {
	c := s.ctl
	if c.client == nil || s.remotePath == "" || s.downloading {
		return
	}
	s.downloading = true
	client, path, gen, id := c.client, s.remotePath, s.gen, s.tabID
	pc := s.ctl // the App is rewritten as tabs switch
	go func() {
		tr, err := client.Open(path)
		pc.postTab(id, func(s *App) {
			c := s.ctl
			s.downloading = false
			if gen != s.gen {
				return
			}
			if err != nil {
				c.pathErr = "Download: " + err.Error()
				return
			}
			view, scroll := s.tl.cur, s.tl.scrollY
			s.SetTrace(tr)
			s.remotePath = ""
			s.tl.cur, s.tl.tgt, s.tl.fitted, s.tl.scrollY = view, view, true, scroll
		})
	}()
}

// streamStatus says how the tiles are coming, for the perf overlay.
func (s *App) streamStatus() string {
	if rs, ok := s.src.(*remoteSource); ok {
		level, complete, cached := rs.status()
		state := "complete"
		if !complete {
			state = "coarser tiles shown while it loads"
		}
		return fmt.Sprintf("tiles: level %d %s · %d cached · %d loading", level, state, cached, rs.loading())
	}
	if n := s.src.loading(); n > 0 {
		return fmt.Sprintf("%d requests loading", n)
	}
	return ""
}

// ---------------------------------------------------------------- the path box

// suggestions are what the path box offers for what is typed: the recent
// paths, then the entries of the folder typed (local, or on the server
// under its root).
func (s *App) suggestions() []string {
	c := s.ctl
	out := slices.Clone(s.recentPaths())
	text := c.pathText
	if !strings.HasPrefix(text, "/") || strings.Contains(text, " /") {
		return out
	}
	dir := text
	if !strings.HasSuffix(dir, "/") {
		dir = filepath.Dir(dir)
	}
	dir = strings.TrimSuffix(dir, "/") + "/"
	key := dir
	if c.state == connConnected {
		key = c.server + " " + dir
	}
	entries, ok := c.dirs[key]
	if !ok {
		c.dirs[key] = nil // asked
		if len(c.dirs) > 64 {
			clear(c.dirs)
		}
		if c.state == connConnected {
			s.listServerDir(key, dir)
		} else {
			entries = listLocalDir(dir)
			c.dirs[key] = entries
		}
	}
	for _, e := range entries {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// listLocalDir is the folders and traces in dir, as paths.
func listLocalDir(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		p := dir + name
		if e.IsDir() || (e.Type()&os.ModeSymlink != 0 && isDir(p)) {
			out = append(out, p+"/")
		} else if ext := strings.ToLower(filepath.Ext(name)); ext == ".sqlite" || ext == ".nsys-rep" || ext == ".qdrep" || trace.IsTorchName(name) {
			out = append(out, p)
		}
		if len(out) >= 200 {
			break
		}
	}
	slices.SortFunc(out, trace.NaturalCompare)
	return out
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// listServerDir fetches the entries of dir on the server, if it is under
// the server's root, for the suggestions.
func (s *App) listServerDir(key, dir string) {
	c := s.ctl
	root := strings.TrimSuffix(c.root, "/") + "/"
	if c.client == nil || c.root == "" || !strings.HasPrefix(dir, root) && dir+"/" != root {
		return
	}
	rel := strings.TrimSuffix(strings.TrimPrefix(dir, root), "/")
	client := c.client
	pc := s.ctl // the App is rewritten as tabs switch
	go func() {
		_, entries, err := client.List(rel)
		if err != nil {
			return
		}
		var out []string
		for _, e := range entries {
			p := root + e.Path
			if e.Type == "dir" {
				p += "/"
			}
			out = append(out, p)
		}
		pc.post(func(s *App) { s.ctl.dirs[key] = out })
	}()
}

// hostOf is the host of a server's URL, for short labels.
func hostOf(server string) string {
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		return u.Host
	}
	return server
}

// StreamStats says how the last frame went, for benchmarks: the blocks it
// drew, whether every tile of its level had come (always, from memory), and
// the requests in flight.
func (s *App) StreamStats() (blocks int, complete bool, loading int) {
	if s.src == nil {
		return 0, false, 0
	}
	complete = true
	switch src := s.src.(type) {
	case *remoteSource:
		_, complete, _ = src.status()
	case *sessionSource:
		for _, p := range src.parts {
			if rs, ok := p.src.(*remoteSource); ok {
				if _, done, _ := rs.status(); !done {
					complete = false
				}
			}
		}
	}
	return s.perf.blocks, complete, s.src.loading()
}

// Loading reports whether a trace is being opened.
func (s *App) Loading() bool { return s.loading != "" && s.loadErr == nil }

// StreamSlots counts the last frame's tiles over every trace streamed from
// a server: there, stood in for by another level's, or missing.
func (s *App) StreamSlots() (exact, coarse, blank int) {
	add := func(src source) {
		if rs, ok := src.(*remoteSource); ok {
			rs.mu.Lock()
			exact, coarse, blank = exact+rs.slots[0], coarse+rs.slots[1], blank+rs.slots[2]
			rs.mu.Unlock()
		}
	}
	switch src := s.src.(type) {
	case *sessionSource:
		for _, p := range src.parts {
			add(p.src)
		}
	default:
		add(src)
	}
	return
}

// prepareStream tells the sources streamed from a server, before a frame,
// what to fetch: the lanes of the tiles (in the rank overview, its lanes
// and the GPU rows its busy strips need; every row else), and, in a
// session, which traces have rows on screen (the others fetch nothing).
func (s *App) prepareStream() {
	lanes := ""
	if s.compactOn() {
		set := append(slices.Clone(s.lanesOn()), "gpu")
		slices.Sort(set)
		lanes = strings.Join(slices.Compact(set), ",")
	}
	setLanes := func(src source) {
		if rs, ok := src.(*remoteSource); ok {
			rs.setLanes(lanes)
		}
	}
	ss, ok := s.src.(*sessionSource)
	if !ok {
		setLanes(s.src)
		return
	}
	for _, p := range ss.parts {
		setLanes(p.src)
	}
	tl := &s.tl
	vis := make([]bool, len(ss.parts))
	top, bottom := tl.scrollY, tl.scrollY+tl.size.H-rulerH
	for _, r := range tl.rows {
		if r.y+r.h < top || r.y > bottom {
			continue
		}
		switch r.kind {
		case rowTrack:
			vis[ss.trackPart[r.track]] = true
		case rowStrip, rowSummary:
			if rk := s.rankInfos(); r.rank < len(rk) && rk[r.rank].part < len(vis) {
				vis[rk[r.rank].part] = true
			}
		}
	}
	ss.visible = vis
}

// applyLink shows what a link said beyond its traces: its alignment and
// its view (#align=, #v=).
func (s *App) applyLink(l *remote.Link) {
	if l == nil {
		return
	}
	if l.Align != "" {
		s.SetAlign(l.Align)
	}
	if l.View[1] > l.View[0] {
		v := l.View
		s.tl.want = &v
	}
	s.applyTraceFilter(l.Only, l.Omit)
}

// Link is what the tab shown shows, as a link says it: its paths (as
// typed), its view, the traces' alignment and those left out; false for
// nothing shown.
func (s *App) Link() (TabLink, bool) {
	if s.tr == nil || s.loading != "" {
		return TabLink{}, false
	}
	l := TabLink{Paths: splitInputs(s.inputs)}
	if a, b, shown := s.Shown(); shown {
		l.View = [2]float64{a, b}
	}
	if ss := s.session(); ss != nil {
		l.Align = ss.alignMode()
		switch param, names := traceFilter(ss.traceKeys(), ss.off); param {
		case "only":
			l.Only = names
		case "omit":
			l.Omit = names
		}
	}
	return l, true
}

// SetWeb says the app runs in a browser, connected to the server of its
// page (cmd/webui): it opens no local files and no other server, and has
// no Download for offline.
func (s *App) SetWeb(on bool) { s.ctl.web = on }
