package server

import (
	"os"
	"sync"
	"time"

	"tracelight/internal/trace"
)

// cache keeps the most recently used traces loaded. Requests for a trace
// that is loading wait for that load rather than starting another; a file
// changed on disk since it loaded is loaded again.
type cache struct {
	opts trace.OpenOptions
	max  int

	mu      sync.Mutex
	entries map[string]*loaded
	// loads bounds the traces loading at once: each load reads with several
	// connections already, and a session of many asks for all at once.
	loads chan struct{}
}

type loaded struct {
	done chan struct{} // closed once tr or err is set
	tr   *trace.Trace
	err  error
	took time.Duration

	mod  time.Time
	size int64
	used time.Time

	// summaries keeps the summaries asked for lately, by kind, n and range:
	// switching tabs, or between ranges, asks for the same ones again.
	summaries summaryCache

	// coll caches the trace's collective kernels' ends (Trace.Collectives).
	collOnce sync.Once
	coll     map[string][]int64
}

// collectives returns the ends of the trace's collective kernels by
// operation (for aligning ranks), computed once.
func (l *loaded) collectives() map[string][]int64 {
	l.collOnce.Do(func() { l.coll = l.tr.Collectives() })
	return l.coll
}

type summaryKey struct {
	kind     string
	n        int
	ranged   bool
	from, to int64
}

// summaryCache is a small cache of summaries, emptied when full.
type summaryCache struct {
	mu sync.Mutex
	m  map[summaryKey][]statJSON
}

const maxSummaries = 64

func (c *summaryCache) get(k summaryKey) ([]statJSON, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *summaryCache) put(k summaryKey, v []statJSON) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxSummaries {
		c.m = map[summaryKey][]statJSON{}
	}
	c.m[k] = v
}

func newCache(opts trace.OpenOptions, max int) *cache {
	return &cache{opts: opts, max: max, entries: map[string]*loaded{}, loads: make(chan struct{}, 4)}
}

// get returns the trace at the real path p, loading it if needed.
func (c *cache) get(p string) (*loaded, error) {
	file, _, _ := trace.SplitMember(p) // a rank of a merged file: the file's
	fi, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	l := c.entries[p]
	if l != nil && (!l.mod.Equal(fi.ModTime()) || l.size != fi.Size()) {
		select {
		case <-l.done: // finished: replace it
			delete(c.entries, p)
			l = nil
		default: // still loading the old version: let it finish
		}
	}
	if l == nil {
		l = &loaded{done: make(chan struct{}), mod: fi.ModTime(), size: fi.Size()}
		c.entries[p] = l
		go func() {
			c.loads <- struct{}{}
			defer func() { <-c.loads }()
			start := time.Now()
			tr, err := trace.OpenWith(p, c.opts)
			l.tr, l.err, l.took = tr, err, time.Since(start)
			close(l.done)
			if err != nil {
				c.mu.Lock()
				if c.entries[p] == l {
					delete(c.entries, p) // let a later request try again
				}
				c.mu.Unlock()
			}
		}()
	}
	l.used = time.Now()
	c.evictLocked()
	c.mu.Unlock()

	<-l.done
	if l.err != nil {
		return nil, l.err
	}
	return l, nil
}

// evictLocked drops the least recently used loaded traces beyond max.
func (c *cache) evictLocked() {
	for len(c.entries) > c.max {
		var oldest string
		var at time.Time
		for p, l := range c.entries {
			select {
			case <-l.done:
			default:
				continue // loading
			}
			if oldest == "" || l.used.Before(at) {
				oldest, at = p, l.used
			}
		}
		if oldest == "" {
			return
		}
		delete(c.entries, oldest)
	}
}
