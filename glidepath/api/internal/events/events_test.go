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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var t0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fakeReviewer: "tok-<aud>-<user>" authenticates as <user> for audience <aud> only.
type fakeReviewer struct{}

func (fakeReviewer) Review(_ context.Context, token string, auds []string) (*authenticationv1.TokenReviewStatus, error) {
	for _, a := range auds {
		if p := "tok-" + a + "-"; strings.HasPrefix(token, p) {
			return &authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: token[len(p):]}}, nil
		}
	}
	return &authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid"}, nil
}

func cdevent(id, source, typ string, content map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"context": map[string]any{"version": "0.4.1", "id": id, "source": source, "type": typ},
		"subject": map[string]any{"id": "x", "type": "pipelineRun", "content": content},
	})
	return b
}

func env(at time.Time, app, id, source, typ string) Envelope {
	return Envelope{ID: eventID(at, key(source, id)), App: app, RoutingKey: RoutingKey(app, typ), ReceivedAt: at,
		Event: cdevent(id, source, typ, nil)}
}

func newStore(now *time.Time) *Store {
	return &Store{Max: 3, MaxAge: time.Hour, MaxStreams: 2, Now: func() time.Time { return *now }}
}

func TestRoutingKey(t *testing.T) {
	for typ, want := range map[string]string{
		"dev.cdevents.pipelinerun.finished.0.3.0":  "gate-api.pipelinerun.finished",
		"dev.cdevents.testcaserun.finished.0.3.0":  "gate-api.testcaserun.finished",
		"dev.hangar.release.promotion-ready.0.1.0": "gate-api.release.promotion-ready",
		"weird":     "gate-api.unknown.weird",
		"Dev.X.A.B": "gate-api.a.b",
	} {
		if got := RoutingKey("gate-api", typ); got != want {
			t.Errorf("%s: got %s want %s", typ, got, want)
		}
	}
}

func TestStoreBoundsDedupeOrder(t *testing.T) {
	now := t0
	s := newStore(&now)
	for i := 0; i < 5; i++ {
		ok, err := s.Add(env(t0.Add(time.Duration(i)*time.Second), "shop", fmt.Sprint(i), "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0"))
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	got := s.All()
	if len(got) != 3 || got[0].ReceivedAt != t0.Add(2*time.Second) {
		t.Fatalf("count bound: %+v", got)
	}
	// Same source+id again (another receive time, as a retry would be): dropped.
	if ok, _ := s.Add(env(t0.Add(time.Minute), "shop", "4", "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0")); ok {
		t.Fatal("duplicate kept")
	}
	// Out of order insert lands in id order.
	s.Max = 10
	s.Add(env(t0.Add(3500*time.Millisecond), "shop", "late", "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0"))
	got = s.All()
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("not ordered: %v", got)
		}
	}
	// Age bound.
	now = t0.Add(time.Hour + 3600*time.Millisecond)
	got = s.All()
	if len(got) != 1 || got[0].ReceivedAt != t0.Add(4*time.Second) {
		t.Fatalf("age bound: %+v", got)
	}
	// A pruned event's key is forgotten with it; one already too old is never stored.
	if ok, _ := s.Add(env(t0, "shop", "0", "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0")); ok {
		t.Fatal("stored an event older than MaxAge")
	}
}

