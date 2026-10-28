// Package server serves traces under a root directory to browsers and to
// the desktop app: a file browser, trace metadata, timeline tiles computed
// on demand, summaries, event details, and the web UI itself.
package server

import (
	"compress/gzip"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
	"tracelight/internal/version"
)

//go:embed web
var webFS embed.FS

// webuiFS is the web client of MyGo's WebAssembly: the desktop app's
// views in the browser (cmd/webui), app.wasm.gz built by go generate.
//
//go:embed webui
var webuiFS embed.FS

// Config configures a server.
type Config struct {
	// Root is the directory whose traces the server serves; nothing
	// outside it is ever read.
	Root string
	// Token, if set, must come with every API request as
	// "Authorization: Bearer <token>".
	Token string
	// Open says where nsys is and where converted reports are cached.
	Open trace.OpenOptions
	// MaxTraces is how many traces stay loaded (16 if 0).
	MaxTraces int
	// Log, if set, logs each request.
	Log *log.Logger
	// UI is the web client served at /: "wasm" (the desktop app's views,
	// compiled to WebAssembly) or "js" (the JavaScript one, the default);
	// the other is at app.html or classic.html.
	UI string
}

// Server is an http.Handler serving traces.
type Server struct {
	cfg   Config
	root  *root
	cache *cache
	tiles *tileCache
	mux   *http.ServeMux
	// listing holds the progress of the merged files /api/session is
	// reading to list their ranks, by real path (/api/progress).
	listing sync.Map
}

// New makes a server for cfg.
func New(cfg Config) (*Server, error) {
	r, err := newRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	if cfg.MaxTraces <= 0 {
		cfg.MaxTraces = 16
	}
	s := &Server{cfg: cfg, root: r, cache: newCache(cfg.Open, cfg.MaxTraces), tiles: newTileCache(128 << 20), mux: http.NewServeMux()}
	s.mux.Handle("GET /", uiHandler(cfg.UI))
	themeJS := theme.JS() // the desktop app's look, for the browser client
	s.mux.HandleFunc("GET /theme.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		out := compress(w, r)
		out.Write(themeJS)
		out.Close()
	})
	s.mux.HandleFunc("GET /api/info", s.api(s.info))
	s.mux.HandleFunc("GET /api/files", s.api(s.files))
	s.mux.HandleFunc("GET /api/trace", s.api(s.traceMeta))
	s.mux.HandleFunc("GET /api/session", s.api(s.session))
	s.mux.HandleFunc("GET /api/progress", s.api(s.progress))
	s.mux.HandleFunc("GET /api/tile", s.api(s.tile))
	s.mux.HandleFunc("GET /api/tiles", s.auth(s.tileBatch))
	s.mux.HandleFunc("GET /api/summary", s.api(s.summary))
	s.mux.HandleFunc("GET /api/event", s.api(s.event))
	s.mux.HandleFunc("GET /api/snapshot", s.auth(s.snapshot))
	return s, nil
}

// Root is the real path of the directory served.
func (s *Server) Root() string { return s.root.dir }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// WebAssembly needs 'wasm-unsafe-eval' (it runs no JavaScript eval), and
	// MyGo's page styles its canvas with a style element.
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-cache")
	}
	// Anything under /api that no endpoint takes answers in JSON too, never
	// with the web UI's files (which "GET /" would serve): a client that
	// asks the wrong thing is told so.
	if p := r.URL.Path; p == "/api" || strings.HasPrefix(p, "/api/") {
		if _, pattern := s.mux.Handler(r); pattern == "" || pattern == "GET /" {
			s.unknownAPI(w, r)
			s.logRequest(r, start)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
	s.logRequest(r, start)
}

func (s *Server) logRequest(r *http.Request, start time.Time) {
	if s.cfg.Log != nil {
		s.cfg.Log.Printf("%s %s %v", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Microsecond))
	}
}

// auth requires the token, if there is one.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="Tracelight"`)
				httpError(w, http.StatusUnauthorized, "a valid token is required")
				return
			}
		}
		h(w, r)
	}
}

