package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"tracelight/internal/trace"
)

// Info is what a server says of itself.
type Info struct {
	Version int      `json:"version"`
	Root    string   `json:"root"`
	Auth    bool     `json:"auth"`
	Notes   []string `json:"notes"`
}

func (c *Client) getJSON(endpoint string, params url.Values, v any) error {
	res, err := c.get(endpoint, params)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(v)
}

// Info asks the server about itself, which checks that it answers and
// takes the token.
func (c *Client) Info() (Info, error) {
	var i Info
	if err := c.getJSON("info", nil, &i); err != nil {
		return Info{}, err
	}
	// /api/info answers without a token: ask for something that needs one.
	if i.Auth {
		if _, _, err := c.List(""); err != nil {
			return Info{}, err
		}
	}
	return i, nil
}

// Row is a row of a trace's timeline, as /api/trace lists them.
type Row struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Device int32  `json:"device"`
	Count  int    `json:"count"`
	Busy   int64  `json:"busy"`
	Lanes  int    `json:"lanes"`
	// Overview is the rank overview's sub-lane of each lane it draws.
	Overview []int `json:"overview"`
}

// Meta is a trace's metadata: its rows and counts, without events.
type Meta struct {
	Path         string `json:"path"`
	Abs          string `json:"abs"`
	Size         int64  `json:"size"`
	Start        int64  `json:"start"`
	SessionStart int64  `json:"sessionStart"`
	SessionMono  int64  `json:"sessionMono"`
	// Clock is trace.Trace.Clock.
	Clock string `json:"clock"`
	// Collectives are the ends of its collective kernels by operation, in
	// ns from its first event (trace.Collectives), for aligning ranks.
	Collectives map[string][]int64 `json:"collectives"`
	Host        string             `json:"host"`
	Duration    int64              `json:"duration"`
	Events      int                `json:"events"`
	Counts      map[string]int     `json:"counts"`
	Devices     map[string]string  `json:"devices"`
	Rows        []Row              `json:"rows"`
	Missing     []string           `json:"missing"`
	Warnings    []string           `json:"warnings"`
	LoadMs      int64              `json:"loadMs"`
}

