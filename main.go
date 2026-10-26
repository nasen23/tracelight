// Command tracelight is a desktop viewer for GPU traces recorded by NVIDIA
// Nsight Systems, an open alternative to its timeline.
//
//	tracelight trace.sqlite                     # open a window
//	tracelight trace.nsys-rep                   # converts with `nsys export` first
//	tracelight --screenshot out.png trace.sqlite  # render headlessly, no window or GPU
//	tracelight --info trace.sqlite              # print a summary and exit
//	tracelight bench trace.sqlite               # time loading and a scripted zoom/pan sequence
//	tracelight serve --root DIR --addr :8737    # serve DIR's traces to browsers and desktop clients
//	tracelight --server http://host:8737        # open traces from a server
//	tracelight --version                        # print the version (also: tracelight version)
package main

import (
	"flag"
	"fmt"
	"image/png"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"tracelight/internal/remote"
	"tracelight/internal/theme"
	"tracelight/internal/trace"
	"tracelight/internal/version"
	"tracelight/internal/viewer"
)

var viewSpan *float64

var themeFlag *string

func main() {
	remote.UserAgent = "Tracelight/" + version.Version
	log.SetFlags(0)
	log.SetPrefix("tracelight: ")
	shot := flag.String("screenshot", "", "render the window to this PNG headlessly and exit")
	size := flag.String("size", "1600x1000", "window size for --screenshot, WxH")
	scale := flag.Float64("scale", 1, "pixel scale of the --screenshot (2 for a Retina-like image)")
	selectFirst := flag.Bool("select", true, "with --screenshot, select a kernel so the details panel shows")
	viewSpan = flag.Float64("span", 0, "with --screenshot, milliseconds of the trace to show (0 picks; with -select=false the whole trace)")
	info := flag.Bool("info", false, "print a summary of the trace and exit")
	themeFlag = flag.String("theme", "dark", "with --screenshot, the appearance: light or dark")
	serverURL := flag.String("server", "", "open traces from a server started with `tracelight serve` (the trace argument is then a path on it)")
	token := flag.String("token", trace.Getenv("TOKEN"), "with --server, the server's token ($TRACELIGHT_TOKEN)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: tracelight [flags] [trace.sqlite|trace.nsys-rep]\n\n")
		flag.PrintDefaults()
	}
	showVersion := flag.Bool("version", false, "print the version and exit")
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("tracelight " + version.Version)
			return
		case "bench":
			benchMain(os.Args[2:])
			return
		case "serve":
			serveMain(os.Args[2:])
			return
		}
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("tracelight " + version.Version)
		return
	}
	path := flag.Arg(0)
	// A server's URL (the web page's, or a link to traces, as copied from
	// the browser) for a file: open what it links to on that server.
	if *serverURL == "" && (strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")) {
		*serverURL, path = path, ""
	}
	if *serverURL != "" {
		if *info || *shot != "" || flag.NArg() > 1 {
			log.Fatal("--server opens a window: --info and --screenshot work on local files")
		}
		run(path, *serverURL, *token)
		return
	}
	if flag.NArg() > 1 || (path == "" && (*info || *shot != "")) {
		flag.Usage()
		os.Exit(2)
	}
	if path == "" || (*shot == "" && !*info) {
		run(path, "", "") // the window loads the trace itself, showing its progress
		return
	}

	start := time.Now()
	tr, err := trace.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	loaded := time.Since(start)
	if *info {
		printInfo(tr, loaded)
		return
	}
	var w, h int
	if _, err := fmt.Sscanf(*size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		log.Fatalf("bad --size %q, want WxH", *size)
	}
	if err := screenshot(tr, *shot, w, h, float32(*scale), *selectFirst); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("loaded %s events in %v, wrote %s\n", fmtInt(tr.EventCount()), loaded.Round(time.Millisecond), *shot)
}