// api wraps a handler returning a value to send as JSON, or an error.
func (s *Server) api(h func(*http.Request) (any, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := h(r)
		if err != nil {
			httpError(w, statusOf(err), err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		out := compress(w, r)
		enc := json.NewEncoder(out)
		if err := enc.Encode(v); err != nil && s.cfg.Log != nil {
			s.cfg.Log.Printf("encode: %v", err)
		}
		out.Close()
	})
}

// statusOf is the HTTP status for an error of a handler.
func statusOf(err error) int {
	var he httpErr
	switch {
	case errors.As(err, &he):
		return he.code
	case errors.Is(err, errOutside):
		return http.StatusForbidden
	case errors.Is(err, errBadPath):
		return http.StatusBadRequest
	case errors.Is(err, errNotFound), errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

type httpErr struct {
	code int
	msg  string
}

func (e httpErr) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return httpErr{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// apiEndpoints are the API's paths (all GET).
var apiEndpoints = []string{"/api/info", "/api/files", "/api/trace", "/api/session", "/api/progress", "/api/tile", "/api/tiles", "/api/summary", "/api/event", "/api/snapshot"}

// unknownAPI answers requests under /api that no endpoint takes: 405 for
// an endpoint asked with another method, 404 otherwise, in JSON.
func (s *Server) unknownAPI(w http.ResponseWriter, r *http.Request) {
	if slices.Contains(apiEndpoints, r.URL.Path) {
		w.Header().Set("Allow", "GET, HEAD")
		httpError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s %s: the API takes GET", r.Method, r.URL.Path))
		return
	}
	httpError(w, http.StatusNotFound, fmt.Sprintf("%s %s: no such API endpoint", r.Method, r.URL.Path))
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// compress returns a writer that gzips the response if the client takes it.
func compress(w http.ResponseWriter, r *http.Request) io.WriteCloser {
	w.Header().Add("Vary", "Accept-Encoding")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		return nopCloser{w}
	}
	w.Header().Set("Content-Encoding", "gzip")
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	return zw
}

func (s *Server) info(r *http.Request) (any, error) {
	notes := s.cfg.Open.Notes()
	if notes == nil {
		notes = []string{}
	}
	return map[string]any{"version": 1, "app": version.Version, "notes": notes, "auth": s.cfg.Token != "", "root": s.root.dir}, nil
}

func (s *Server) files(r *http.Request) (any, error) {
	dir, entries, err := s.root.list(r.URL.Query().Get("dir"), s.cfg.Open)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []entry{}
	}
	return map[string]any{"dir": dir, "entries": entries}, nil
}

// load resolves the request's path parameter and loads its trace.
func (s *Server) load(r *http.Request) (string, *loaded, error) {
	return s.loadPath(r.URL.Query().Get("path"))
}

// loadPath resolves a client's path and loads its trace.
func (s *Server) loadPath(p string) (string, *loaded, error) {
	// A rank of a merged file (trace.Members): the file, inside the root,
	// and the rank of it.
	file, rank, member := trace.SplitMember(p)
	real, err := s.root.resolve(file)
	if err != nil {
		return "", nil, err
	}
	if fi, err := os.Stat(real); err != nil || fi.IsDir() {
		return "", nil, badRequest("that is a directory, not a trace file")
	}
	if trace.Detect(real) == "" { // trace decides what it reads
		return "", nil, badRequest("not a trace file tracelight reads (an nsys .sqlite export or an .nsys-rep report)")
	}
	key, rel := real, s.root.rel(real)
	if member {
		key, rel = trace.MemberPath(real, rank), trace.MemberPath(rel, rank)
	}
	l, err := s.cache.get(key)
	if err != nil {
		return "", nil, httpErr{http.StatusUnprocessableEntity, err.Error()}
	}
	return rel, l, nil
}

type rowMeta struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Device int32  `json:"device"`
	Count  int    `json:"count"`
	Busy   int64  `json:"busy"`
	Lanes  int    `json:"lanes"`
	// Overview is the rank overview's sub-lane of each lane it draws
	// (trace.Track.Overview).
	Overview []int `json:"overview,omitempty"`
}

func trackKind(k trace.TrackKind) string {
	switch k {
	case trace.TrackStream:
		return "stream"
	case trace.TrackRuntime:
		return "api"
	case trace.TrackNVTX:
		return "nvtx"
	case trace.TrackCPU:
		return "cpu"
	case trace.TrackPython:
		return "python"
	}
	return "other"
}

func (s *Server) traceMeta(r *http.Request) (any, error) {
	p, l, err := s.load(r)
	if err != nil {
		return nil, err
	}
	tr := l.tr
	tr.Overview()
	rows := make([]rowMeta, len(tr.Tracks))
	for i, tk := range tr.Tracks {
		rows[i] = rowMeta{tk.Group, tk.Name, trackKind(tk.Kind), tk.Device, tk.Count, tk.Busy, len(tk.Lanes), tk.Overview}
	}
	counts := map[string]int{}
	for k, n := range tr.Counts {
		counts[k.String()] = n
	}
	devices := map[string]string{}
	for id, n := range tr.Devices {
		devices[strconv.Itoa(int(id))] = n
	}
	idle := tr.IdleDevices()
	if idle == nil {
		idle = []int32{}
	}
	return map[string]any{
		"path": p, "abs": filepath.Join(s.root.dir, filepath.FromSlash(p)), "size": l.size,
		"start": tr.Start, "sessionStart": tr.SessionStartUTC, "sessionStartNs": strconv.FormatInt(tr.SessionStartUTC, 10), "sessionMono": tr.SessionStartMono, "clock": tr.Clock, "collectives": l.collectives(), "host": tr.Host, "duration": tr.Duration(), "events": tr.EventCount(),
		"counts": counts, "devices": devices, "idle": idle, "rows": rows,
		"missing": tr.Missing, "warnings": tr.Warnings, "loadMs": l.took.Milliseconds(),
		"tilePx": TilePx,
	}, nil
}

// Tiles: a tile is TilePx pixels of the timeline at zoom level L, where a
// pixel is 2^L nanoseconds, so tile i covers [i, i+1) × TilePx × 2^L ns from
// the trace's start. A client shows a view of s ns a pixel from the tiles of
// level floor(log2 s), which are at least as detailed as the view, and can
// draw the tiles it has of other levels, scaled, until those come.
const (
	TilePx   = 512
	minLevel = -8 // 1/256 ns a pixel
	maxLevel = 50
)

// TileWindow returns the times a tile covers, relative to the trace start.
func TileWindow(level, index int) (t0, t1 int64) {
	span := TilePx * math.Ldexp(1, level)
	return int64(math.Floor(float64(index) * span)), int64(math.Floor(float64(index+1) * span))
}

// Tile is the response of /api/tile.
type Tile struct {
	Level int   `json:"level"`
	Index int   `json:"index"`
	T0    int64 `json:"t0"`
	T1    int64 `json:"t1"`
	// Rows has the blocks of each row (by index in the trace's rows) that
	// has any in the tile: per lane, a flat list of BlockFields numbers per
	// block.
	Rows []TileRow `json:"rows"`
	// Names are the names the blocks refer to.
	Names map[int32]string `json:"names"`
}

// TileRow is a row's blocks in a tile.
type TileRow struct {
	Row   int       `json:"r"`
	Lanes [][]int64 `json:"l"`
}

// BlockFields is the number of values per block in a tile's lanes: start
// and end (ns from the trace's start; merged blocks are clipped to the
// tile), name, kind, count of events (1: an event of its own), index of
// the (first) event in its lane, and busy time.
const BlockFields = 7

func (s *Server) tile(r *http.Request) (any, error) {
	q := r.URL.Query()
	level, err1 := strconv.Atoi(q.Get("level"))
	index, err2 := strconv.Atoi(q.Get("index"))
	if err1 != nil || err2 != nil || level < minLevel || level > maxLevel || index < 0 || index > 1<<40 {
		return nil, badRequest("level and index must be integers, level from %d to %d", minLevel, maxLevel)
	}
	_, l, err := s.load(r)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(s.tiles.get(l, level, index, "")), nil
}

// MakeTile computes a tile of tr.
func MakeTile(tr *trace.Trace, level, index int) *Tile { return makeTile(tr, level, index, "") }

// makeTile computes a tile of tr with the rows and lanes lanes says (see
// tileLanes): every row and lane if "".
func makeTile(tr *trace.Trace, level, index int, lanes string) *Tile {
	t0, t1 := TileWindow(level, index)
	t := &Tile{Level: level, Index: index, T0: t0, T1: t1, Rows: []TileRow{}, Names: map[int32]string{}}
	if lanes != "" {
		tr.Overview() // the lanes the overview draws
	}
	if t0 > tr.Duration() {
		return t
	}
	nsPerPx := math.Ldexp(1, level)
	a, b := tr.Start+t0, tr.Start+t1
	var blocks []trace.Block
	for ri, tk := range tr.Tracks {
		var row *TileRow
		n := tileLanes(tk, lanes)
		for li, lane := range tk.Lanes[:n] {
			blocks = lane.LOD(a, b, nsPerPx, 1, 5, blocks[:0])
			if len(blocks) == 0 {
				continue
			}
			if row == nil {
				t.Rows = append(t.Rows, TileRow{Row: ri, Lanes: make([][]int64, n)})
				row = &t.Rows[len(t.Rows)-1]
			}
			flat := make([]int64, 0, len(blocks)*BlockFields)
			for _, bl := range blocks {
				e := &lane.Events[bl.Index]
				start, end := bl.Start, bl.End
				if bl.Merged {
					start, end = max(start, a), min(end, b)
				}
				flat = append(flat, start-tr.Start, end-tr.Start, int64(e.Name), int64(e.Kind), int64(bl.Count), int64(bl.Index), bl.Busy)
				if !bl.Merged {
					t.Names[e.Name] = tr.Names[e.Name]
				} else if _, ok := t.Names[e.Name]; !ok {
					t.Names[e.Name] = tr.Names[e.Name] // for the color of the block
				}
			}
			row.Lanes[li] = flat
		}
	}
	return t
}

func kindByName(name string) ([]trace.Kind, error) {
	switch name {
	case "kernel", "":
		return []trace.Kind{trace.KindKernel}, nil
	case "api":
		return []trace.Kind{trace.KindRuntime}, nil
	case "nvtx":
		return []trace.Kind{trace.KindNVTX}, nil
	case "cpu":
		return []trace.Kind{trace.KindCPUOp}, nil
	case "memory":
		return []trace.Kind{trace.KindMemcpy, trace.KindMemset}, nil
	case "sync":
		return []trace.Kind{trace.KindSync}, nil
	}
	return nil, badRequest("kind must be kernel, api, nvtx, cpu, memory or sync")
}

type statJSON struct {
	Name  string  `json:"name"`
	Count int     `json:"count"`
	Total int64   `json:"total"`
	Avg   int64   `json:"avg"`
	Min   int64   `json:"min"`
	Max   int64   `json:"max"`
	Share float64 `json:"share"`
}

func (s *Server) summary(r *http.Request) (any, error) {
	kinds, err := kindByName(r.URL.Query().Get("kind"))
	if err != nil {
		return nil, err
	}
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if n, err = strconv.Atoi(v); err != nil || n < 1 || n > 10000 {
			return nil, badRequest("n must be from 1 to 10000")
		}
	}
	// from and to (ns from the trace's start, as tiles' times) sum over that
	// range only, by each event's overlap with it (trace.TopInRange).
	q := r.URL.Query()
	ranged := q.Has("from") || q.Has("to")
	var from, to int64
	if ranged {
		f, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
		t, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
		if err1 != nil || err2 != nil || t < f {
			return nil, badRequest("from and to must be integers (ns from the trace's start), from <= to")
		}
		from, to = f, t
	}
	_, l, err := s.load(r)
	if err != nil {
		return nil, err
	}
	key := summaryKey{q.Get("kind"), n, ranged, from, to}
	if out, ok := l.summaries.get(key); ok {
		return out, nil
	}
	var stats []trace.NameStat
	if ranged {
		stats = l.tr.TopInRange(n, l.tr.Start+from, l.tr.Start+to, kinds...)
	} else {
		stats = l.tr.TopByTotal(n, kinds...)
	}
	out := make([]statJSON, len(stats))
	for i, st := range stats {
		out[i] = statJSON{st.Name, st.Count, st.Total, st.Avg(), st.Min, st.Max, st.Share}
	}
	l.summaries.put(key, out)
	return out, nil
}

type eventJSON struct {
	Row    int        `json:"row"`
	Lane   int        `json:"lane"`
	Index  int        `json:"index"`
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`
	KindID int        `json:"kindId"`
	Start  int64      `json:"start"`
	End    int64      `json:"end"`
	Track  string     `json:"track"`
	Group  string     `json:"group"`
	Corr   int32      `json:"corr,omitempty"`
	Kernel any        `json:"kernel,omitempty"`
	MemOp  any        `json:"memop,omitempty"`
	Linked *eventJSON `json:"linked,omitempty"`
	// Stack is the Python stack it ran under, the outermost frame first
	// (trace.PythonStack); Python says the trace has Python calls at all
	// (so an empty Stack means none here, not none recorded).
	Stack  []frameJSON `json:"stack,omitempty"`
	Python bool        `json:"python,omitempty"`
	// Ops are the operators and annotations it ran under on its thread,
	// the outermost first (trace.OperatorStack).
	Ops []frameJSON `json:"ops,omitempty"`
}

// frameJSON is a frame of a Python stack: its function and its call.
type frameJSON struct {
	Name  string `json:"name"`
	Row   int    `json:"row"`
	Lane  int    `json:"lane"`
	Index int    `json:"index"`
}

func eventInfo(tr *trace.Trace, ref trace.Ref) *eventJSON {
	e := tr.Event(ref)
	tk := tr.Tracks[ref.Track]
	j := &eventJSON{
		Row: int(ref.Track), Lane: int(ref.Lane), Index: int(ref.Index),
		Name: tr.Name(e), Kind: e.Kind.String(), KindID: int(e.Kind),
		Start: e.Start - tr.Start, End: e.End - tr.Start, Track: tk.Name, Group: tk.Group, Corr: e.Corr,
	}
	switch e.Kind {
	case trace.KindKernel:
		k := tr.Kernels[e.Extra]
		j.Kernel = map[string]any{
			"grid": [3]int32{k.GridX, k.GridY, k.GridZ}, "block": [3]int32{k.BlockX, k.BlockY, k.BlockZ},
			"registers": k.Registers, "staticShared": k.StaticShared, "dynamicShared": k.DynamicShared,
			"fullName": tr.Names[k.FullName],
		}
	case trace.KindMemcpy, trace.KindMemset:
		m := tr.MemOps[e.Extra]
		j.MemOp = map[string]any{"bytes": m.Bytes, "copyKind": m.CopyKind, "value": m.Value}
	}
	return j
}

func (s *Server) event(r *http.Request) (any, error) {
	q := r.URL.Query()
	row, err1 := strconv.Atoi(q.Get("row"))
	lane, err2 := strconv.Atoi(q.Get("lane"))
	index, err3 := strconv.Atoi(q.Get("index"))
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, badRequest("row, lane and index must be integers")
	}
	_, l, err := s.load(r)
	if err != nil {
		return nil, err
	}
	tr := l.tr
	if row < 0 || row >= len(tr.Tracks) || lane < 0 || lane >= len(tr.Tracks[row].Lanes) ||
		index < 0 || index >= tr.Tracks[row].Lanes[lane].Len() {
		return nil, httpErr{http.StatusNotFound, "no such event"}
	}
	ref := trace.Ref{Track: int32(row), Lane: int32(lane), Index: int32(index)}
	j := eventInfo(tr, ref)
	if tr.Counts[trace.KindPython] > 0 {
		j.Python = true
		for _, f := range tr.PythonStack(ref) {
			j.Stack = append(j.Stack, frameJSON{f.Name, int(f.Ref.Track), int(f.Ref.Lane), int(f.Ref.Index)})
		}
	}
	if tr.Counts[trace.KindCPUOp] > 0 {
		for _, f := range tr.OperatorStack(ref) {
			j.Ops = append(j.Ops, frameJSON{f.Name, int(f.Ref.Track), int(f.Ref.Lane), int(f.Ref.Index)})
		}
	}
	if other, ok := tr.Correlated(ref); ok {
		j.Linked = eventInfo(tr, other)
	}
	return j, nil
}

// snapshot sends the whole parsed trace, compactly, for the desktop app.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	_, l, err := s.load(r)
	if err != nil {
		httpError(w, statusOf(err), err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	out := compress(w, r)
	if err := l.tr.WriteSnapshot(out); err != nil && s.cfg.Log != nil {
		s.cfg.Log.Printf("snapshot: %v", err)
	}
	out.Close()
}
