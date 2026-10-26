package results

import (
	"os"
	"sync"
	"time"
)

// maxEntries bounds memory: a full run is tens of KB trimmed, so this is tens of MB at most.
const maxEntries = 4000

// ttlCache is an in-memory cache with per-entry expiry. Nothing is persisted: a restart re-reads,
// and Results stays the system of record.
type ttlCache struct {
	mu sync.Mutex
	m  map[string]ttlEntry
}

type ttlEntry struct {
	v   any
	exp time.Time
}

func (c *ttlCache) get(k string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		return nil, false
	}
	return e.v, true
}

func (c *ttlCache) set(k string, v any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ttlEntry{}
	}
	if len(c.m) >= maxEntries {
		now := time.Now()
		for key, e := range c.m {
			if now.After(e.exp) {
				delete(c.m, key)
			}
		}
		if len(c.m) >= maxEntries {
			c.m = map[string]ttlEntry{} // still full of live entries: start over rather than grow
		}
	}
	c.m[k] = ttlEntry{v, time.Now().Add(ttl)}
}

var readFile = func(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
