package viewer

import (
	"math"
	"slices"
	"sync"
	"time"

	"tracelight/internal/remote"
	"tracelight/internal/trace"
)

// Streaming a trace from a server, as the web client does: the timeline
// lays out the rows from the trace's metadata at once, then draws from
// tiles fetched for what it shows. A tile is remote.TilePx pixels of
// timeline at zoom level L (2^L ns a pixel) with the server's per-pixel
// LOD blocks of every lane; the view at s ns a pixel draws level
// floor(log2 s), as detailed as the screen or more. What a frame misses is
// fetched in batches (/api/tiles) shared by every trace of the session
// (tileBatcher): a coarse tile over a slot with nothing to show first, then
// the view's tiles nearest the middle, then the neighbors and the coarser
// level for pans and zooms; a trace whose rows do not show asks for
// nothing. Meanwhile the tiles at hand of coarser (or finer, two levels)
// levels stand in, clipped. In the rank overview, tiles hold only the rows
// and lanes it draws (lanes); a tile of every row stands in for those.
// Tiles stay cached, the least recently drawn dropped first.

const (
	minTileLevel   = -8
	maxTileLevel   = 50
	maxCachedTiles = 600
	// minBackoff and maxBackoff bound the wait before tiles whose request
	// failed are asked for again.
	minBackoff = 250 * time.Millisecond
	maxBackoff = 5 * time.Second
	tileBatch  = 16 // tiles a request
	maxBatches = 8  // requests at once
)

// tileFetcher is what remoteSource needs of a server (remote.Client).
type tileFetcher interface {
	Tiles(paths []string, reqs []remote.TileReq, lanes string) ([]*remote.Tile, error)
	Event(path string, r trace.Ref) (*remote.Event, error)
	Summary(path, kind string, n int, ranged bool, from, to int64) ([]trace.NameStat, error)
}

type tileKey struct {
	level, index int
	lanes        string // the lanes it holds (tileLanes); "" for every row
}

// rtile is a tile, fetched or on its way.
type rtile struct {
	key    tileKey
	t0, t1 int64
	// lanes holds the blocks of each row and lane that has any.
	lanes map[int][][]vblock
	ready bool
	used  int64 // the frame it was last drawn in
}

// tileUse is a tile drawn in a frame, clipped to [lo, hi) where it stands
// in for a missing one.
type tileUse struct {
	t      *rtile
	lo, hi int64
	clip   bool
}

type tileWant struct {
	key  tileKey
	prio float64
}

// remoteSource streams one trace from a server.
type remoteSource struct {
	api   tileFetcher
	batch *tileBatcher
	path  string
	dur   int64
	// wake asks for a frame, from any goroutine, when something arrived.
	wake func()
	// lanes are the lanes the tiles it asks for hold (App.tileLanes).
	lanes string

	mu       sync.Mutex
	tiles    map[tileKey]*rtile
	want     []tileWant // what the last frame asked for, nearest first
	inflight int
	frameNo  int64
	use      []tileUse
	// level is the level the last frame drew; complete whether it had
	// every tile of it.
	level    int
	complete bool
	errs     int
	// After a failed request, its tiles are asked for again from retryAt
	// on, backoff later each time (to maxBackoff), not at once: a server
	// that is down, or a gateway that wants a sign-in, is not flooded.
	retryAt time.Time
	backoff time.Duration
	// slots counts the last frame's tiles of its level: there, stood in
	// for by a coarser or finer one, or missing (nothing drawn there).
	slots [3]int

	events  map[trace.Ref]*eventInfo
	evWait  map[trace.Ref]bool
	tops    map[topKey][]trace.NameStat
	topWait map[topKey]bool
}

// newRemoteSource streams the trace at path; b, if not nil, is the batcher
// of the session's traces (else it gets one of its own).
func newRemoteSource(api tileFetcher, b *tileBatcher, path string, dur int64, wake func()) *remoteSource {
	if b == nil {
		b = newTileBatcher(api, wake)
	}
	rs := &remoteSource{api: api, batch: b, path: path, dur: dur, wake: wake, tiles: map[tileKey]*rtile{},
		events: map[trace.Ref]*eventInfo{}, evWait: map[trace.Ref]bool{},
		tops: map[topKey][]trace.NameStat{}, topWait: map[topKey]bool{}}
	b.add(rs)
	return rs
}

