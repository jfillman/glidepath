// Package events is glidepath-api's CDEvents stream (docs/admin/api.md#events): a tap on the shared
// broker that re-verifies every event's sender (tap.go), an in-memory buffer each replica keeps
// (this file), and fan-out to the other replicas (peers.go). Best-effort, never durable: a replica
// that restarts keeps only what it can copy from a peer.
package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Envelope is what a subscriber gets: the CDEvent as the pipeline sent it, and what the API knows
// about it. ID orders events by when the first replica received them, so it means the same thing
// on every replica and a stream can resume on another one.
type Envelope struct {
	ID         string          `json:"id"`
	App        string          `json:"app"`
	RoutingKey string          `json:"routingKey"`
	ReceivedAt time.Time       `json:"receivedAt"`
	Event      json.RawMessage `json:"event"`
}

// header is the part of a CDEvent the API reads.
type header struct {
	Context struct {
		ID     string `json:"id"`
		Source string `json:"source"`
		Type   string `json:"type"`
	} `json:"context"`
	Subject struct {
		Type    string         `json:"type"`
		Content map[string]any `json:"content"`
	} `json:"subject"`
}

func parseHeader(raw []byte) (*header, error) {
	var h header
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("not a CDEvent: %w", err)
	}
	if h.Context.ID == "" || h.Context.Source == "" || h.Context.Type == "" {
		return nil, errors.New("not a CDEvent: context.id, context.source and context.type are required")
	}
	if len(h.Context.ID) > 256 || len(h.Context.Source) > 512 || len(h.Context.Type) > 256 {
		return nil, errors.New("CDEvent context field too long")
	}
	return &h, nil
}

