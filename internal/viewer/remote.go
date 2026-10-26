package viewer

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
)

// RemoteEntry is a file or directory on a server.
type RemoteEntry struct {
	Name, Path string
	// Type is "dir", "sqlite" or "report".
	Type string
	Size int64
	// Ready says whether the server can open the trace now; Note, how or
	// why not.
	Ready bool
	Note  string
}

// picker is the state of the picker of a server's traces.
type picker struct {
	url     string
	dir     string
	entries []RemoteEntry
	err     error
	listing bool
	// shown is whether the picker shows over the trace.
	shown bool
	// picked are the traces ticked to open side by side, in the order
	// ticked (from any folder).
	picked []string
}

// ShowRemote shows the picker of the server's traces, over the trace if one
// is open.
func (s *App) ShowRemote() {
	if s.picker == nil {
		return
	}
	s.picker.shown = true
	s.listRemote(s.picker.dir)
}

func (s *App) listRemote(dir string) {
	pk, client := s.picker, s.ctl.client
	pk.listing, pk.err = true, nil
	if client == nil {
		pk.listing, pk.err = false, errors.New("not connected")
		return
	}
	pc := s.ctl // the App is rewritten as tabs switch
	go func() {
		d, entries, err := client.List(dir)
		out := make([]RemoteEntry, len(entries))
		for i, e := range entries {
			out[i] = RemoteEntry{Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size, Ready: e.Ready, Note: e.Note}
		}
		pc.post(func(s *App) { s.setRemoteListing(d, out, err) })
	}()
}

// setRemoteListing shows the entries of the server's directory dir, or why
// they could not be listed.
func (s *App) setRemoteListing(dir string, entries []RemoteEntry, err error) {
	if s.picker == nil {
		return
	}
	s.picker.listing, s.picker.err = false, err
	if err == nil {
		s.picker.dir, s.picker.entries = dir, entries
	}
}

// remotePicker lists the server's directory, to open a trace from it.
func (s *App) remotePicker(c *ui.Context) {
	rm := s.picker
	t := c.Theme()
	ui.Column(c).Width(680).MaxHeight(520).Radius(12).Background(colPanel).Border(1, colBorder).Clip().Children(func() {
		ui.Row(c).Padding(14, 16, 8, 16).Gap(10).Children(func() {
			ui.Text(c, "Traces on "+strings.TrimSuffix(rm.url, "/")).FontSize(14).Bold().SingleLine().Grow(1).Shrink(1)
			if rm.listing {
				ui.Spinner(c)
			}
			if s.tr != nil && ui.Button(c, "Back to the trace").Clicked() {
				rm.shown = false
			}
		})
		// The path, each part a way back up.
		ui.Row(c).Padding(0, 12, 8, 12).Gap(2).Wrap().Children(func() {
			crumb := func(label, dir string, current bool) {
				b := ui.Row(c.Key("crumb:"+dir)).Padding(2, 6).Radius(5)
				if b.Hovered() && !current {
					b.Background(t.SurfaceHover)
				}
				if b.Clicked() && !current {
					s.listRemote(dir)
				}
				b.Children(func() {
					txt := ui.Text(c, label).FontSize(12.5).TextColor(colMuted)
					if current {
						txt.TextColor(colText).Bold()
					}
				})
			}
			parts := []string{}
			if rm.dir != "" {
				parts = strings.Split(rm.dir, "/")
			}
			crumb("root", "", len(parts) == 0)
			for i, p := range parts {
				ui.Text(c, "/").FontSize(12.5).TextColor(colFaint)
				crumb(p, strings.Join(parts[:i+1], "/"), i == len(parts)-1)
			}
		})
		ui.Scroll(c).Grow(1).Padding(6).Gap(1).BorderWidth(1, 0, 0, 0).BorderColor(colBorder).MinHeight(160).Children(func() {
			switch {
			case rm.err != nil:
				ui.Text(c, rm.err.Error()).TextColor(colWarn).Padding(10).Selectable()
			case len(rm.entries) == 0 && !rm.listing:
				ui.Text(c, "No traces or folders here.").TextColor(colFaint).Padding(10)
			}
			for _, e := range rm.entries {
				row := ui.Row(c.Key("e:"+e.Path)).Padding(7, 10).Gap(10).Radius(7).Focusable()
				// A trace's box: ticked ones open side by side.
				on := slices.Contains(rm.picked, e.Path)
				if row.Hovered() || row.Focused() {
					row.Background(t.SurfaceHover)
				}
				if row.Clicked() {
					if e.Type == "dir" {
						s.listRemote(e.Path)
					} else {
						rm.shown = false
						s.openRemote([]string{e.Path}, e.Path)
					}
				}
				row.Children(func() {
					if e.Type == "dir" {
						ui.Box(c).Width(16)
					} else if ui.Checkbox(c.Key("pick"), &on, "").Label("Pick " + e.Name).Tooltip("Pick to open side by side").Changed() {
						if on {
							rm.picked = append(rm.picked, e.Path)
						} else {
							rm.picked = slices.DeleteFunc(rm.picked, func(p string) bool { return p == e.Path })
						}
					}
					mark, col := "▤", colMuted
					if e.Type == "dir" {
						mark, col = "▸", colAccent
					}
					ui.Text(c, mark).FontSize(13).TextColor(col).Width(14)
					ui.Text(c, e.Name).FontSize(13).SingleLine().Grow(1).Shrink(1)
					if e.Note != "" {
						note := ui.Text(c, e.Note).FontSize(11).TextColor(colFaint)
						if !e.Ready {
							note.TextColor(colWarn)
						}
					}
					if e.Size > 0 {
						ui.Text(c, fmtBytes(e.Size)).FontSize(11.5).TextColor(colFaint).Width(80).TextAlign(ui.End)
					}
				})
			}
		})
		ui.Row(c).Padding(10, 16).BorderWidth(1, 0, 0, 0).BorderColor(colBorder).Children(func() {
			ui.Text(c, "Traces are parsed on the server and streamed as tiles of the timeline; nsys is needed only there.").
				FontSize(11.5).TextColor(colFaint).Grow(1)
			if n := len(rm.picked); n > 0 {
				if ui.Button(c, "Clear").Clicked() {
					rm.picked = nil
				}
				label := fmt.Sprintf("Open %d picked side by side", n)
				if n == 1 {
					label = "Open the picked trace"
				}
				if ui.PrimaryButton(c, label).Clicked() {
					picked := rm.picked
					rm.picked, rm.shown = nil, false
					s.openRemote(picked, strings.Join(picked, " "))
				}
			} else if rm.dir != "" && len(rm.entries) > 1 && ui.Button(c, "Open this folder side by side").Clicked() {
				rm.shown = false
				s.OpenPath(strings.TrimSuffix(s.ctl.root, "/") + "/" + rm.dir)
			}
		})
	})
}
