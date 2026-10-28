package evidence

// Rekor entries, read fresh: a port of Tower's rekorEntries.ts (backstage packages/backend/src).
//
// A transparency-log entry's receipt (its Signed Entry Timestamp) is signed by Rekor's key when it
// is fetched. The copy cosign embeds in an image was signed by the key Rekor had at signing time,
// which a Rekor restart with an in-memory signer used to lose; re-reading the entry by its log index
// gives a receipt signed by today's key. Only GET is used. A classic image signature with no
// embedded receipt is found by reading the entries next to its attestations' (Chains logs both in
// the same run) and keeping only one whose body names this exact payload hash and signature.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/glidepath/api/internal/jsval"
)

// RawTlogEntry is everything needed to re-verify a Rekor entry's SET, not just display it.
type RawTlogEntry struct {
	LogIndex             string
	LogIDHex             string
	IntegratedTime       string
	SignedEntryTimestamp string
	CanonicalizedBody    string // base64
	Kind                 string
	Version              string
}

// parseRekorEntryResponse reads Rekor's GET /api/v1/log/entries?logIndex=N answer:
// {<uuid>: {body, integratedTime, logID, logIndex, verification: {signedEntryTimestamp}}}.
func parseRekorEntryResponse(data []byte) *RawTlogEntry {
	parsed, err := jsval.Parse(data)
	if err != nil {
		return nil
	}
	// Object.values(parsed)[0]
	var entry any = jsval.Undefined
	switch p := parsed.(type) {
	case *jsval.Obj:
		if keys := p.Keys(); len(keys) > 0 {
			entry = p.Get(keys[0])
		}
	case []any:
		if len(p) > 0 {
			entry = p[0]
		}
	case nil:
		return nil
	}
	body, logIndex, logID := jsval.Get(entry, "body"), jsval.Get(entry, "logIndex"), jsval.Get(entry, "logID")
	set := jsval.Path(entry, "verification", "signedEntryTimestamp")
	if !jsval.Truthy(body) || logIndex == jsval.Undefined || !jsval.Truthy(logID) || !jsval.Truthy(set) {
		return nil
	}
	kind, version, ok := kindVersionOf(jsval.String(body))
	if !ok {
		return nil
	}
	return &RawTlogEntry{
		LogIndex:             jsval.String(logIndex),
		LogIDHex:             jsval.String(logID),
		IntegratedTime:       jsval.String(jsval.Or(jsval.Get(entry, "integratedTime"), "")),
		SignedEntryTimestamp: jsval.String(set),
		CanonicalizedBody:    jsval.String(body),
		Kind:                 kind,
		Version:              version,
	}
}

// kindVersionOf decodes a canonicalized body (base64 JSON) and returns its kind and apiVersion.
func kindVersionOf(b64body string) (string, string, bool) {
	body, err := jsval.Parse(jsval.Base64(b64body))
	if err != nil {
		return "", "", false
	}
	kind, version := jsval.Get(body, "kind"), jsval.Get(body, "apiVersion")
	if !jsval.Truthy(kind) || !jsval.Truthy(version) {
		return "", "", false
	}
	return jsval.String(kind), jsval.String(version), true
}

// RekorLookup reads entries by log index. One per evidence request: entries are cached for that
// request only, so a receipt is never older than the request.
type RekorLookup interface {
	ByIndex(ctx context.Context, logIndex string) *RawTlogEntry
}

type rekorLookup struct {
	get   func(ctx context.Context, path string) ([]byte, error)
	mu    sync.Mutex
	cache map[string]*rekorCall
}

type rekorCall struct {
	once  sync.Once
	entry *RawTlogEntry
}

var digits = regexp.MustCompile(`^\d+$`)

// NewRekorLookup is Tower's createRekorLookup: get fetches a path under Rekor's base URL.
func NewRekorLookup(get func(ctx context.Context, path string) ([]byte, error)) RekorLookup {
	return &rekorLookup{get: get, cache: map[string]*rekorCall{}}
}

func (l *rekorLookup) ByIndex(ctx context.Context, logIndex string) *RawTlogEntry {
	if !digits.MatchString(logIndex) {
		return nil
	}
	l.mu.Lock()
	c, ok := l.cache[logIndex]
	if !ok {
		c = &rekorCall{}
		l.cache[logIndex] = c
	}
	l.mu.Unlock()
	c.once.Do(func() {
		body, err := l.get(ctx, "/api/v1/log/entries?logIndex="+logIndex)
		if err == nil {
			c.entry = parseRekorEntryResponse(body)
		}
	})
	return c.entry
}

