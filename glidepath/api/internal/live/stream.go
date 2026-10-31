package live

// GET /v1/apps/{app}/live/stream (follow-on 5, 2026-10-31): an app's runs pushed to the viewer instead of polled.
// Tower's CI/CD tab polled GET /v1/apps/{app}/live every 6 s per open tab (about 760 calls an hour each, every one
// through the dev cluster's API server proxy). A stream is one connection per viewer: a snapshot when it opens, then
// one change per PipelineRun or TaskRun that appears, moves (a new resourceVersion) or goes.
//
// One watch per namespace serves every viewer of it, on each replica: it lists the namespace's PipelineRuns and
// TaskRuns every Interval (through the Client's ListTTL cache, so the polling reads and the streams share one list)
// and diffs the result against what it last sent. The watch starts with its first viewer and stops with its last,
// so a namespace nobody watches costs nothing. A list rather than a Kubernetes watch keeps the API's RBAC and its
// tests as they are, and an Interval of 2 s is as fresh as the cache.

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ErrBusy: this replica already serves MaxStreams live streams.
var ErrBusy = errors.New("too many live streams on this replica")

// Snapshot is the namespace's runs as a stream starts: what GET /v1/apps/{app}/live answers.
type Snapshot struct {
	PipelineRuns []Obj `json:"pipelineRuns"`
	TaskRuns     []Obj `json:"taskRuns"`
}

// Change is one object that appeared or moved (upsert, with the object) or went (delete, its kind and name only).
type Change struct {
	Type   string `json:"type"` // upsert, delete
	Kind   string `json:"kind"` // PipelineRun, TaskRun
	Name   string `json:"name"`
	Object Obj    `json:"object,omitempty"`
}

// Sub is one viewer's stream. C closes when the viewer fell too far behind (Lagged) or the watch stopped; a viewer
// then reconnects for a fresh snapshot.
type Sub struct {
	C      chan Change
	Lagged bool
	w      *watch
}

// Hub holds the namespaces' watches.
type Hub struct {
	Client     *Client
	Interval   time.Duration // 0 = ListTTL
	MaxStreams int           // 0 = no limit
	Buffer     int           // changes queued per viewer before it counts as lagged; 0 = 256
	Log        *slog.Logger

	mu      sync.Mutex
	watches map[string]*watch
	streams int
}

type watch struct {
	ns     string
	refs   int // viewers subscribed or subscribing, under Hub.mu
	ready  chan struct{}
	err    error // the first list's failure: the watch never started
	cancel context.CancelFunc

	mu   sync.Mutex
	objs map[string]Obj // "<kind>/<name>" -> the object as last sent
	subs map[*Sub]struct{}
}

func kindOf(o Obj) string {
	k, _ := o["kind"].(string)
	return k
}

func nameOf(o Obj) string {
	meta, _ := o["metadata"].(map[string]any)
	n, _ := meta["name"].(string)
	return n
}

func versionOf(o Obj) string {
	meta, _ := o["metadata"].(map[string]any)
	v, _ := meta["resourceVersion"].(string)
	return v
}