// Meta fetches the metadata of the trace at path (which parses it on the
// server, once).
func (c *Client) Meta(path string) (*Meta, error) {
	var m Meta
	if err := c.getJSON("trace", url.Values{"path": {path}}, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Skeleton is a trace of m's rows and metadata, with no events: what the
// timeline lays out while tiles bring what to draw. Its times are relative
// (Start is 0).
func (m *Meta) Skeleton(path string) *trace.Trace {
	tr := &trace.Trace{
		Path: path, Names: []string{""}, End: m.Duration,
		Devices: map[int32]string{}, Counts: map[trace.Kind]int{},
		SessionStartUTC: m.SessionStart, SessionStartMono: m.SessionMono, Host: m.Host, Clock: m.Clock,
		Missing: m.Missing, Warnings: m.Warnings,
	}
	for k, v := range m.Devices {
		if id, err := strconv.Atoi(k); err == nil {
			tr.Devices[int32(id)] = v
		}
	}
	for k := trace.KindKernel; k <= trace.LastKind; k++ {
		if n := m.Counts[k.String()]; n > 0 {
			tr.Counts[k] = n
		}
	}
	for _, r := range m.Rows {
		tk := &trace.Track{Group: r.Group, Name: r.Name, Count: r.Count, Busy: r.Busy, Device: r.Device, Overview: r.Overview}
		switch r.Kind {
		case "stream":
			tk.Kind = trace.TrackStream
		case "api":
			tk.Kind = trace.TrackRuntime
		case "nvtx":
			tk.Kind = trace.TrackNVTX
		case "cpu":
			tk.Kind = trace.TrackCPU
		case "python":
			tk.Kind = trace.TrackPython
		}
		tk.Lanes = make([]*trace.Lane, r.Lanes)
		for i := range tk.Lanes {
			tk.Lanes[i] = &trace.Lane{}
		}
		tr.Tracks = append(tr.Tracks, tk)
	}
	return tr
}

// Tile is a tile of a trace's timeline (see the server's Tile): per row
// and lane, blocks of BlockFields numbers.
type Tile struct {
	Level int   `json:"level"`
	Index int   `json:"index"`
	T0    int64 `json:"t0"`
	T1    int64 `json:"t1"`
	Rows  []struct {
		Row   int       `json:"r"`
		Lanes [][]int64 `json:"l"`
	} `json:"rows"`
	Names map[int32]string `json:"names"`
}

// BlockFields is the number of values of a block in a tile: start, end
// (ns from the trace's start), name, kind, count, index, busy.
const BlockFields = 7

// TilePx is the width of a tile in pixels: tile i of level L covers
// [i, i+1) × TilePx × 2^L ns.
const TilePx = 512

// Tile fetches tile index of level of the trace at path.
func (c *Client) Tile(path string, level, index int) (*Tile, error) {
	var t Tile
	err := c.getJSON("tile", url.Values{"path": {path}, "level": {strconv.Itoa(level)}, "index": {strconv.Itoa(index)}}, &t)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Event is an event's details, as /api/event gives them.
type Event struct {
	Row    int    `json:"row"`
	Lane   int    `json:"lane"`
	Index  int    `json:"index"`
	Name   string `json:"name"`
	KindID int    `json:"kindId"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Track  string `json:"track"`
	Group  string `json:"group"`
	Corr   int32  `json:"corr"`
	Kernel *struct {
		Grid          [3]int32 `json:"grid"`
		Block         [3]int32 `json:"block"`
		Registers     int32    `json:"registers"`
		StaticShared  int32    `json:"staticShared"`
		DynamicShared int32    `json:"dynamicShared"`
		FullName      string   `json:"fullName"`
	} `json:"kernel"`
	MemOp *struct {
		Bytes    int64 `json:"bytes"`
		CopyKind int32 `json:"copyKind"`
		Value    int32 `json:"value"`
	} `json:"memop"`
	Linked *Event `json:"linked"`
	// Stack is the Python stack it ran under, the outermost frame first;
	// Python says the trace has Python calls at all.
	Stack []struct {
		Name  string `json:"name"`
		Row   int    `json:"row"`
		Lane  int    `json:"lane"`
		Index int    `json:"index"`
	} `json:"stack"`
	Python bool `json:"python"`
	// Ops are the operators and annotations it ran under, the outermost
	// first.
	Ops []struct {
		Name  string `json:"name"`
		Row   int    `json:"row"`
		Lane  int    `json:"lane"`
		Index int    `json:"index"`
	} `json:"ops"`
}

// Event fetches the details of an event, and of the one its correlation id
// links it to.
func (c *Client) Event(path string, r trace.Ref) (*Event, error) {
	var e Event
	err := c.getJSON("event", url.Values{"path": {path}, "row": {strconv.Itoa(int(r.Track))},
		"lane": {strconv.Itoa(int(r.Lane))}, "index": {strconv.Itoa(int(r.Index))}}, &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Summary fetches the top n names of a kind ("kernel", "api", "nvtx", "cpu") by
// total time; ranged, over [from, to) only (ns from the trace's start), by
// each event's overlap with it.
func (c *Client) Summary(path, kind string, n int, ranged bool, from, to int64) ([]trace.NameStat, error) {
	q := url.Values{"path": {path}, "kind": {kind}, "n": {strconv.Itoa(n)}}
	if ranged {
		q.Set("from", strconv.FormatInt(from, 10))
		q.Set("to", strconv.FormatInt(to, 10))
	}
	var out []struct {
		Name  string  `json:"name"`
		Count int     `json:"count"`
		Total int64   `json:"total"`
		Min   int64   `json:"min"`
		Max   int64   `json:"max"`
		Share float64 `json:"share"`
	}
	if err := c.getJSON("summary", q, &out); err != nil {
		return nil, err
	}
	stats := make([]trace.NameStat, len(out))
	for i, s := range out {
		stats[i] = trace.NameStat{Name: s.Name, Count: s.Count, Total: s.Total, Min: s.Min, Max: s.Max, Share: s.Share}
	}
	return stats, nil
}

// SessionTrace is a trace that paths name, as /api/session lists them.
type SessionTrace struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	Ready        bool   `json:"ready"`
	Note         string `json:"note"`
	SessionStart int64  `json:"sessionStart"`
	SessionMono  int64  `json:"sessionMono"`
	Host         string `json:"host"`
	Rank         int    `json:"rank"`
}

// Session lists the traces that paths name: files, directories (every
// trace in them) and globs, absolute or relative to the server's root.
func (c *Client) Session(paths []string) ([]SessionTrace, []string, error) {
	var out struct {
		Traces   []SessionTrace `json:"traces"`
		Warnings []string       `json:"warnings"`
	}
	if err := c.getJSON("session", url.Values{"path": paths}, &out); err != nil {
		return nil, nil, err
	}
	return out.Traces, out.Warnings, nil
}

// LoadProgress is how far the server's load of a trace has got: Phase as
// trace.Phase's names ("queued", "reading", "done", ...), Fraction from 0
// to 1.
type LoadProgress struct {
	Phase    string  `json:"phase"`
	Fraction float64 `json:"fraction"`
	Error    string  `json:"error"`
}

// Progress asks how far the server's loads of the traces at paths (as
// Session lists them, or a merged file it is listing) have got, in their
// order.
func (c *Client) Progress(paths []string) ([]LoadProgress, error) {
	var out struct {
		Files []LoadProgress `json:"files"`
	}
	if err := c.getJSON("progress", url.Values{"path": paths}, &out); err != nil {
		return nil, err
	}
	if len(out.Files) != len(paths) {
		return nil, fmt.Errorf("progress: %d answers for %d paths", len(out.Files), len(paths))
	}
	return out.Files, nil
}

// TileReq asks for a tile of the trace at index P of a batch's paths.
type TileReq struct{ P, Level, Index int }

// Tiles fetches tiles of several traces in one request (/api/tiles): reqs
// index paths; lanes, if not "", keeps the rows and lanes of a rank
// overview ("gpu,nvtx": see the server). The tiles come in reqs' order.
func (c *Client) Tiles(paths []string, reqs []TileReq, lanes string) ([]*Tile, error) {
	q := url.Values{"path": paths}
	for _, r := range reqs {
		q.Add("t", strconv.Itoa(r.P)+":"+strconv.Itoa(r.Level)+":"+strconv.Itoa(r.Index))
	}
	if lanes != "" {
		q.Set("lanes", lanes)
	}
	var out struct {
		Tiles []struct {
			P    int   `json:"p"`
			Tile *Tile `json:"tile"`
		} `json:"tiles"`
	}
	if err := c.getJSON("tiles", q, &out); err != nil {
		return nil, err
	}
	if len(out.Tiles) != len(reqs) {
		return nil, errors.New("the server sent another number of tiles than asked for")
	}
	tiles := make([]*Tile, len(reqs))
	for i, t := range out.Tiles {
		tiles[i] = t.Tile
	}
	return tiles, nil
}
