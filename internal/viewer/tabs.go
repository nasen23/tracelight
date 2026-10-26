package viewer

import (
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"tracelight/internal/remote"
	"tracelight/internal/theme"
)

// Tabs: each keeps a timeline open (a trace, or a session of several) with
// its view, selection, range, rows and source, switched between without
// loading again; a tab not shown draws and fetches nothing. The tab shown
// has its state in the App; control.tabs keeps the others' (a copy of the
// App each, but for what all tabs share: the control, the appearance, the
// sizes of the panes). What an open brings back lands in the tab it was
// asked for in (postTab), shown or not. As the web client's, with the same
// names (tabTitle).
//
// Keys: Ctrl/⌘+T a new tab, Ctrl/⌘+W closes the tab, Ctrl+Tab and
// Ctrl+Shift+Tab (or Ctrl/⌘+Shift+] and [) the next and the one before,
// Ctrl/⌘+1…8 the nth, Ctrl/⌘+9 the last; the web client's Alt+T, Alt+W,
// Alt+1…9, Alt+] and Alt+[ too. Ctrl/⌘+Enter in the path box opens there in
// a new tab; files from the file manager, a drop or the Open dialog open in
// a new tab unless this one is empty.

// tab is a tab's state while it is not shown.
type tab struct {
	id  int
	app App
	// The path box's text and error, which the control holds for the tab
	// shown.
	pathText, pathErr string
}

// keepGlobals takes what every tab shares from o.
func (s *App) keepGlobals(o *App) {
	s.ctl, s.sidebarW, s.timelinePc = o.ctl, o.sidebarW, o.timelinePc
	s.noAnim, s.showPerf, s.mode, s.OnThemeMode = o.noAnim, o.showPerf, o.mode, o.OnThemeMode
	s.navSpeed, s.OnNavSpeed, s.picker, s.mods = o.navSpeed, o.OnNavSpeed, o.picker, o.mods
}

// adopt makes the App tab t's state, keeping what tabs share.
func (s *App) adopt(t *tab) {
	g := *s
	*s = t.app
	s.keepGlobals(&g)
	s.ctl.pathText, s.ctl.pathErr = t.pathText, t.pathErr
}

// hide lets go of most of the tab shown's tiles, as another is shown
// (theme.HiddenTabTiles, shared among its traces).
func (s *App) hide() {
	var srcs []*remoteSource
	switch src := s.src.(type) {
	case *remoteSource:
		srcs = append(srcs, src)
	case *sessionSource:
		for _, p := range src.parts {
			if rs, ok := p.src.(*remoteSource); ok {
				srcs = append(srcs, rs)
			}
		}
	}
	for _, rs := range srcs {
		rs.keep(max(theme.HiddenTabTiles/len(srcs), 8))
	}
}

// stash keeps the App's state in tab t.
func (s *App) stash(t *tab) {
	t.app = *s
	t.pathText, t.pathErr = s.ctl.pathText, s.ctl.pathErr
}

func (c *control) tabByID(id int) *tab {
	for _, t := range c.tabs {
		if t.id == id {
			return t
		}
	}
	return nil
}

// postTab hands fn to the UI thread for the tab of id, from any goroutine:
// the App has that tab's state while fn runs, shown or not; nothing if the
// tab was closed.
func (c *control) postTab(id int, fn func(*App)) {
	c.post(func(s *App) { s.inTab(id, fn) })
}

// inTab runs fn with the App holding the state of the tab of id.
func (s *App) inTab(id int, fn func(*App)) {
	c := s.ctl
	if s.tabID == id {
		fn(s)
		return
	}
	t := c.tabByID(id)
	if t == nil {
		return
	}
	shown := c.tabs[c.cur]
	focus := c.focusPath
	s.stash(shown)
	s.adopt(t)
	c.background = true
	fn(s)
	c.background = false
	s.stash(t)
	s.adopt(shown)
	c.focusPath = focus // the path box of the tab shown
}

// TabTitles are the tabs' names, in order.
func (s *App) TabTitles() []string { return s.tabTitles() }

// Empty reports whether the tab shown shows nothing, nor is opening.
func (s *App) Empty() bool { return s.empty() }

// tabTitles are the tabs' names, in order.
func (s *App) tabTitles() []string {
	out := make([]string, len(s.ctl.tabs))
	for i, t := range s.ctl.tabs {
		in := t.app.inputs
		if i == s.ctl.cur {
			in = s.inputs
		}
		out[i] = tabTitle(splitInputs(in))
	}
	return out
}

// splitInputs splits what a tab was opened with into its paths.
func splitInputs(in string) []string {
	if strings.TrimSpace(in) == "" {
		return nil
	}
	return splitPaths(in)
}

// NewTab opens an empty tab after the one shown, and shows it, the path box
// focused; false if theme.MaxTabs are open.
func (s *App) NewTab() bool {
	c := s.ctl
	if len(c.tabs) >= theme.MaxTabs {
		return false
	}
	s.hide()
	s.stash(c.tabs[c.cur])
	c.tabIDs++
	t := &tab{id: c.tabIDs, app: App{hidden: map[int]bool{}, collapsed: map[string]bool{}, tabID: c.tabIDs}}
	c.tabs = append(c.tabs[:c.cur+1], append([]*tab{t}, c.tabs[c.cur+1:]...)...)
	c.cur++
	s.enter(t)
	c.focusPath = true
	return true
}

