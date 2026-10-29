package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PeerPath is where replicas hand each other events (POST one) and a starting replica copies a
// peer's buffer (GET).
const PeerPath = "/internal/events/peer"

// Peers fans an event out to the Service's other replicas. An event reaches one replica (the one
// the broker's interceptor call landed on); it stores it and forwards it here, so a stream on any
// replica sees every event. Peers are every address in the Service's EndpointSlices, ready or not
// (a starting replica already serves), except this pod's own.
type Peers struct {
	CS        kubernetes.Interface
	Namespace string
	Service   string
	Port      int
	SelfIP    string
	TokenPath string // this pod's projected token, audience PeerAudience
	HTTP      *http.Client
	Log       *slog.Logger
	TTL       time.Duration // how long a peer list is reused

	mu    sync.Mutex
	addrs []string
	at    time.Time
}

func (p *Peers) list(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	if !p.at.IsZero() && time.Since(p.at) < p.TTL {
		a := p.addrs
		p.mu.Unlock()
		return a, nil
	}
	p.mu.Unlock()
	slices, err := p.CS.DiscoveryV1().EndpointSlices(p.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + p.Service,
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			for _, a := range ep.Addresses {
				if a == p.SelfIP || seen[a] {
					continue
				}
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	p.mu.Lock()
	p.addrs, p.at = out, time.Now()
	p.mu.Unlock()
	return out, nil
}

func (p *Peers) url(addr string) string {
	return "http://" + net.JoinHostPort(addr, strconv.Itoa(p.Port)) + PeerPath
}

func (p *Peers) token() (string, error) {
	b, err := os.ReadFile(p.TokenPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Forward sends env to every peer, in the background; a peer that misses it is retried once.
func (p *Peers) Forward(env Envelope) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		addrs, err := p.list(ctx)
		if err != nil {
			p.Log.Warn("events.peer list failed", "error", err.Error())
			return
		}
		body, _ := json.Marshal(env)
		var wg sync.WaitGroup
		for _, a := range addrs {
			wg.Add(1)
			go func(a string) {
				defer wg.Done()
				var err error
				for attempt := 0; attempt < 2; attempt++ {
					if err = p.post(ctx, a, body); err == nil {
						return
					}
					time.Sleep(time.Second)
				}
				p.Log.Warn("events.peer forward failed", "peer", a, "id", env.ID, "error", err.Error())
			}(a)
		}
		wg.Wait()
	}()
}

func (p *Peers) post(ctx context.Context, addr string, body []byte) error {
	tok, err := p.token()
	if err != nil {
		return fmt.Errorf("reading peer token: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.url(addr), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("peer answered %d", resp.StatusCode)
	}
	return nil
}

// Backfill copies one peer's buffer into store: a replica that just started (a deploy, a restart)
// would otherwise answer resumes with nothing. Every peer is asked; duplicates are dropped by Add.
func (p *Peers) Backfill(ctx context.Context, store *Store) int {
	addrs, err := p.list(ctx)
	if err != nil {
		p.Log.Warn("events.peer list failed", "error", err.Error())
		return 0
	}
	n := 0
	for _, a := range addrs {
		got, err := p.get(ctx, a)
		if err != nil {
			p.Log.Warn("events.peer backfill failed", "peer", a, "error", err.Error())
			continue
		}
		for _, e := range got {
			if ok, _ := store.Add(e); ok {
				n++
			}
		}
	}
	return n
}

func (p *Peers) get(ctx context.Context, addr string) ([]Envelope, error) {
	tok, err := p.token()
	if err != nil {
		return nil, fmt.Errorf("reading peer token: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.url(addr), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer answered %d", resp.StatusCode)
	}
	var out struct {
		Events []Envelope `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// PeerHandler serves PeerPath. Only glidepath-api itself may call it: a token for PeerAudience that
// TokenReview says is Identity (system:serviceaccount:platform-system:glidepath-api).
type PeerHandler struct {
	Reviewer Reviewer
	Audience string
	Identity string
	Store    *Store
	Log      *slog.Logger
}

func (h *PeerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		h.deny(w, r, "no bearer token")
		return
	}
	st, err := h.Reviewer.Review(r.Context(), tok, []string{h.Audience})
	if err != nil {
		http.Error(w, "token review failed", http.StatusBadGateway)
		return
	}
	if !st.Authenticated || st.User.Username != h.Identity {
		h.deny(w, r, "not "+h.Identity+" with audience "+h.Audience)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"events": h.Store.All()})
	case http.MethodPost:
		var e Envelope
		if err := json.NewDecoder(io.LimitReader(r.Body, maxTapBody*2)).Decode(&e); err != nil {
			http.Error(w, "not an envelope", http.StatusBadRequest)
			return
		}
		if _, ok := idTime(e.ID); !ok || !appPattern.MatchString(e.App) {
			http.Error(w, "not an envelope", http.StatusBadRequest)
			return
		}
		if _, err := h.Store.Add(e); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *PeerHandler) deny(w http.ResponseWriter, r *http.Request, why string) {
	h.Log.Warn("events.peer deny", "reason", why, "method", r.Method)
	http.Error(w, "forbidden", http.StatusForbidden)
}
