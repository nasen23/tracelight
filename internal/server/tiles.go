package server

import (
	"container/list"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"tracelight/internal/theme"
	"tracelight/internal/trace"
)

// Tiles in batches: a client showing many traces (the ranks of a job) asks
// for the tiles of all of them that it misses in one request, rather than
// one round trip each:
//
//	GET /api/tiles?path=A&path=B&t=0:25:3&t=1:25:3&lanes=gpu,nvtx
//
// t is "path index:level:index"; lanes, if given, keeps the rows and lanes
// a rank overview draws (theme.CompactLanes: every lane of the GPU rows,
// the outermost few of the CUDA API and NVTX rows, trace.OverviewLanes),
// which is most of a tile's
// size less. The answer is {"tiles": [{"p": path index, "tile": Tile}]},
// in the order asked. Tiles are computed in parallel and kept, encoded, in
// a cache shared by every client, so that a view seen again, or by
// another client, costs nothing to compute.

const maxBatch = 128

// tileLanes is how many lanes of a row a tile with the lanes lanes keeps:
// all of them for "" (every row), and for the rows of a summary lane asked
// for (theme.CompactLanes' keys: "gpu", "api", ...), those the overview
// draws: every lane of a GPU row, of the others those up to the last the
// overview draws (trace.Track.Overview); none otherwise.
func tileLanes(tk *trace.Track, lanes string) int {
	n := len(tk.Lanes)
	if lanes == "" {
		return n
	}
	kind := trackKind(tk.Kind)
	for _, l := range theme.CompactLanes {
		if l.Kind != kind || !slices.Contains(strings.Split(lanes, ","), l.Key) {
			continue
		}
		if tk.Kind == trace.TrackStream {
			return n
		}
		if tk.Overview != nil {
			return min(n, len(tk.Overview))
		}
		return trace.OverviewLanes(tk.Kind, n)
	}
	return 0
}

// parseLanes reads the lanes parameter: some of theme.CompactLanes' keys,
// sorted, comma-separated; "" for every row.
func parseLanes(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	var out []string
	var keys []string
	for _, l := range theme.CompactLanes {
		keys = append(keys, l.Key)
	}
	for _, k := range strings.Split(v, ",") {
		if !slices.Contains(keys, k) {
			return "", badRequest("lanes takes %s", strings.Join(keys, ", "))
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ","), nil
}

type tileKey struct {
	l            *loaded // a changed file loads anew: its tiles go with it
	level, index int
	lanes        string
}

type tileEntry struct {
	key  tileKey
	json []byte
	done chan struct{} // closed once json is set
}

// tileCache keeps encoded tiles, the least recently used dropped beyond a
// budget of bytes; a tile being computed is computed once, whoever asks.
type tileCache struct {
	mu      sync.Mutex
	budget  int
	size    int
	entries map[tileKey]*list.Element
	lru     *list.List // of *tileEntry, most recent first
	// Computed and served, for tests and the log.
	computed, served int
}

func newTileCache(budget int) *tileCache {
	return &tileCache{budget: budget, entries: map[tileKey]*list.Element{}, lru: list.New()}
}

// get returns the encoded tile, computing it if it is not kept.
func (c *tileCache) get(l *loaded, level, index int, lanes string) []byte {
	k := tileKey{l, level, index, lanes}
	c.mu.Lock()
	c.served++
	if el, ok := c.entries[k]; ok {
		c.lru.MoveToFront(el)
		e := el.Value.(*tileEntry)
		c.mu.Unlock()
		<-e.done
		return e.json
	}
	e := &tileEntry{key: k, done: make(chan struct{})}
	c.entries[k] = c.lru.PushFront(e)
	c.computed++
	c.mu.Unlock()

	b, _ := json.Marshal(makeTile(l.tr, level, index, lanes))
	c.mu.Lock()
	e.json = b
	close(e.done)
	if el, ok := c.entries[k]; ok && el.Value == e {
		c.size += len(b)
	}
	for c.size > c.budget && c.lru.Len() > 1 {
		el := c.lru.Back()
		old := el.Value.(*tileEntry)
		if old.json == nil {
			break // still being computed
		}
		c.lru.Remove(el)
		delete(c.entries, old.key)
		c.size -= len(old.json)
	}
	c.mu.Unlock()
	return b
}

// tileBatch answers /api/tiles.
func (s *Server) tileBatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	paths, reqs := q["path"], q["t"]
	lanes, err := parseLanes(q.Get("lanes"))
	if err == nil && (len(paths) == 0 || len(reqs) == 0 || len(reqs) > maxBatch) {
		err = badRequest("give path, and from 1 to %d t=path index:level:index", maxBatch)
	}
	type req struct{ p, level, index int }
	var rs []req
	for _, t := range reqs {
		if err != nil {
			break
		}
		parts := strings.Split(t, ":")
		var v [3]int
		for i := range v {
			if len(parts) != 3 {
				break
			}
			v[i], err = strconv.Atoi(parts[i])
			if err != nil {
				break
			}
		}
		switch {
		case err != nil || len(parts) != 3:
			err = badRequest("t is path index:level:index, as 0:25:3")
		case v[0] < 0 || v[0] >= len(paths):
			err = badRequest("t=%s: no path %d", t, v[0])
		case v[1] < minLevel || v[1] > maxLevel || v[2] < 0 || v[2] > 1<<40:
			err = badRequest("t=%s: level from %d to %d, index from 0", t, minLevel, maxLevel)
		}
		rs = append(rs, req{v[0], v[1], v[2]})
	}
	if err != nil {
		httpError(w, statusOf(err), err.Error())
		return
	}
	// The traces, loaded in parallel (each loads once, whoever asks).
	ls := make([]*loaded, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Go(func() { _, ls[i], errs[i] = s.loadPath(p) })
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			httpError(w, statusOf(e), fmt.Sprintf("%s: %v", paths[i], e))
			return
		}
	}
	// The tiles, in parallel, from the cache where it has them.
	out := make([][]byte, len(rs))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, q := range rs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = s.tiles.get(ls[q.p], q.level, q.index, lanes)
		}()
	}
	wg.Wait()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	zw := compress(w, r)
	zw.Write([]byte(`{"tiles":[`))
	for i, b := range out {
		if i > 0 {
			zw.Write([]byte(","))
		}
		fmt.Fprintf(zw, `{"p":%d,"tile":`, rs[i].p)
		zw.Write(b)
		zw.Write([]byte("}"))
	}
	zw.Write([]byte("]}"))
	zw.Close()
}