// ShowTab shows the ith tab.
func (s *App) ShowTab(i int) {
	c := s.ctl
	if i < 0 || i >= len(c.tabs) || i == c.cur {
		return
	}
	s.hide()
	s.stash(c.tabs[c.cur])
	c.cur = i
	s.enter(c.tabs[i])
}

// enter shows tab t (the current one), whose state the App takes; a tab
// restored opens now.
func (s *App) enter(t *tab) {
	s.nav.stop()
	s.adopt(t)
	s.tl.dirty = true
	s.tl.dragging, s.tl.hovering = false, false
	s.setTitle(s.title)
	if l := s.lazy; l != nil {
		s.lazy = nil
		s.linkNext = l
		s.ctl.pathText = strings.Join(l.Paths, " ")
		s.OpenPath(s.ctl.pathText)
	}
}

// TabLink is a tab as a link says it: its paths, view and alignment.
type TabLink struct {
	Paths []string   `json:"paths"`
	View  [2]float64 `json:"v,omitempty"`
	Align string     `json:"align,omitempty"`
	// Only or Omit name the traces of a session shown, or left out.
	Only []string `json:"only,omitempty"`
	Omit []string `json:"omit,omitempty"`
}

// TabLinks are the tabs as links (an empty tab's without paths), and the
// index of the one shown.
func (s *App) TabLinks() ([]TabLink, int) {
	c := s.ctl
	out := make([]TabLink, len(c.tabs))
	for i, t := range c.tabs {
		a := &t.app
		if i == c.cur {
			a = s
		}
		if a.lazy != nil {
			out[i] = TabLink{Paths: a.lazy.Paths, View: a.lazy.View, Align: a.lazy.Align, Only: a.lazy.Only, Omit: a.lazy.Omit}
			continue
		}
		if l, ok := a.Link(); ok {
			out[i] = l
		} else {
			out[i] = TabLink{Paths: splitInputs(a.inputs)}
		}
	}
	return out, c.cur
}

// RestoreTabs adds tabs for links, each opened when first shown: those
// before index at go before the tab shown, the others after it (at is
// where the tab shown was among them).
func (s *App) RestoreTabs(links []TabLink, at int) {
	c := s.ctl
	var before, after []*tab
	for i, l := range links {
		if i == at || len(c.tabs)+len(before)+len(after) >= theme.MaxTabs {
			continue
		}
		c.tabIDs++
		t := &tab{id: c.tabIDs, app: App{hidden: map[int]bool{}, collapsed: map[string]bool{}, tabID: c.tabIDs}}
		if len(l.Paths) > 0 {
			t.app.inputs = strings.Join(l.Paths, " ")
			t.app.lazy = &remote.Link{Paths: l.Paths, View: l.View, Align: l.Align, Only: l.Only, Omit: l.Omit}
		}
		if i < at {
			before = append(before, t)
		} else {
			after = append(after, t)
		}
	}
	tabs := append(append(append([]*tab{}, c.tabs[:c.cur]...), before...), c.tabs[c.cur])
	c.tabs = append(append(tabs, after...), c.tabs[c.cur+1:]...)
	c.cur += len(before)
}

// CycleTab shows the tab by steps after (or before) the one shown.
func (s *App) CycleTab(by int) {
	n := len(s.ctl.tabs)
	s.ShowTab(((s.ctl.cur+by)%n + n) % n)
}

// CloseTab closes the ith tab; closing the last leaves an empty one.
func (s *App) CloseTab(i int) {
	c := s.ctl
	if i < 0 || i >= len(c.tabs) {
		return
	}
	if len(c.tabs) == 1 {
		c.tabIDs++
		c.tabs[0] = &tab{id: c.tabIDs, app: App{hidden: map[int]bool{}, collapsed: map[string]bool{}, tabID: c.tabIDs}}
		s.enter(c.tabs[0])
		c.focusPath = true
		return
	}
	c.tabs = append(c.tabs[:i], c.tabs[i+1:]...)
	switch {
	case i == c.cur:
		c.cur = min(i, len(c.tabs)-1)
		s.enter(c.tabs[c.cur])
	case i < c.cur:
		c.cur--
	}
}

// Tabs reports how many tabs are open and which is shown.
func (s *App) Tabs() (n, shown int) { return len(s.ctl.tabs), s.ctl.cur }

// SetMenuTabs says that the app's menu takes Ctrl/⌘+T and Ctrl/⌘+W (on
// macOS, where the menu bar has them), so that the window does not too.
func (s *App) SetMenuTabs(on bool) { s.ctl.menuTabs = on }

// empty reports whether the tab shown shows nothing, nor is opening.
func (s *App) empty() bool { return s.tr == nil && (s.loading == "" || s.loadErr != nil) }

