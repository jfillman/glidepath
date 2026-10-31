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

func TestALaggingViewerIsClosedNotSkipped(t *testing.T) {
	st := &listStore{objs: []Obj{tr("a", "1")}}
	h := &Hub{Client: &Client{Store: st, TTL: time.Millisecond}, Interval: 5 * time.Millisecond, Buffer: 1}
	sub, snap, err := h.Subscribe(context.Background(), "ns")
	if err != nil || len(snap.TaskRuns) != 1 {
		t.Fatalf("%v %v", snap, err)
	}
	defer h.Unsubscribe(sub)
	st.mu.Lock()
	st.objs = []Obj{tr("a", "2"), tr("b", "1"), tr("c", "1")} // three changes, room for one
	st.mu.Unlock()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				if !sub.Lagged {
					t.Fatal("closed without being lagged")
				}
				return
			}
		case <-deadline:
			t.Fatal("a viewer that fell behind was never closed")
		}
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
