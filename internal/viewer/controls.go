package viewer

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// The views of control.go: the path box, the server's status, the connect
// dialog.

// shortcuts takes the window's keys for the controls: Ctrl/⌘+L or / to the
// path box, Ctrl/⌘+O to open, Ctrl/⌘+K to connect.
func (s *App) shortcuts(c *ui.Context) {
	ctl := s.ctl
	if ctl.showConnect {
		return
	}
	if c.Shortcut(ui.Cmd, ui.KeyL) || (!ctl.pathFocused && c.Shortcut(0, ui.KeySlash)) {
		ctl.focusPath = true
	}
	if !ctl.pathFocused && s.tr != nil && c.Shortcut(0, ui.KeyC) {
		s.toggleCompact()
	}
	if c.Shortcut(ui.Cmd, ui.KeyO) {
		s.browse()
	}
	if c.Shortcut(ui.Cmd, ui.KeyK) && !ctl.web {
		s.openConnect()
	}
}

// browse is Open…: the server's traces when connected, else a file dialog.
func (s *App) browse() {
	if s.ctl.state == connConnected {
		s.ShowRemote()
		return
	}
	if s.ctl.browse != nil {
		s.ctl.browse()
	}
}

func (s *App) openConnect() {
	ctl := s.ctl
	ctl.showConnect = true
	if ctl.urlText == "" {
		if ctl.server != "" {
			ctl.urlText = ctl.server
		} else if len(ctl.settings.Servers) > 0 {
			ctl.urlText = ctl.settings.Servers[0]
		}
	}
}

// pathBox is the path input: a trace, a folder of traces or a glob, on this
// machine or, connected, on the server; recent paths and the folder's
// entries are suggested as it is typed.
func (s *App) pathBox(c *ui.Context, width float32) {
	ctl := s.ctl
	placeholder := "Path of a trace or a folder of traces  (⌘L or /)"
	label := "Path"
	if ctl.state == connConnected {
		placeholder = "Path on " + hostOf(ctl.server) + ": a trace, a folder or a glob  (⌘L)"
		label = "Path on the server"
	}
	box := ui.Autocomplete(c.Key("path"), &ctl.pathText, s.suggestions()).Label(label).Placeholder(placeholder).Width(width)
	if ctl.pathErr != "" {
		box.Error(ctl.pathErr)
	}
	if ctl.focusPath {
		box.Focus()
		ctl.focusPath = false
	}
	ctl.pathFocused = box.Focused()
	if box.Changed() {
		ctl.pathErr = ""
	}
	// Ctrl/⌘+Enter opens in a new tab.
	newTab := ctl.pathFocused && c.Shortcut(ui.Cmd, ui.KeyEnter)
	if box.Submitted() || newTab {
		newTab = newTab || s.mods&ui.Cmd != 0
		text := ctl.pathText
		if !newTab || s.empty() {
			s.OpenPath(text)
			return
		}
		// Next frame: the new tab changes the App this one draws.
		s.post(func(s *App) {
			if !s.NewTab() {
				s.ctl.pathErr = "Too many tabs: close one first"
				return
			}
			s.ctl.pathText = text
			s.OpenPath(text)
		})
		c.AnimationFrame()
	}
}

// connDot is the color of the connection's state.
func (s *App) connDot() ui.Color {
	switch s.ctl.state {
	case connConnected:
		return colSwatchStream
	case connConnecting:
		return colRange
	case connFailed, connSignIn:
		return colWarn
	}
	return colFaint
}

func (s *App) connText() string {
	ctl := s.ctl
	switch ctl.state {
	case connConnected:
		return hostOf(ctl.server)
	case connConnecting:
		return "Connecting to " + hostOf(ctl.server) + "…"
	case connFailed:
		return "Not connected"
	case connSignIn:
		return "Sign-in needed"
	}
	return "Local"
}

// connChip shows where paths open (this machine or a server), and opens the
// connect dialog.
func (s *App) connChip(c *ui.Context) {
	chip := ui.Row(c.Key("conn-chip")).Gap(7).Padding(4, 10).Radius(999).Border(1, colBorder).Focusable().
		Label("Server").Tooltip("Connect to a server (Ctrl/⌘+K)")
	if s.ctl.web {
		chip.Tooltip("The server this page is on: traces are parsed there and streamed here")
	} else {
		if chip.Hovered() || chip.Focused() {
			chip.Background(colPanelAlt)
		}
		if chip.Clicked() {
			s.openConnect()
		}
	}
	chip.Children(func() {
		dot := s.connDot()
		ui.Box(c).Size(8, 8).Radius(4).Background(dot)
		ui.Text(c, s.connText()).FontSize(12).SingleLine().MaxWidth(220)
	})
}