// setLanes sets the lanes of the tiles it asks for from now on.
func (rs *remoteSource) setLanes(lanes string) {
	rs.mu.Lock()
	rs.lanes = lanes
	rs.mu.Unlock()
}

func tileSpan(level int) float64 { return remote.TilePx * math.Ldexp(1, level) }

func levelFor(scale float64) int {
	return min(max(int(math.Floor(math.Log2(scale))), minTileLevel), maxTileLevel)
}

func (rs *remoteSource) notify() {
	if rs.wake != nil {
		rs.wake()
	}
}

// frame picks the tiles to draw [t0, t1) at scale with, asks for those
// missing, and drops the least recently drawn beyond the cache's size.
func (rs *remoteSource) frame(t0, t1, scale float64) {
	rs.mu.Lock()
	rs.frameLocked(t0, t1, scale)
	rs.mu.Unlock()
	rs.batch.kick()
}

func (rs *remoteSource) frameLocked(t0, t1, scale float64) {
	rs.frameNo++
	rs.use = rs.use[:0]
	rs.want = rs.want[:0]
	level := levelFor(scale)
	span := tileSpan(level)
	t0, t1 = max(t0, 0), min(t1, float64(rs.dur)+1)
	rs.level, rs.complete = level, true
	rs.slots = [3]int{}
	if t1 <= t0 {
		return
	}
	first, last := int(math.Floor(t0/span)), int(math.Floor(t1/span))
	mid := (t0 + t1) / 2
	for i := first; i <= last; i++ {
		lo, hi := int64(float64(i)*span), int64(float64(i+1)*span)
		if t := rs.ready(level, i); t != nil {
			rs.use = append(rs.use, tileUse{t: t})
			rs.slots[0]++
			continue
		}
		rs.complete = false
		rs.ask(level, i, math.Abs((float64(i)+0.5)*span-mid)/span)
		// Stand-ins: the nearest coarser level at hand, or the finer ones,
		// one level, then two (from a zoom out).
		found := false
		for up := 1; up <= 10 && level+up <= maxTileLevel; up++ {
			l := level + up
			if t := rs.ready(l, int(math.Floor(float64(lo)/tileSpan(l)))); t != nil {
				rs.use = append(rs.use, tileUse{t: t, lo: lo, hi: hi, clip: true})
				found = true
				break
			}
		}
		for down := 1; down <= 2 && !found && level-down >= minTileLevel; down++ {
			n := 1 << down
			for j := n * i; j < n*(i+1); j++ {
				if t := rs.ready(level-down, j); t != nil {
					rs.use = append(rs.use, tileUse{t: t, lo: lo, hi: hi, clip: true})
					found = true
				}
			}
		}
		if found {
			rs.slots[1]++
		} else {
			// Nothing to show here: a coarse tile over it first, fast to come.
			rs.slots[2]++
			if l := min(level+3, maxTileLevel); l > level {
				rs.ask(l, int(math.Floor(float64(lo)/tileSpan(l))), -1)
			}
		}
	}
	// Prefetch: a tile each side, and the coarser level, for pans and
	// zooms out; then the finer level around the middle, for zooms in.
	rs.ask(level, first-1, 50)
	rs.ask(level, last+1, 50)
	if level < maxTileLevel {
		s2 := tileSpan(level + 1)
		for i := int(math.Floor(t0 / s2)); i <= int(math.Floor(t1/s2)); i++ {
			rs.ask(level+1, i, 60)
		}
	}
	if level > minTileLevel {
		rs.ask(level-1, int(math.Floor(mid/tileSpan(level-1))), 70)
	}
	slices.SortStableFunc(rs.want, func(a, b tileWant) int {
		switch {
		case a.prio < b.prio:
			return -1
		case a.prio > b.prio:
			return 1
		}
		return 0
	})
	rs.evict()
}