// httpGetter fetches base+path and fails on anything but 200, as Tower's API-server fetch does.
func httpGetter(client *http.Client, base string) func(ctx context.Context, path string) ([]byte, error) {
	return func(ctx context.Context, path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			b := string(body)
			if len(b) > 200 {
				b = b[:200]
			}
			return nil, fmt.Errorf("request to %s failed with %d: %s", path, res.StatusCode, b)
		}
		return body, nil
	}
}

// TlogSource is where the log entry an attestation was checked against came from.
type TlogSource string

const (
	SourceRekor    TlogSource = "rekor"
	SourceEmbedded TlogSource = "embedded"
	SourceNone     TlogSource = "none"
)

type refreshed struct {
	entry    *RawTlogEntry
	source   TlogSource
	mismatch bool
}

// refreshTlogEntry: Rekor's current copy when it has the same body as the embedded one (a fresh
// receipt for the same entry), else the embedded copy. A different body at that index means the
// artifact's claim and the log disagree, which must not verify.
func refreshTlogEntry(ctx context.Context, embedded *RawTlogEntry, lookup RekorLookup) refreshed {
	if embedded == nil {
		return refreshed{source: SourceNone}
	}
	if lookup == nil {
		return refreshed{entry: embedded, source: SourceEmbedded}
	}
	fresh := lookup.ByIndex(ctx, embedded.LogIndex)
	if fresh == nil {
		return refreshed{entry: embedded, source: SourceEmbedded}
	}
	if fresh.CanonicalizedBody != embedded.CanonicalizedBody {
		return refreshed{entry: embedded, source: SourceEmbedded, mismatch: true}
	}
	return refreshed{entry: fresh, source: SourceRekor}
}

const neighbours = 8

// findHashedRekord finds a classic `cosign sign` signature's hashedrekord entry, for a signature
// whose OCI layer carries no receipt: the entries around hints (its attestations' log indexes),
// nearest first, whose body records exactly this payload hash and this signature.
func findHashedRekord(ctx context.Context, lookup RekorLookup, payloadSha256Hex, signatureB64 string, hints []float64) *RawTlogEntry {
	seen := map[float64]bool{}
	var candidates []float64
	for _, h := range hints {
		for d := 1.0; d <= neighbours; d++ {
			for _, c := range []float64{h - d, h + d} {
				if !seen[c] {
					seen[c] = true
					candidates = append(candidates, c)
				}
			}
		}
	}
	var ordered []float64
	for _, c := range candidates {
		if c >= 0 {
			ordered = append(ordered, c)
		}
	}
	dist := func(i float64) float64 {
		best := -1.0
		for _, h := range hints {
			d := i - h
			if d < 0 {
				d = -d
			}
			if best < 0 || d < best {
				best = d
			}
		}
		return best
	}
	sort.SliceStable(ordered, func(a, b int) bool { return dist(ordered[a]) < dist(ordered[b]) })
	for _, i := range ordered {
		entry := lookup.ByIndex(ctx, jsval.NumberString(i))
		if entry == nil || entry.Kind != "hashedrekord" {
			continue
		}
		body, err := jsval.Parse(jsval.Base64(entry.CanonicalizedBody))
		if err != nil {
			continue
		}
		spec := jsval.Get(body, "spec")
		if jsval.Path(spec, "data", "hash", "algorithm") == any("sha256") &&
			jsval.Path(spec, "data", "hash", "value") == any(payloadSha256Hex) &&
			jsval.Path(spec, "signature", "content") == any(signatureB64) {
			return entry
		}
	}
	return nil
}

// VerificationCode is why something did or didn't verify, as a stable code Tower turns into plain
// words. "verified" is the only success.
type VerificationCode string

// classifyVerification is Tower's, string matching on the error included.
func classifyVerification(verified bool, errMsg *string, source TlogSource, mismatch bool) VerificationCode {
	if verified {
		return "verified"
	}
	if mismatch {
		return "log-entry-mismatch"
	}
	e := ""
	if errMsg != nil {
		e = strings.ToLower(*errMsg)
	}
	switch {
	case strings.Contains(e, "trust root") || strings.Contains(e, "fulcio trust root"):
		return "trust-root-unavailable"
	case strings.Contains(e, "digest does not match"):
		return "digest-mismatch"
	case strings.Contains(e, "missing its certificate") || strings.Contains(e, "missing its"):
		return "incomplete"
	case strings.Contains(e, "timestamps") || source == SourceNone:
		return "no-log-entry"
	case strings.Contains(e, "inclusion promise") || strings.Contains(e, "checkpoint") || strings.Contains(e, "set verification") || strings.Contains(e, "tlog"):
		if source == SourceEmbedded {
			return "stale-receipt"
		}
		return "signature-invalid"
	}
	return "signature-invalid"
}