// connectDialog connects to a server: its URL and token, the servers of
// late, the state of the connection, and disconnecting.
func (s *App) connectDialog(c *ui.Context) {
	ctl := s.ctl
	if !ctl.showConnect {
		return
	}
	ui.Modal(c, &ctl.showConnect, func() {
		ui.Column(c).Width(520).Padding(20).Gap(12).Radius(12).Background(colPanel).Border(1, colBorder).Children(func() {
			if ctl.web {
				ui.Text(c, "Connect to this page's server").FontSize(16).Bold()
			} else {
				ui.Text(c, "Connect to a server").FontSize(16).Bold()
				ui.Text(c, "Open traces on a machine running tracelight serve: they are parsed there and streamed here as tiles of the timeline.").
					FontSize(12).TextColor(colMuted).MaxWidth(480)
			}
			ui.Row(c).Gap(8).Children(func() {
				ui.Box(c).Size(8, 8).Radius(4).Background(s.connDot())
				status := "Not connected: paths open on this machine"
				switch ctl.state {
				case connConnected:
					status = "Connected to " + strings.TrimSuffix(ctl.server, "/")
					if ctl.root != "" {
						status += " · serving " + ctl.root
					}
				case connConnecting:
					status = "Connecting to " + strings.TrimSuffix(ctl.server, "/") + "…"
				case connFailed:
					status = "Could not connect: " + ctl.err.Error()
				case connSignIn:
					status = "Sign-in needed: " + ctl.err.Error()
					if ctl.signingIn {
						status = "Sign in in the window that opened; Tracelight connects once you are in."
					}
				}
				t := ui.Text(c, status).FontSize(12).Shrink(1).MaxLines(3)
				if ctl.state == connFailed {
					t.TextColor(colWarn)
				}
				if ctl.state == connConnecting {
					ui.Spinner(c)
				}
			})
			connect := false
			if !ctl.web {
				ui.Text(c, "Server URL").FontSize(11.5).TextColor(colFaint)
				u := ui.TextInput(c.Key("server-url"), &ctl.urlText).Label("Server URL").Placeholder("http://gpu-box:8737").AutoFocus()
				if u.Submitted() {
					connect = true
				}
			}
			ui.Text(c, "Token (if the server has one; not kept)").FontSize(11.5).TextColor(colFaint)
			tk := ui.TextInput(c.Key("server-token"), &ctl.tokenText).Label("Token").Placeholder("optional").Password()
			if tk.Submitted() {
				connect = true
			}
			ui.Row(c).Gap(8).Children(func() {
				if ui.PrimaryButton(c, "Connect").Disabled(strings.TrimSpace(ctl.urlText) == "").Clicked() {
					connect = true
				}
				if !ctl.web && (ctl.state == connConnected || ctl.state == connConnecting) && ui.Button(c, "Disconnect").Clicked() {
					s.Disconnect()
				}
				ui.Spacer(c)
				if ctl.state == connConnected && ui.Button(c, "Browse its traces…").Clicked() {
					ctl.showConnect = false
					s.ShowRemote()
				}
				if ui.Button(c, "Close").Clicked() {
					ctl.showConnect = false
				}
			})
			s.signInControls(c)
			if connect && strings.TrimSpace(ctl.urlText) != "" {
				s.Connect(ctl.urlText, ctl.tokenText, "")
			}
			if len(ctl.settings.Servers) > 0 {
				ui.Text(c, "RECENT SERVERS").FontSize(10.5).Bold().TextColor(colFaint).MarginY(4)
				ui.Column(c).Gap(2).Children(func() {
					for _, srv := range ctl.settings.Servers {
						row := ui.Row(c.Key("srv:"+srv)).Padding(6, 10).Gap(8).Radius(6).Focusable().Label(srv)
						if row.Hovered() || row.Focused() {
							row.Background(colPanelAlt)
						}
						if row.Clicked() {
							ctl.urlText = srv
							s.Connect(srv, ctl.tokenText, "")
						}
						row.Children(func() {
							current := ctl.state == connConnected && srv == ctl.server
							ui.Text(c, strings.TrimSuffix(srv, "/")).FontSize(12.5).SingleLine().Grow(1).Shrink(1)
							if current {
								ui.Text(c, "connected").FontSize(11).TextColor(colSwatchStream)
							}
							if n := len(ctl.settings.RecentServerPaths[srv]); n > 0 {
								ui.Text(c, compact(n)+" recent paths").FontSize(11).TextColor(colFaint)
							}
						})
					}
				})
			}
		})
	})
}

// signInControls are the connect dialog's ways through a sign-in gateway:
// the sign-in window, or a Cookie header pasted from a signed-in browser;
// and, once in, forgetting the session kept.
func (s *App) signInControls(c *ui.Context) {
	ctl := s.ctl
	switch {
	case ctl.state == connSignIn:
		ui.Column(c).Gap(8).Padding(10, 12).Radius(8).Background(colPanelAlt).Border(1, colBorder).Children(func() {
			ui.Text(c, "This server is behind a sign-in page.").FontSize(12.5).Bold()
			if ctl.signIn != nil {
				ui.Row(c).Gap(8).Children(func() {
					label := "Sign in…"
					if ctl.signingIn {
						label = "Signing in…"
					}
					if ui.PrimaryButton(c, label).Disabled(ctl.signingIn).Tooltip("Opens the sign-in page in a Tracelight window; the session is kept for this server").Clicked() {
						s.SignIn()
					}
					ui.Text(c, "in a Tracelight window, as in your browser").FontSize(11.5).TextColor(colFaint)
				})
			}
			ui.Text(c, "Or paste the Cookie header of a signed-in browser's request to this server (developer tools › Network › a request › Request headers › Cookie):").
				FontSize(11.5).TextColor(colFaint).MaxWidth(480)
			ui.Row(c).Gap(8).Children(func() {
				in := ui.TextInput(c.Key("server-cookie"), &ctl.cookieText).Label("Cookie header").Placeholder("name=value; other=value").Password().Grow(1)
				if (ui.Button(c, "Use").Disabled(strings.TrimSpace(ctl.cookieText) == "").Clicked() || in.Submitted()) && strings.TrimSpace(ctl.cookieText) != "" {
					s.UseCookie(ctl.cookieText)
				}
			})
		})
	case ctl.state == connConnected && ctl.settings.ServerCookies[ctl.server] != "":
		ui.Row(c).Gap(8).Children(func() {
			ui.Text(c, "Signed in through the server's gateway; the session is kept for it.").FontSize(11.5).TextColor(colFaint).Grow(1)
			if ui.Button(c, "Forget the session").Clicked() {
				s.ForgetCookie(ctl.server)
			}
		})
	}
}