// ready returns the tile of level and index if it has come, of the lanes
// asked for or of every row, marking it drawn.
func (rs *remoteSource) ready(level, index int) *rtile {
	t := rs.tiles[tileKey{level, index, rs.lanes}]
	if (t == nil || !t.ready) && rs.lanes != "" {
		t = rs.tiles[tileKey{level, index, ""}]
	}
	if t == nil || !t.ready {
		return nil
	}
	t.used = rs.frameNo
	return t
}

// ask wants the tile of level and index fetched, unless it is out of the
// trace, here (of the lanes asked for or of every row), or on its way.
func (rs *remoteSource) ask(level, index int, prio float64) {
	if index < 0 || float64(index)*tileSpan(level) > float64(rs.dur) {
		return
	}
	k := tileKey{level, index, rs.lanes}
	if _, ok := rs.tiles[k]; ok {
		return
	}
	if t := rs.tiles[tileKey{level, index, ""}]; rs.lanes != "" && t != nil && t.ready {
		return
	}
	rs.want = append(rs.want, tileWant{k, prio})
}

// keep drops all but its n most recently drawn tiles (a tab hidden).
func (rs *remoteSource) keep(n int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var ready []*rtile
	for _, t := range rs.tiles {
		if t.ready {
			ready = append(ready, t)
		}
	}
	slices.SortFunc(ready, func(a, b *rtile) int { return int(b.used - a.used) })
	for _, t := range ready[min(n, len(ready)):] {
		delete(rs.tiles, t.key)
	}
}

func (rs *remoteSource) evict() {
	if len(rs.tiles) <= maxCachedTiles {
		return
	}
	var old []*rtile
	for _, t := range rs.tiles {
		if t.ready && t.used < rs.frameNo {
			old = append(old, t)
		}
	}
	slices.SortFunc(old, func(a, b *rtile) int { return int(a.used - b.used) })
	for _, t := range old[:max(0, min(len(old), len(rs.tiles)-maxCachedTiles))] {
		delete(rs.tiles, t.key)
	}
}

// tileBatcher fetches the tiles the traces of a session want, in batches:
// up to tileBatch tiles of any of them a request, the most wanted first,
// maxBatches requests at once. Locks: the batcher's, then a source's; a
// source's lock is never held while taking the batcher's.
type tileBatcher struct {
	api      tileFetcher
	wake     func()
	mu       sync.Mutex
	sources  []*remoteSource
	inflight int
}

func newTileBatcher(api tileFetcher, wake func()) *tileBatcher {
	return &tileBatcher{api: api, wake: wake}
}

func (b *tileBatcher) add(rs *remoteSource) {
	b.mu.Lock()
	b.sources = append(b.sources, rs)
	b.mu.Unlock()
}

type batchItem struct {
	rs   *remoteSource
	t    *rtile
	prio float64
}

// kick starts requests for what the sources want, while it may.
func (b *tileBatcher) kick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.inflight < maxBatches {
		items := b.take()
		if len(items) == 0 {
			return
		}
		b.inflight++
		go b.fetch(items)
	}
}

// take picks the next batch: the sources' wants by priority, of the lanes
// of the most wanted, each marked as on its way.
func (b *tileBatcher) take() []batchItem {
	var all []batchItem
	now := time.Now()
	for _, rs := range b.sources {
		rs.mu.Lock()
		if now.Before(rs.retryAt) {
			rs.mu.Unlock()
			continue
		}
		for _, w := range rs.want {
			if _, ok := rs.tiles[w.key]; !ok {
				all = append(all, batchItem{rs: rs, t: &rtile{key: w.key}, prio: w.prio})
			}
		}
		rs.mu.Unlock()
	}
	if len(all) == 0 {
		return nil
	}
	slices.SortStableFunc(all, func(x, y batchItem) int {
		switch {
		case x.prio < y.prio:
			return -1
		case x.prio > y.prio:
			return 1
		}
		return 0
	})
	lanes := all[0].t.key.lanes
	var out []batchItem
	for _, it := range all {
		if len(out) == tileBatch {
			break
		}
		if it.t.key.lanes != lanes {
			continue
		}
		it.rs.mu.Lock()
		if _, ok := it.rs.tiles[it.t.key]; !ok {
			it.rs.tiles[it.t.key] = it.t
			it.rs.inflight++
			out = append(out, it)
		}
		it.rs.mu.Unlock()
	}
	return out
}