// Subscribe opens a viewer's stream of namespace: the snapshot it starts from, and the changes after it. Every
// Subscribe needs its Unsubscribe.
func (h *Hub) Subscribe(ctx context.Context, namespace string) (*Sub, Snapshot, error) {
	h.mu.Lock()
	if h.MaxStreams > 0 && h.streams >= h.MaxStreams {
		h.mu.Unlock()
		return nil, Snapshot{}, ErrBusy
	}
	if h.watches == nil {
		h.watches = map[string]*watch{}
	}
	w := h.watches[namespace]
	if w == nil {
		wctx, cancel := context.WithCancel(context.Background())
		w = &watch{ns: namespace, ready: make(chan struct{}), cancel: cancel, objs: map[string]Obj{}, subs: map[*Sub]struct{}{}}
		h.watches[namespace] = w
		go h.run(wctx, w)
	}
	w.refs++
	h.streams++
	h.mu.Unlock()

	select {
	case <-w.ready:
	case <-ctx.Done():
		h.release(w)
		return nil, Snapshot{}, ctx.Err()
	}
	if w.err != nil {
		h.release(w)
		return nil, Snapshot{}, w.err
	}
	buf := h.Buffer
	if buf <= 0 {
		buf = 256
	}
	sub := &Sub{C: make(chan Change, buf), w: w}
	w.mu.Lock()
	snap := Snapshot{PipelineRuns: []Obj{}, TaskRuns: []Obj{}}
	keys := make([]string, 0, len(w.objs))
	for k := range w.objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := w.objs[k]
		if kindOf(o) == "TaskRun" {
			snap.TaskRuns = append(snap.TaskRuns, o)
		} else {
			snap.PipelineRuns = append(snap.PipelineRuns, o)
		}
	}
	w.subs[sub] = struct{}{}
	w.mu.Unlock()
	return sub, snap, nil
}

// Unsubscribe ends a viewer's stream; the namespace's watch stops with its last viewer.
func (h *Hub) Unsubscribe(sub *Sub) {
	if sub == nil {
		return
	}
	sub.w.mu.Lock()
	if _, ok := sub.w.subs[sub]; ok {
		delete(sub.w.subs, sub)
		close(sub.C)
	}
	sub.w.mu.Unlock()
	h.release(sub.w)
}

func (h *Hub) release(w *watch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.refs--
	h.streams--
	if w.refs == 0 {
		w.cancel()
		if h.watches[w.ns] == w {
			delete(h.watches, w.ns)
		}
	}
}

// Watching is how many namespaces have a watch running, and how many streams this replica serves.
func (h *Hub) Watching() (namespaces, streams int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watches), h.streams
}

func (h *Hub) run(ctx context.Context, w *watch) {
	interval := h.Interval
	if interval <= 0 {
		interval = ListTTL
	}
	first := true
	for {
		err := h.poll(ctx, w)
		if first {
			w.err = err
			close(w.ready)
			first = false
			if err != nil {
				return
			}
		} else if err != nil && ctx.Err() == nil && h.Log != nil {
			h.Log.Warn("live.stream poll failed", "namespace", w.ns, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for s := range w.subs {
				delete(w.subs, s)
				close(s.C)
			}
			w.mu.Unlock()
			return
		case <-time.After(interval):
		}
	}
}

// poll lists the namespace and sends every viewer what changed since the last list.
func (h *Hub) poll(ctx context.Context, w *watch) error {
	prs, err := h.Client.PipelineRuns(ctx, w.ns)
	if err != nil {
		return err
	}
	trs, err := h.Client.TaskRuns(ctx, w.ns)
	if err != nil {
		return err
	}
	now := make(map[string]Obj, len(prs)+len(trs))
	for _, o := range append(append([]Obj{}, prs...), trs...) {
		now[kindOf(o)+"/"+nameOf(o)] = o
	}
	var changes []Change
	keys := make([]string, 0, len(now))
	for k := range now {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range keys {
		o := now[k]
		if prev, ok := w.objs[k]; !ok || versionOf(prev) != versionOf(o) {
			changes = append(changes, Change{Type: "upsert", Kind: kindOf(o), Name: nameOf(o), Object: o})
		}
	}
	gone := []string{}
	for k := range w.objs {
		if _, ok := now[k]; !ok {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	for _, k := range gone {
		o := w.objs[k]
		changes = append(changes, Change{Type: "delete", Kind: kindOf(o), Name: nameOf(o)})
	}
	w.objs = now
	for s := range w.subs {
		for _, c := range changes {
			select {
			case s.C <- c:
			default: // the viewer fell behind: it reconnects for a fresh snapshot rather than miss a change
				s.Lagged = true
				delete(w.subs, s)
				close(s.C)
			}
			if s.Lagged {
				break
			}
		}
	}
	return nil
}
