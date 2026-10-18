package web

import (
	"sync"
	"time"
)

// pollEvery is how often an event stream checks presence and transcription for a change. A var for
// tests.
var pollEvery = time.Second

// cacheIdle is how long an entry nobody reads stays.
const cacheIdle = time.Minute

// podCache keeps, by key, what a load read for a little less than pollEvery of real time: a pod's
// streams poll the database once per period, not each on its own (spec 017, AC14). A local write
// drops its key.
type podCache[T any] struct {
	mu    sync.Mutex
	m     map[string]*cacheEntry[T]
	swept time.Time
}

type cacheEntry[T any] struct {
	// mu makes the streams that find the entry stale wait for one load.
	mu       sync.Mutex
	loadedAt time.Time
	v        T
	// used is guarded by the cache's mu.
	used time.Time
}

// get returns key's value, loading it once its copy is older than cacheFresh. A failed load keeps
// the copy it had.
func (c *podCache[T]) get(key string, load func() (T, error)) T {
	now := time.Now()
	c.mu.Lock()
	if now.Sub(c.swept) > cacheIdle {
		for k, e := range c.m {
			if now.Sub(e.used) > cacheIdle {
				delete(c.m, k)
			}
		}
		c.swept = now
	}
	if c.m == nil {
		c.m = map[string]*cacheEntry[T]{}
	}
	e := c.m[key]
	if e == nil {
		e = &cacheEntry[T]{}
		c.m[key] = e
	}
	e.used = now
	c.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.loadedAt.IsZero() && time.Since(e.loadedAt) < cacheFresh() {
		return e.v
	}
	if v, err := load(); err == nil {
		e.v, e.loadedAt = v, time.Now()
	}
	return e.v
}

// cacheFresh is shorter than the poll tick: a tick that fires a hair early still reloads, or a
// change would wait one more tick.
func cacheFresh() time.Duration { return pollEvery * 9 / 10 }

// drop forgets key's copy: the next get loads it.
func (c *podCache[T]) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}