func (b *tileBatcher) fetch(items []batchItem) {
	var paths []string
	index := map[*remoteSource]int{}
	reqs := make([]remote.TileReq, len(items))
	for i, it := range items {
		p, ok := index[it.rs]
		if !ok {
			p = len(paths)
			index[it.rs] = p
			paths = append(paths, it.rs.path)
		}
		reqs[i] = remote.TileReq{P: p, Level: it.t.key.level, Index: it.t.key.index}
	}
	tiles, err := b.api.Tiles(paths, reqs, items[0].t.key.lanes)
	for i, it := range items {
		var lanes map[int][][]vblock
		if err == nil {
			lanes = decodeTile(tiles[i])
		}
		rs := it.rs
		rs.mu.Lock()
		rs.inflight--
		if err != nil {
			delete(rs.tiles, it.t.key) // a later frame asks again, after the backoff
			rs.errs++
			rs.backoff = min(max(2*rs.backoff, minBackoff), maxBackoff)
			rs.retryAt = time.Now().Add(rs.backoff)
			if b.wake != nil {
				time.AfterFunc(rs.backoff, b.wake)
			}
		} else {
			rs.backoff = 0
			it.t.t0, it.t.t1, it.t.lanes, it.t.ready = tiles[i].T0, tiles[i].T1, lanes, true
			it.t.used = rs.frameNo
		}
		rs.mu.Unlock()
	}
	b.mu.Lock()
	b.inflight--
	b.mu.Unlock()
	if b.wake != nil {
		b.wake()
	}
	b.kick()
}

// decodeTile turns a tile's flat lanes into blocks.
func decodeTile(t *remote.Tile) map[int][][]vblock {
	out := make(map[int][][]vblock, len(t.Rows))
	for _, r := range t.Rows {
		lanes := make([][]vblock, len(r.Lanes))
		for li, flat := range r.Lanes {
			bs := make([]vblock, 0, len(flat)/remote.BlockFields)
			for k := 0; k+remote.BlockFields <= len(flat); k += remote.BlockFields {
				b := vblock{start: flat[k], end: flat[k+1], name: t.Names[int32(flat[k+2])], kind: trace.Kind(flat[k+3]),
					count: int(flat[k+4]), index: int(flat[k+5]), busy: flat[k+6]}
				b.merged = b.count > 1
				bs = append(bs, b)
			}
			lanes[li] = bs
		}
		out[r.Row] = lanes
	}
	return out
}

func (rs *remoteSource) lod(ti, li int, t0, t1, scale float64, out []vblock) []vblock {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	a, b := int64(math.Floor(t0)), int64(math.Ceil(t1))+1
	for _, u := range rs.use {
		lanes := u.t.lanes[ti]
		if li >= len(lanes) {
			continue
		}
		bs := lanes[li]
		lo, hi := a, b
		if u.clip {
			lo, hi = max(lo, u.lo), min(hi, u.hi)
		}
		for i := searchBlocks(bs, lo); i < len(bs) && bs[i].start < hi; i++ {
			bl := bs[i]
			if u.clip && bl.merged {
				bl.start, bl.end = max(bl.start, u.lo), min(bl.end, u.hi)
			}
			out = append(out, bl)
		}
	}
	return out
}

