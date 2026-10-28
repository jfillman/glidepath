package results

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightsCollapseConcurrentReads(t *testing.T) {
	var f flights
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := f.do(context.Background(), "k", func(context.Context) (any, error) {
				calls.Add(1)
				<-release
				return "v", nil
			})
			if err != nil || v != "v" {
				t.Errorf("got %v, %v", v, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("5 concurrent reads of one key ran %d times", n)
	}
}

// The case behind the 10 s timeouts: Tower gives up, and the read it started must still finish
// and fill the cache, or every retry starts cold.
func TestRunReadOutlivesItsCaller(t *testing.T) {
	const id = "0b9d1a2e-1111-4c4c-9999-aaaaaaaaaaaa"
	var hits atomic.Int32
	slow := make(chan struct{})
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-slow
		pr, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "p"}, "status": map[string]any{"completionTime": "e"}})
		json.NewEncoder(w).Encode(map[string]any{"records": []any{
			map[string]any{"name": "ns/results/" + id + "/records/r", "data": map[string]any{"type": pipelineRun, "value": base64.StdEncoding.EncodeToString(pr)}},
		}})
	}))
	defer stub.Close()
	c := &Client{Base: stub.URL, HTTP: stub.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "ns", id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller should give up at its deadline, got %v", err)
	}
	close(slow)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := c.cache.get(runKey("ns", id)); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned read never filled the cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.Get(context.Background(), "ns", id); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("Results was asked %d times; the retry should have been served from the cache", n)
	}
}

func TestCacheEvictsSoonestToExpireWhenFull(t *testing.T) {
	var c ttlCache
	for i := 0; i < maxEntries; i++ {
		c.set(string(rune('a'+i%26))+time.Duration(i).String(), i, time.Hour+time.Duration(i)*time.Millisecond)
	}
	c.set("soon", 0, time.Minute)
	c.set("new", 0, time.Hour)
	if _, ok := c.get("soon"); ok {
		t.Fatal("the entry closest to expiry should have been dropped")
	}
	if len(c.m) != maxEntries {
		t.Fatalf("cache holds %d entries, want %d", len(c.m), maxEntries)
	}
}