func TestFilterAndResume(t *testing.T) {
	now := t0.Add(time.Minute)
	s := &Store{Max: 100, MaxAge: time.Hour, MaxStreams: 5, Now: func() time.Time { return now }}
	a := env(t0, "shop", "a", "/glidepath/app-shop-cicd/r1", "dev.cdevents.pipelinerun.started.0.3.0")
	b := env(t0.Add(time.Second), "cart", "b", "/glidepath/app-cart-cicd/r1", "dev.cdevents.pipelinerun.finished.0.3.0")
	c := env(t0.Add(20*time.Second), "shop", "c", "/glidepath/app-shop-cicd/r2", "dev.cdevents.pipelinerun.finished.0.3.0")
	d := env(t0.Add(25*time.Second), "shop", "d", "/glidepath/app-shop-cicd/r2", "dev.cdevents.artifact.published.0.3.0")
	for _, e := range []Envelope{a, b, c, d} {
		s.Add(e)
	}
	ids := func(es []Envelope) string {
		var out []string
		for _, e := range es {
			var h header
			json.Unmarshal(e.Event, &h)
			out = append(out, h.Context.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(s.Recent(Filter{App: "shop"}, Resume{}, 0)); got != "a,c,d" {
		t.Error("app filter:", got)
	}
	if got := ids(s.Recent(Filter{TypePrefix: "dev.cdevents.pipelinerun."}, Resume{}, 0)); got != "a,b,c" {
		t.Error("type filter:", got)
	}
	if got := ids(s.Recent(Filter{}, Resume{}, 2)); got != "c,d" {
		t.Error("limit keeps the newest:", got)
	}
	// After c's id with a 10 s grace: d, and nothing from before c-10s; c itself is not repeated.
	r, err := ParseResume(c.ID, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(s.Recent(Filter{}, r, 0)); got != "d" {
		t.Error("resume after id:", got)
	}
	// A grace window that reaches b replays it (it may have arrived after c on another replica).
	r, _ = ParseResume(c.ID, "", 19*time.Second)
	if got := ids(s.Recent(Filter{}, r, 0)); got != "b,d" {
		t.Error("resume with grace:", got)
	}
	r, _ = ParseResume("", t0.Add(500*time.Millisecond).Format(time.RFC3339Nano), 0)
	if got := ids(s.Recent(Filter{}, r, 0)); got != "b,c,d" {
		t.Error("since a time:", got)
	}
	if _, err := ParseResume("", "yesterday", 0); err == nil {
		t.Error("bad since accepted")
	}
	// Last-Event-ID wins over since.
	r, _ = ParseResume(c.ID, t0.Format(time.RFC3339), 0)
	if r.AfterID != c.ID {
		t.Error("Last-Event-ID not preferred")
	}
}

func TestSubscribe(t *testing.T) {
	now := t0
	s := &Store{Max: 1000, MaxAge: time.Hour, MaxStreams: 2, Now: func() time.Time { return now }}
	first := env(t0, "shop", "1", "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0")
	s.Add(first)
	sub, replay, err := s.Subscribe(Filter{App: "shop"}, Resume{After: t0.Add(-time.Second)})
	if err != nil || len(replay) != 1 {
		t.Fatal(err, replay)
	}
	s.Add(env(t0.Add(time.Second), "cart", "2", "/glidepath/app-cart-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0"))
	s.Add(env(t0.Add(2*time.Second), "shop", "3", "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0"))
	select {
	case e := <-sub.C:
		if e.App != "shop" || !strings.Contains(string(e.Event), `"3"`) {
			t.Fatal("wrong event", e)
		}
	default:
		t.Fatal("live event not delivered")
	}
	if len(sub.C) != 0 {
		t.Fatal("filtered event delivered")
	}
	// Stream limit.
	sub2, _, _ := s.Subscribe(Filter{}, Resume{})
	if _, _, err := s.Subscribe(Filter{}, Resume{}); err != ErrBusy {
		t.Fatal("over MaxStreams:", err)
	}
	s.Unsubscribe(sub2)
	s.Unsubscribe(sub2) // twice is fine
	// A subscriber that doesn't read is ended, not blocked on.
	for i := 0; i < subQueue+1; i++ {
		s.Add(env(t0.Add(time.Duration(10+i)*time.Millisecond), "shop", "f"+strconv.Itoa(i), "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.started.0.3.0"))
	}
	if !sub.Lagged {
		t.Fatal("slow subscriber not marked lagged")
	}
	n := 0
	for range sub.C {
		n++
	}
	if n != subQueue {
		t.Fatal("queue", n)
	}
	s.Unsubscribe(sub)
	sub3, _, _ := s.Subscribe(Filter{}, Resume{})
	s.Close()
	if _, ok := <-sub3.C; ok {
		t.Fatal("Close left a stream open")
	}
	if _, _, err := s.Subscribe(Filter{}, Resume{}); err == nil {
		t.Fatal("subscribed after Close")
	}
}

// ---- tap ----

func tapRequest(t *testing.T, authz string, body []byte) *http.Request {
	h := map[string][]string{"Content-Type": {"application/cloudevents+json"}}
	if authz != "" {
		h["Authorization"] = []string{authz}
	}
	raw, _ := json.Marshal(map[string]any{"body": string(body), "header": h,
		"extensions": map[string]any{}, "interceptor_params": map[string]any{"audience": "anything"}})
	return httptest.NewRequest(http.MethodPost, "/internal/events/tap", bytes.NewReader(raw))
}

func newTap(logs io.Writer) (*Tap, *Store) {
	st := &Store{Max: 100, MaxAge: time.Hour, MaxStreams: 5, Now: func() time.Time { return t0 }}
	return &Tap{
		Verifier: &Verifier{Reviewer: fakeReviewer{}, Audience: "cdevents-broker",
			Relays: map[string]bool{"system:serviceaccount:platform-system:glidepath-relay": true}},
		Store: st, Log: slog.New(slog.NewJSONHandler(logs, nil)), Now: func() time.Time { return t0 },
	}, st
}

func TestTap(t *testing.T) {
	const pr = "dev.cdevents.pipelinerun.finished.0.3.0"
	shopSA := "tok-cdevents-broker-system:serviceaccount:app-shop-cicd:pipeline-runner"
	relay := "tok-cdevents-broker-system:serviceaccount:platform-system:glidepath-relay"
	cases := []struct {
		name, authz string
		body        []byte
		app         string // "" = dropped
	}{
		{"pipeline event", "Bearer " + shopSA, cdevent("1", "/glidepath/app-shop-cicd/shop-push-1", pr, nil), "shop"},
		{"infra app", "Bearer tok-cdevents-broker-system:serviceaccount:infra-db-cicd:x", cdevent("2", "/glidepath/infra-db-cicd/r", pr, nil), "db"},
		{"old source prefix", "Bearer " + shopSA, cdevent("3", "/platform-cicd/app-shop-cicd/r", pr, nil), "shop"},
		{"no token", "", cdevent("4", "/glidepath/app-shop-cicd/r", pr, nil), ""},
		{"not bearer", "Basic " + shopSA, cdevent("5", "/glidepath/app-shop-cicd/r", pr, nil), ""},
		{"bad token", "Bearer forged", cdevent("6", "/glidepath/app-shop-cicd/r", pr, nil), ""},
		{"other audience", "Bearer tok-kubernetes-system:serviceaccount:app-shop-cicd:pipeline-runner", cdevent("7", "/glidepath/app-shop-cicd/r", pr, nil), ""},
		{"not a ServiceAccount", "Bearer tok-cdevents-broker-alice", cdevent("8", "/glidepath/app-shop-cicd/r", pr, nil), ""},
		{"another app's source", "Bearer " + shopSA, cdevent("9", "/glidepath/app-cart-cicd/r", pr, nil), ""},
		{"not a -cicd namespace", "Bearer tok-cdevents-broker-system:serviceaccount:default:x", cdevent("10", "/glidepath/default/r", pr, nil), ""},
		{"claims another app", "Bearer " + shopSA, cdevent("11", "/glidepath/app-shop-cicd/r", pr, map[string]any{"appName": "cart"}), ""},
		{"bad source", "Bearer " + shopSA, cdevent("12", "https://evil/app-shop-cicd", pr, nil), ""},
		{"relay", "Bearer " + relay, cdevent("13", "/platform-cicd/app-cart-cicd/hangar-prod-cart-prod-outcome", "dev.cdevents.environment.deployed.0.2.0",
			map[string]any{"appName": "cart", "appNamespace": "app-cart-cicd"}), "cart"},
		{"relay namespace mismatch", "Bearer " + relay, cdevent("14", "/platform-cicd/app-shop-cicd/x", "dev.cdevents.environment.deployed.0.2.0",
			map[string]any{"appName": "cart", "appNamespace": "app-cart-cicd"}), ""},
		{"relay without app", "Bearer " + relay, cdevent("15", "/platform-cicd/platform-system/x", pr, nil), ""},
		{"non-relay platform SA", "Bearer tok-cdevents-broker-system:serviceaccount:platform-system:other", cdevent("16", "/platform-cicd/app-cart-cicd/x", pr,
			map[string]any{"appName": "cart", "appNamespace": "app-cart-cicd"}), ""},
		{"not a CDEvent", "Bearer " + shopSA, []byte(`{"hello":"world"}`), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			tap, st := newTap(&logs)
			w := httptest.NewRecorder()
			tap.ServeHTTP(w, tapRequest(t, c.authz, c.body))
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if resp["continue"] != false {
				t.Fatal("tap must always answer continue: false", resp)
			}
			got := st.All()
			if c.app == "" {
				if len(got) != 0 {
					t.Fatal("kept a rejected event", got)
				}
				if !strings.Contains(logs.String(), "events.tap drop") {
					t.Fatal("drop not logged")
				}
			} else if len(got) != 1 || got[0].App != c.app {
				t.Fatal("not kept as", c.app, got, logs.String())
			}
			// Never a token in a log line.
			if tok := strings.TrimPrefix(strings.TrimPrefix(c.authz, "Bearer "), "Basic "); tok != "" && strings.Contains(logs.String(), tok) {
				t.Fatal("token logged:", logs.String())
			}
		})
	}
	// The interceptor_params audience is the caller's to send; the tap ignores it.
	var logs bytes.Buffer
	tap, st := newTap(&logs)
	tap.ServeHTTP(httptest.NewRecorder(), tapRequest(t, "Bearer tok-anything-system:serviceaccount:app-shop-cicd:x", cdevent("a", "/glidepath/app-shop-cicd/r", pr, nil)))
	if len(st.All()) != 0 {
		t.Fatal("audience taken from the request")
	}
	// The same event twice is kept once, and the envelope says where it goes.
	ev := cdevent("dup", "/glidepath/app-shop-cicd/r", pr, nil)
	tap.ServeHTTP(httptest.NewRecorder(), tapRequest(t, "Bearer "+shopSA, ev))
	tap.ServeHTTP(httptest.NewRecorder(), tapRequest(t, "Bearer "+shopSA, ev))
	got := st.All()
	if len(got) != 1 || got[0].RoutingKey != "shop.pipelinerun.finished" || !got[0].ReceivedAt.Equal(t0) {
		t.Fatal(got)
	}
	var back map[string]any
	if json.Unmarshal(got[0].Event, &back) != nil || back["context"].(map[string]any)["id"] != "dup" {
		t.Fatal("event not kept verbatim", string(got[0].Event))
	}
}

// ---- peers ----

func peerSlice(port int, addrs ...string) *discoveryv1.EndpointSlice {
	eps := []discoveryv1.Endpoint{}
	for _, a := range addrs {
		eps = append(eps, discoveryv1.Endpoint{Addresses: []string{a}})
	}
	p := int32(port)
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "glidepath-api-abc", Namespace: "platform-system", Labels: map[string]string{"kubernetes.io/service-name": "glidepath-api"}},
		AddressType: discoveryv1.AddressTypeIPv4, Endpoints: eps, Ports: []discoveryv1.EndpointPort{{Port: &p}},
	}
}

func TestPeerForwardAndBackfill(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	const self = "system:serviceaccount:platform-system:glidepath-api"
	remote := &Store{Max: 100, MaxAge: time.Hour, MaxStreams: 5, Now: func() time.Time { return t0 }}
	var mu sync.Mutex
	posts := 0
	ph := &PeerHandler{Reviewer: fakeReviewer{}, Audience: "glidepath-api-peer", Identity: self, Store: remote, Log: logger}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts++
			mu.Unlock()
		}
		ph.ServeHTTP(w, r)
	}))
	defer srv.Close()
	_, portS, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portS)
	tokFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokFile, []byte("tok-glidepath-api-peer-"+self+"\n"), 0o600)
	cs := fake.NewSimpleClientset(peerSlice(port, "10.0.0.1", "127.0.0.1"))
	peers := &Peers{CS: cs, Namespace: "platform-system", Service: "glidepath-api", Port: port, SelfIP: "10.0.0.1",
		TokenPath: tokFile, HTTP: &http.Client{Timeout: 5 * time.Second}, Log: logger, TTL: time.Second}

	local := &Store{Max: 100, MaxAge: time.Hour, MaxStreams: 5, Now: func() time.Time { return t0 }}
	tap := &Tap{Verifier: &Verifier{Reviewer: fakeReviewer{}, Audience: "cdevents-broker"}, Store: local, Peers: peers, Log: logger, Now: func() time.Time { return t0 }}
	ev := cdevent("e1", "/glidepath/app-shop-cicd/r", "dev.cdevents.pipelinerun.finished.0.3.0", nil)
	tap.ServeHTTP(httptest.NewRecorder(), tapRequest(t, "Bearer tok-cdevents-broker-system:serviceaccount:app-shop-cicd:x", ev))
	deadline := time.Now().Add(5 * time.Second)
	for len(remote.All()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	r, l := remote.All(), local.All()
	if len(r) != 1 || r[0].ID != l[0].ID || r[0].App != "shop" {
		t.Fatalf("peer did not get the same envelope: %+v vs %+v", r, l)
	}
	// The peer got it from us: the self address was skipped, and a duplicate tap call isn't re-forwarded.
	tap.ServeHTTP(httptest.NewRecorder(), tapRequest(t, "Bearer tok-cdevents-broker-system:serviceaccount:app-shop-cicd:x", ev))
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if posts != 1 {
		t.Fatal("posts:", posts)
	}
	mu.Unlock()

	// Backfill: a fresh replica copies the peer's buffer.
	fresh := &Store{Max: 100, MaxAge: time.Hour, MaxStreams: 5, Now: func() time.Time { return t0 }}
	if n := peers.Backfill(context.Background(), fresh); n != 1 || fresh.All()[0].ID != r[0].ID {
		t.Fatal("backfill", n, fresh.All())
	}

	// Only glidepath-api, with the peer audience, may post or read.
	body, _ := json.Marshal(r[0])
	for _, tok := range []string{"", "tok-glidepath-api-peer-system:serviceaccount:app-shop-cicd:x", "tok-cdevents-broker-" + self, "forged"} {
		for _, m := range []string{http.MethodPost, http.MethodGet} {
			req := httptest.NewRequest(m, PeerPath, bytes.NewReader(body))
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			w := httptest.NewRecorder()
			ph.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s with %q: %d", m, tok, w.Code)
			}
		}
	}
}