// key identifies one CDEvent for dedupe: the spec makes context.id unique per source.
func key(source, id string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

// eventID is "<receivedAt, unix microseconds, 16 digits>-<first 12 hex of key>": ordered by time
// as plain strings, and the same on every replica that got the event from the one that received it.
func eventID(at time.Time, k string) string {
	return fmt.Sprintf("%016d-%s", at.UnixMicro(), k[:12])
}

var idPattern = regexp.MustCompile(`^(\d{16})-[0-9a-f]{12}$`)

// idTime is the receive time an event id carries.
func idTime(id string) (time.Time, bool) {
	m := idPattern.FindStringSubmatch(id)
	if m == nil {
		return time.Time{}, false
	}
	us, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMicro(us).UTC(), true
}

// RoutingKey is "<app>.<subject>.<predicate>", lower case, from the CDEvent type
// (dev.cdevents.pipelinerun.finished.0.3.0 -> <app>.pipelinerun.finished): the AMQP topic
// semantics Autopilot's event triggers match (autopilot triggers.amqp_topic_match).
func RoutingKey(app, eventType string) string {
	parts := strings.Split(strings.ToLower(eventType), ".")
	for len(parts) > 0 {
		if _, err := strconv.Atoi(parts[len(parts)-1]); err != nil {
			break
		}
		parts = parts[:len(parts)-1] // the version
	}
	subject, predicate := "unknown", "unknown"
	if n := len(parts); n >= 2 {
		subject, predicate = parts[n-2], parts[n-1]
	} else if n == 1 && parts[0] != "" {
		predicate = parts[0]
	}
	return app + "." + subject + "." + predicate
}

// Filter selects events: one app ("" = every app) and a CDEvent type prefix ("" = every type).
type Filter struct {
	App        string
	TypePrefix string
}

type item struct {
	Envelope
	key string
	typ string
}

func (f Filter) match(it *item) bool {
	return (f.App == "" || it.App == f.App) && strings.HasPrefix(it.typ, f.TypePrefix)
}

// ErrBusy is returned when a replica already serves its maximum number of streams.
var ErrBusy = errors.New("too many event streams on this replica")

// Store is one replica's buffer: at most Max events, none older than MaxAge, ordered by ID.
type Store struct {
	Max        int
	MaxAge     time.Duration
	MaxStreams int
	Now        func() time.Time

	mu     sync.Mutex
	items  []*item
	seen   map[string]bool
	subs   map[*Sub]struct{}
	closed bool
}

// Sub is one live stream's queue. C is closed when the stream fell too far behind (Lagged) or the
// replica is shutting down.
type Sub struct {
	C      chan Envelope
	filter Filter
	Lagged bool
}

const subQueue = 256

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// prune drops events past MaxAge and the oldest past Max. Caller holds mu.
func (s *Store) prune() {
	cut := s.now().Add(-s.MaxAge)
	drop := 0
	for drop < len(s.items) && (s.items[drop].ReceivedAt.Before(cut) || len(s.items)-drop > s.Max) {
		delete(s.seen, s.items[drop].key)
		drop++
	}
	if drop > 0 {
		s.items = append([]*item(nil), s.items[drop:]...)
	}
}

// Add stores an envelope unless the same CDEvent (context.source + context.id) is already here,
// and hands it to every matching stream. It reports whether the event was new.
func (s *Store) Add(e Envelope) (bool, error) {
	h, err := parseHeader(e.Event)
	if err != nil {
		return false, err
	}
	it := &item{Envelope: e, key: key(h.Context.Source, h.Context.ID), typ: h.Context.Type}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[it.key] {
		return false, nil
	}
	if it.ReceivedAt.Before(s.now().Add(-s.MaxAge)) {
		return false, nil // already too old to keep (a slow peer's copy)
	}
	s.seen[it.key] = true
	i := sort.Search(len(s.items), func(i int) bool { return s.items[i].ID > it.ID })
	s.items = append(s.items, nil)
	copy(s.items[i+1:], s.items[i:])
	s.items[i] = it
	s.prune()
	for sub := range s.subs {
		if !sub.filter.match(it) {
			continue
		}
		select {
		case sub.C <- it.Envelope:
		default:
			// The client isn't reading fast enough. End its stream; it resumes with Last-Event-ID.
			sub.Lagged = true
			close(sub.C)
			delete(s.subs, sub)
		}
	}
	return true, nil
}

// Resume says where a stream or a read starts: after an event id, or after a time.
type Resume struct {
	AfterID string
	After   time.Time
	Grace   time.Duration // with AfterID: also replay events received up to Grace before it
}

// ParseResume reads Last-Event-ID (preferred) or ?since= (an event id or an RFC 3339 time).
func ParseResume(lastEventID, since string, grace time.Duration) (Resume, error) {
	v := strings.TrimSpace(lastEventID)
	if v == "" {
		v = strings.TrimSpace(since)
	}
	if v == "" {
		return Resume{}, nil
	}
	if _, ok := idTime(v); ok {
		return Resume{AfterID: v, Grace: grace}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return Resume{}, Invalid{What: "since / Last-Event-ID must be an event id or an RFC 3339 time"}
	}
	return Resume{After: t}, nil
}

func (r Resume) set() bool { return r.AfterID != "" || !r.After.IsZero() }

// includes says whether a resume replays it. After an id, an event received shortly before it is
// replayed too: replicas can add a peer's event slightly out of order, so a stream may have sent a
// later id before an earlier one arrived. Subscribers dedupe by id.
func (r Resume) includes(it *item) bool {
	switch {
	case r.AfterID != "":
		if it.ID == r.AfterID {
			return false
		}
		if it.ID > r.AfterID {
			return true
		}
		t, _ := idTime(r.AfterID)
		return !it.ReceivedAt.Before(t.Add(-r.Grace))
	case !r.After.IsZero():
		return it.ReceivedAt.After(r.After)
	}
	return true
}

// Recent returns the matching events in id order (oldest first), at most limit of the newest.
func (s *Store) Recent(f Filter, r Resume, limit int) []Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	return s.collect(f, r, limit)
}

func (s *Store) collect(f Filter, r Resume, limit int) []Envelope {
	out := []Envelope{}
	for _, it := range s.items {
		if f.match(it) && r.includes(it) {
			out = append(out, it.Envelope)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe registers a live stream and, when the resume is set, returns what it replays, under
// one lock, so nothing falls between the replay and the first live event.
func (s *Store) Subscribe(f Filter, r Resume) (*Sub, []Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrBusy
	}
	if s.subs == nil {
		s.subs = map[*Sub]struct{}{}
	}
	if len(s.subs) >= s.MaxStreams {
		return nil, nil, ErrBusy
	}
	var replay []Envelope
	if r.set() {
		s.prune()
		replay = s.collect(f, r, 0)
	}
	sub := &Sub{C: make(chan Envelope, subQueue), filter: f}
	s.subs[sub] = struct{}{}
	return sub, replay, nil
}

// Unsubscribe removes a stream (a no-op when Add already ended it).
func (s *Store) Unsubscribe(sub *Sub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		close(sub.C)
	}
}

// Streams is the number of live streams.
func (s *Store) Streams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// Close ends every stream; the replica is shutting down.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for sub := range s.subs {
		close(sub.C)
		delete(s.subs, sub)
	}
}

// All is every buffered event, for a peer starting up.
func (s *Store) All() []Envelope {
	return s.Recent(Filter{}, Resume{}, 0)
}

// Invalid is a malformed query.
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }
