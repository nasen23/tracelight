//go:build js

package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/egoist/mygo"

	"tracelight/internal/viewer"
)

// bridge gives the page window.tracelight, for tests and benchmarks: the
// app's state as JSON (state()), and a few commands, done on the app's
// loop (zoomTo(a, b), animations(on), compact(mode), showTab(i),
// newTab(), closeTab(i), open(path, newTab), showTrace(i, on), soloTrace(i)).
func bridge(app *viewer.App, win *mygo.Window) {
	do := func(fn func(args []js.Value)) js.Func {
		return js.FuncOf(func(this js.Value, args []js.Value) any {
			win.Update(func() { fn(args) })
			return nil
		})
	}
	state := js.FuncOf(func(this js.Value, args []js.Value) any {
		type st struct {
			Loading  bool       `json:"loading"`
			Complete bool       `json:"complete"`
			Pending  int        `json:"pending"`
			Slots    [3]int     `json:"slots"`
			Extent   [2]float64 `json:"extent"`
			View     [2]float64 `json:"view"`
			Shown    bool       `json:"shown"`
			Tabs     [2]int     `json:"tabs"`
			Titles   []string   `json:"titles"`
			Blocks   int        `json:"blocks"`
			// Traces: of the session's, those shown and all.
			Traces [2]int `json:"traces"`
			// Data is where the timeline's events are drawn: x, y, w, h.
			Data [4]float32 `json:"data"`
			// Loads: the loading screen's lines, those done, and how far
			// they all are, in percent.
			Loads [3]int `json:"loads"`
		}
		var v st
		v.Loading = app.Loading()
		v.Blocks, v.Complete, v.Pending = app.StreamStats()
		v.Slots[0], v.Slots[1], v.Slots[2] = app.StreamSlots()
		v.Extent[0], v.Extent[1] = app.Extent()
		v.View[0], v.View[1], v.Shown = app.Shown()
		v.Tabs[0], v.Tabs[1] = app.Tabs()
		v.Titles = app.TabTitles()
		v.Traces[0], v.Traces[1] = app.TracesShown()
		v.Data[0], v.Data[1], v.Data[2], v.Data[3] = app.DataArea()
		n, done, frac := app.LoadProgress()
		v.Loads = [3]int{n, done, int(frac * 100)}
		b, _ := json.Marshal(v)
		return string(b)
	})
	js.Global().Set("tracelight", js.ValueOf(map[string]any{
		"state":      state,
		"zoomTo":     do(func(a []js.Value) { app.ZoomTo(a[0].Float(), a[1].Float()) }),
		"animations": do(func(a []js.Value) { app.SetAnimations(a[0].Bool()) }),
		"compact":    do(func(a []js.Value) { app.SetCompact(a[0].String()) }),
		"showTab":    do(func(a []js.Value) { app.ShowTab(a[0].Int()) }),
		"newTab":     do(func(a []js.Value) { app.NewTab() }),
		"closeTab":   do(func(a []js.Value) { app.CloseTab(a[0].Int()) }),
		"showTrace":  do(func(a []js.Value) { app.SetTraceShown(a[0].Int(), a[1].Bool()) }),
		"soloTrace":  do(func(a []js.Value) { app.SoloTrace(a[0].Int()) }),
		"open": do(func(a []js.Value) {
			if len(a) > 1 && a[1].Bool() && !app.Empty() {
				app.NewTab()
			}
			app.OpenPath(a[0].String())
		}),
	}))
}