// tabShortcuts takes the tabs' keys (see the top of this file), done the
// next frame: a switch changes the App the frame draws.
func (s *App) tabShortcuts(c *ui.Context) {
	do := func(fn func(*App)) {
		s.post(fn)
		c.AnimationFrame()
	}
	if (!s.ctl.menuTabs && c.Shortcut(ui.Cmd, ui.KeyT)) || c.Shortcut(ui.Alt, ui.KeyT) {
		do(func(s *App) { s.NewTab() })
	}
	if (!s.ctl.menuTabs && c.Shortcut(ui.Cmd, ui.KeyW)) || c.Shortcut(ui.Alt, ui.KeyW) {
		do(func(s *App) { s.CloseTab(s.ctl.cur) })
	}
	if c.Shortcut(ui.Ctrl, ui.KeyTab) || c.Shortcut(ui.Cmd|ui.Shift, ui.KeyBracketRight) || c.Shortcut(ui.Alt, ui.KeyBracketRight) {
		do(func(s *App) { s.CycleTab(1) })
	}
	if c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyTab) || c.Shortcut(ui.Cmd|ui.Shift, ui.KeyBracketLeft) || c.Shortcut(ui.Alt, ui.KeyBracketLeft) {
		do(func(s *App) { s.CycleTab(-1) })
	}
	digits := []ui.Key{ui.Key1, ui.Key2, ui.Key3, ui.Key4, ui.Key5, ui.Key6, ui.Key7, ui.Key8, ui.Key9}
	for k, key := range digits {
		if c.Shortcut(ui.Cmd, key) || c.Shortcut(ui.Alt, key) {
			do(func(s *App) {
				if k == 8 {
					s.ShowTab(len(s.ctl.tabs) - 1)
				} else {
					s.ShowTab(k)
				}
			})
		}
	}
}

// tabBar draws the tabs: a tab a timeline (its name, a spinner while it
// opens, ×), and + for a new one.
func (s *App) tabBar(c *ui.Context) {
	c2 := s.ctl
	// What the bar does changes the App the frame draws: next frame.
	later := func(fn func(*App)) {
		s.post(fn)
		c.AnimationFrame()
	}
	titles := s.tabTitles()
	ui.Row(c.Key("tabs")).Height(theme.TabBarH).PaddingX(10).Gap(2).Background(colPanel).
		BorderWidth(0, 0, 1, 0).BorderColor(colBorder).Label("Tabs").Children(func() {
		for i, t := range c2.tabs {
			a := &t.app
			if i == c2.cur {
				a = s
			}
			active := i == c2.cur
			hint := ""
			switch {
			case i < 8:
				hint = " (Ctrl/⌘+" + strconv.Itoa(i+1) + ")"
			case i == len(c2.tabs)-1:
				hint = " (Ctrl/⌘+9)"
			}
			tip := a.inputs
			if tip == "" {
				tip = "An empty tab: type a path above"
			}
			row := ui.Row(c.Key(t.id)).Gap(6).PaddingStart(12).PaddingEnd(4).Height(theme.TabBarH - 6).
				Width(theme.TabMaxW).MinWidth(theme.TabMinW).Shrink(1).Radius(7).Focusable().
				Label("Tab " + titles[i]).Tooltip(tip + hint)
			switch {
			case active:
				row.Background(colBg).Border(1, colBorder)
			case row.Hovered():
				row.Background(colPanelAlt)
			}
			closed := false
			row.Children(func() {
				if active {
					ui.Box(c).Size(3, 14).Radius(1.5).Background(colAccent)
				}
				if a.loading != "" && a.loadErr == nil {
					ui.Spinner(c).Size(12, 12)
				}
				name := ui.Text(c, titles[i]).FontSize(12.5).SingleLine().Grow(1).Shrink(1)
				if active {
					name.Bold()
				} else {
					name.TextColor(colMuted)
				}
				x := glyphButton(c, c.Key("x"), "×", "Close tab "+titles[i], "Close tab (Ctrl/⌘+W)", active || row.Hovered())
				closed = x.Clicked()
			})
			if closed {
				later(func(s *App) { s.CloseTab(i) })
			} else if row.Clicked() {
				later(func(s *App) { s.ShowTab(i) })
			}
		}
		if glyphButton(c, c.Key("new-tab"), "+", "New tab", "New tab (Ctrl/⌘+T) · Ctrl/⌘+Enter in the path box opens there in a new tab", true).Clicked() {
			later(func(s *App) { s.NewTab() })
		}
	})
}

// glyphButton is a small borderless button of one glyph (the tabs' × and
// +), faint unless shown.
func glyphButton(c *ui.Context, key *ui.Context, glyph, label, tip string, shown bool) ui.Element {
	b := ui.Row(key).Size(22, 22).Radius(5).Center().Focusable().Label(label).Tooltip(tip)
	col := colFaint
	if !shown {
		col = colFaint.Alpha(0)
	}
	if b.Hovered() {
		b.Background(colBorder)
		col = colText
	}
	b.Children(func() { ui.Text(c, glyph).FontSize(15).TextColor(col) })
	return b
}
