package results

import (
	"context"
	"sync"
	"time"
)

// fillTimeout bounds a read that fills the cache. It is longer than any caller's deadline on
// purpose: Tower gives up after 10 s, and a cold read of a busy app's window under load can take
// longer. If the read died with the caller, every retry would start cold again and none would
// finish; detached, the first caller's read completes and the next caller finds it cached.
const fillTimeout = 90 * time.Second

type call struct {
	done chan struct{}
	v    any
	err  error
}

// flights collapses concurrent reads of the same key into one.
type flights struct {
	mu sync.Mutex
	m  map[string]*call
}

// do runs fn once per key at a time, detached from ctx (it keeps ctx's values, not its
// cancellation), and waits for it or for ctx, whichever ends first. A caller that gives up leaves
// the read running for whoever asks next.
func (f *flights) do(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*call{}
	}
	c, ok := f.m[key]
	if !ok {
		c = &call{done: make(chan struct{})}
		f.m[key] = c
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fillTimeout)
			defer cancel()
			c.v, c.err = fn(fctx)
			f.mu.Lock()
			delete(f.m, key)
			f.mu.Unlock()
			close(c.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.v, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
