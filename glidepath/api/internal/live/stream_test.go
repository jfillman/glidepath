package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// listStore lists a fixed namespace's TaskRuns, whatever is in objs now; err fails every list.
type listStore struct {
	mu   sync.Mutex
	objs []Obj
	err  error
}

func (s *listStore) List(_ context.Context, gvr schema.GroupVersionResource, _, _ string) ([]Obj, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if gvr.Resource != "taskruns" {
		return nil, nil
	}
	return append([]Obj{}, s.objs...), nil
}
func (s *listStore) Get(context.Context, schema.GroupVersionResource, string, string) (Obj, error) {
	return nil, nil
}
func (s *listStore) Containers(context.Context, string, string) ([]string, error) { return nil, nil }
func (s *listStore) Log(context.Context, string, string, string) (io.ReadCloser, error) {
	return nil, nil
}

func tr(name, rv string) Obj {
	return Obj{"kind": "TaskRun", "metadata": map[string]any{"name": name, "resourceVersion": rv}}
}

// The viewer must not read while the watch queues its changes: a reader that keeps pace is not lagging, and
// whether it keeps pace is a scheduler race. So the test reads nothing until the watch has dropped the viewer
// (observed through the afterPoll hook, no wall-clock bound), then drains what was queued.
func TestALaggingViewerIsClosedNotSkipped(t *testing.T) {
	st := &listStore{objs: []Obj{tr("a", "1")}}
	polled := make(chan struct{}, 1)
	h := &Hub{Client: &Client{Store: st, TTL: time.Millisecond}, Interval: 5 * time.Millisecond, Buffer: 1}
	h.afterPoll = func() {
		select {
		case polled <- struct{}{}:
		default:
		}
	}
	sub, snap, err := h.Subscribe(context.Background(), "ns")
	if err != nil || len(snap.TaskRuns) != 1 {
		t.Fatalf("%v %v", snap, err)
	}
	defer h.Unsubscribe(sub)
	st.mu.Lock()
	st.objs = []Obj{tr("a", "2"), tr("b", "1"), tr("c", "1")} // three changes, room for one
	st.mu.Unlock()
	hang := time.After(time.Minute) // a hang guard only, never the assertion
	for subscribed := true; subscribed; {
		select {
		case <-polled:
		case <-hang:
			t.Fatal("the watch never dropped the lagging viewer")
		}
		sub.w.mu.Lock()
		_, subscribed = sub.w.subs[sub]
		sub.w.mu.Unlock()
	}
	// Dropped: exactly the one change that fit was queued, then the channel is closed and Lagged is set.
	n := 0
	for range sub.C {
		n++
	}
	if n != 1 {
		t.Fatalf("queued %d changes, want 1", n)
	}
	if !sub.Lagged {
		t.Fatal("closed without being lagged")
	}
}

func TestAWatchWhoseFirstListFailsRefusesTheViewer(t *testing.T) {
	st := &listStore{err: errors.New("the API server is down")}
	h := &Hub{Client: &Client{Store: st}, Interval: time.Hour}
	if _, _, err := h.Subscribe(context.Background(), "ns"); err == nil {
		t.Fatal("no error")
	}
	if n, streams := h.Watching(); n != 0 || streams != 0 {
		t.Fatalf("%d %d left behind", n, streams)
	}
}

func TestViewersOfOneNamespaceShareItsWatch(t *testing.T) {
	st := &listStore{objs: []Obj{tr("a", "1")}}
	h := &Hub{Client: &Client{Store: st}, Interval: time.Hour, MaxStreams: 3}
	var subs []*Sub
	for i := 0; i < 3; i++ {
		s, _, err := h.Subscribe(context.Background(), "ns")
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	if n, streams := h.Watching(); n != 1 || streams != 3 {
		t.Fatalf("%d namespaces, %d streams", n, streams)
	}
	if _, _, err := h.Subscribe(context.Background(), "other"); !errors.Is(err, ErrBusy) {
		t.Fatalf("over MaxStreams: %v", err)
	}
	for _, s := range subs {
		h.Unsubscribe(s)
	}
	if n, streams := h.Watching(); n != 0 || streams != 0 {
		t.Fatal(fmt.Sprint(n, streams))
	}
}