// run opens the window, and path in it unless it is empty: on the server
// at serverURL, if given, after connecting to it. Files also come from the
// file manager, the Dock, drops and the Open dialog; the window's path box
// and its Connect dialog do the rest.
func run(path, serverURL, token string) {
	app := viewer.New(nil)
	var win *mygo.Window
	app.SetBrowse(func() {
		go func() {
			paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
				Parent: win,
				Title:  "Open traces (several open side by side)",
				// Several files: the ranks to compare, the others left out.
				Multiple: true,
				Filters: []mygo.FileFilter{
					{Name: "Traces", Extensions: []string{"sqlite", "nsys-rep", "qdrep", "json", "gz"}},
					{Name: "Nsight Systems traces", Extensions: []string{"sqlite", "nsys-rep", "qdrep"}},
					{Name: "PyTorch profiler traces", Extensions: []string{"json", "gz"}},
					{Name: "All files", Extensions: []string{"*"}},
				},
			})
			if err == nil && len(paths) > 0 {
				win.Update(func() { app.OpenLocalPaths(paths) })
			}
		}()
	})
	var pending []string
	mygo.App.SetName("Tracelight") // its data directory, as the packaged app's
	mygo.App.OnOpenFile(func(p string) {
		if win == nil {
			pending = append(pending, p)
			return
		}
		app.OpenLocal(p)
	})
	mygo.App.WhenReady(func() {
		win = mygo.NewWindow(mygo.WindowOptions{
			Title:     viewer.Title(path),
			Width:     1440,
			Height:    900,
			MinWidth:  800,
			MinHeight: 500,
			StateKey:  "main",
			Content:   ui.View(app.View),
		})
		app.SetWake(func() { win.Update(func() {}) })
		tabMenu(app, win)
		app.SetSignIn(signIn)
		app.OnTitle(win.SetTitle)
		setupSettings(app)
		switch {
		case serverURL != "":
			app.Connect(serverURL, token, path)
		case path != "":
			app.OpenLocal(path)
		}
		for _, p := range pending {
			app.OpenLocal(p)
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// screenshot renders the viewer without a window, on the CPU, with MyGo's
// in-memory tester: it needs no display and no GPU.
func screenshot(tr *trace.Trace, out string, w, h int, scale float32, selectEvent bool) error {
	app := viewer.New(tr)
	app.SetAnimations(false)
	app.SetThemeMode(theme.ParseMode(*themeFlag))
	tt := ui.NewTester(app.View, w, h)
	if scale != 1 {
		tt.SetScale(scale)
	}
	if selectEvent {
		if ref, ok := viewer.ShowcaseEvent(tr); ok {
			app.Select(ref)
			// Zoom in around it, so that kernels have room for their names.
			e := tr.Event(ref)
			span := float64(min(max(e.Dur()*40, 1_000_000), tr.Duration()))
			if *viewSpan > 0 {
				span = *viewSpan * 1e6
			}
			mid := float64(e.Start-tr.Start) + float64(e.Dur())/2
			app.ZoomTo(mid-span/2, mid+span/2)
			tt.Frame()
			if x, y, ok := app.EventCenter(ref); ok {
				tt.Move(x, y) // the tooltip, as the pointer rests on the event
			}
		}
	}
	tt.Frame()
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := png.Encode(f, tt.Image()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func printInfo(tr *trace.Trace, loaded time.Duration) {
	fmt.Printf("%s: %s events in %d rows, %.3f ms, loaded in %v\n", tr.Path, fmtInt(tr.EventCount()), len(tr.Tracks),
		float64(tr.Duration())/1e6, loaded.Round(time.Millisecond))
	for _, k := range []trace.Kind{trace.KindKernel, trace.KindMemcpy, trace.KindMemset, trace.KindRuntime, trace.KindNVTX, trace.KindNVTXMark} {
		fmt.Printf("  %-11s %10s\n", k, fmtInt(tr.Counts[k]))
	}
	for _, m := range tr.Missing {
		fmt.Printf("  missing table %s\n", m)
	}
	for _, w := range tr.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	fmt.Println("rows:")
	for _, t := range tr.Tracks {
		fmt.Printf("  %-40s %-40s %9s events, %d lanes\n", t.Group, t.Name, fmtInt(t.Count), len(t.Lanes))
	}
	fmt.Println("top kernels by total time:")
	for _, s := range tr.TopByTotal(10, trace.KindKernel) {
		fmt.Printf("  %6.2f%%  %10.3f ms  %7d×  %s\n", 100*s.Share, float64(s.Total)/1e6, s.Count, s.Name)
	}
}

func fmtInt(n int) string {
	s := fmt.Sprint(n)
	out := ""
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(r)
	}
	return out
}

// tabMenu gives the macOS menu bar the tabs' New Tab (⌘T) and Close Tab
// (⌘W), which would close the window there, Close Window going to ⇧⌘W;
// the window takes the other tab keys itself, and elsewhere all of them.
func tabMenu(app *viewer.App, win *mygo.Window) {
	if runtime.GOOS != "darwin" {
		return
	}
	do := func(fn func()) func(*mygo.MenuItem, *mygo.Window) {
		return func(*mygo.MenuItem, *mygo.Window) { win.Update(fn) }
	}
	app.SetMenuTabs(true)
	mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Label: "File", Submenu: []*mygo.MenuItem{
			{Label: "New Tab", Accelerator: "CmdOrCtrl+T", Click: do(func() { app.NewTab() })},
			{Label: "Open…", Accelerator: "CmdOrCtrl+O", Click: do(app.Browse)},
			mygo.Separator(),
			{Label: "Close Tab", Accelerator: "CmdOrCtrl+W", Click: do(func() { _, i := app.Tabs(); app.CloseTab(i) })},
			{Label: "Close Window", Accelerator: "Shift+CmdOrCtrl+W", Click: func(*mygo.MenuItem, *mygo.Window) { win.Close() }},
		}},
		{Role: mygo.RoleEditMenu},
		{Role: mygo.RoleWindowMenu},
	}))
}