func (rs *remoteSource) hit(ti, li int, t, tol, scale float64) (vblock, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	at := int64(math.Round(t))
	var best vblock
	bestD := tol + 1
	for _, u := range rs.use {
		lanes := u.t.lanes[ti]
		if li >= len(lanes) {
			continue
		}
		bs := lanes[li]
		for i := max(searchBlocks(bs, at-int64(tol))-1, 0); i < len(bs) && float64(bs[i].start) <= t+tol; i++ {
			bl := bs[i]
			if u.clip && (bl.end < u.lo || bl.start >= u.hi) {
				continue
			}
			var d float64
			switch {
			case at < bl.start:
				d = float64(bl.start - at)
			case at > bl.end:
				d = float64(at - bl.end)
			}
			if d < bestD {
				best, bestD = bl, d
			}
		}
	}
	return best, bestD <= tol
}

func toInfo(e *remote.Event) *eventInfo {
	if e == nil {
		return nil
	}
	in := &eventInfo{ref: trace.Ref{Track: int32(e.Row), Lane: int32(e.Lane), Index: int32(e.Index)},
		name: e.Name, kind: trace.Kind(e.KindID), start: e.Start, end: e.End, group: e.Group, track: e.Track, corr: e.Corr}
	if k := e.Kernel; k != nil {
		in.kernel = &trace.KernelInfo{GridX: k.Grid[0], GridY: k.Grid[1], GridZ: k.Grid[2], BlockX: k.Block[0], BlockY: k.Block[1], BlockZ: k.Block[2],
			Registers: k.Registers, StaticShared: k.StaticShared, DynamicShared: k.DynamicShared}
		in.full = k.FullName
	}
	if m := e.MemOp; m != nil {
		in.memop = &trace.MemOpInfo{Bytes: m.Bytes, CopyKind: m.CopyKind, Value: m.Value}
	}
	in.linked = toInfo(e.Linked)
	in.python = e.Python
	for _, f := range e.Stack {
		in.stack = append(in.stack, trace.StackFrame{Name: f.Name, Ref: trace.Ref{Track: int32(f.Row), Lane: int32(f.Lane), Index: int32(f.Index)}})
	}
	for _, f := range e.Ops {
		in.ops = append(in.ops, trace.StackFrame{Name: f.Name, Ref: trace.Ref{Track: int32(f.Row), Lane: int32(f.Lane), Index: int32(f.Index)}})
	}
	return in
}

func (rs *remoteSource) event(r trace.Ref) (*eventInfo, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if in, ok := rs.events[r]; ok {
		return in, true
	}
	if !rs.evWait[r] {
		rs.evWait[r] = true
		go func() {
			e, err := rs.api.Event(rs.path, r)
			rs.mu.Lock()
			delete(rs.evWait, r)
			if err == nil {
				if len(rs.events) > 256 {
					clear(rs.events)
				}
				rs.events[r] = toInfo(e)
			}
			rs.mu.Unlock()
			rs.notify()
		}()
	}
	return nil, false
}

var summaryKinds = map[trace.Kind]string{trace.KindKernel: "kernel", trace.KindRuntime: "api", trace.KindNVTX: "nvtx", trace.KindCPUOp: "cpu"}

func (rs *remoteSource) top(k trace.Kind, ranged bool, from, to int64) ([]trace.NameStat, bool) {
	key := topKey{k, ranged, from, to}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if st, ok := rs.tops[key]; ok {
		return st, true
	}
	if !rs.topWait[key] {
		rs.topWait[key] = true
		go func() {
			st, err := rs.api.Summary(rs.path, summaryKinds[k], 100, ranged, from, to)
			rs.mu.Lock()
			delete(rs.topWait, key)
			if err == nil {
				if len(rs.tops) > 32 {
					clear(rs.tops)
				}
				rs.tops[key] = st
			}
			rs.mu.Unlock()
			rs.notify()
		}()
	}
	return nil, false
}

func (rs *remoteSource) memBytes(r trace.Ref) (int64, bool) { return 0, false }

func (rs *remoteSource) loading() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.inflight + len(rs.want) + len(rs.evWait) + len(rs.topWait)
}

// status says how complete the last frame was: its level, whether every
// tile of it had come, and how many tiles are cached.
func (rs *remoteSource) status() (level int, complete bool, cached int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.level, rs.complete, len(rs.tiles)
}
