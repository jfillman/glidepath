package evidence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Ported from Tower's rekorEntries.test.ts, case for case.

func b64j(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func response(logIndex int, body any, logID string) []byte {
	b, _ := json.Marshal(map[string]any{"uuid": map[string]any{"body": b64j(body), "integratedTime": 1700000000, "logID": logID,
		"logIndex": logIndex, "verification": map[string]any{"signedEntryTimestamp": "SET"}}})
	return b
}

var intoto = map[string]any{"kind": "intoto", "apiVersion": "0.0.2", "spec": map[string]any{"content": map[string]any{"payloadHash": map[string]any{"value": "p"}}}}

func hashed(hash, sig string) map[string]any {
	return map[string]any{"kind": "hashedrekord", "apiVersion": "0.0.1", "spec": map[string]any{
		"data": map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": hash}}, "signature": map[string]any{"content": sig}}}
}

func TestParseRekorEntryResponse(t *testing.T) {
	// Rekor's entry answer, with the receipt it signed now.
	got := parseRekorEntryResponse(response(2879, intoto, "beef"))
	want := RawTlogEntry{LogIndex: "2879", LogIDHex: "beef", IntegratedTime: "1700000000", SignedEntryTimestamp: "SET",
		CanonicalizedBody: b64j(intoto), Kind: "intoto", Version: "0.0.2"}
	if got == nil || *got != want {
		t.Fatalf("%+v", got)
	}
	if parseRekorEntryResponse([]byte("{}")) != nil || parseRekorEntryResponse([]byte("not json")) != nil {
		t.Fatal("no entry")
	}
}

func lookupOf(body []byte) RekorLookup {
	return NewRekorLookup(func(context.Context, string) ([]byte, error) {
		if body == nil {
			return nil, errors.New("down")
		}
		return body, nil
	})
}

func TestRefreshTlogEntry(t *testing.T) {
	ctx := context.Background()
	embedded := &RawTlogEntry{LogIndex: "2879", LogIDHex: "old-key", IntegratedTime: "1700000000", SignedEntryTimestamp: "OLD-SET",
		CanonicalizedBody: b64j(intoto), Kind: "intoto", Version: "0.0.2"}
	// Rekor's current copy of the same entry (found live: receipts signed by a Rekor key lost on restart).
	r := refreshTlogEntry(ctx, embedded, lookupOf(response(2879, intoto, "new-key")))
	if r.source != SourceRekor || r.entry.LogIDHex != "new-key" || r.entry.SignedEntryTimestamp != "SET" {
		t.Fatalf("%+v", r)
	}
	// An index whose entry is a different body is refused; an unreachable Rekor keeps the embedded copy.
	if r := refreshTlogEntry(ctx, embedded, lookupOf(response(2879, hashed("x", "y"), "aa"))); r.source != SourceEmbedded || !r.mismatch {
		t.Fatalf("%+v", r)
	}
	if r := refreshTlogEntry(ctx, embedded, lookupOf(nil)); r.source != SourceEmbedded || r.entry != embedded || r.mismatch {
		t.Fatalf("%+v", r)
	}
	if r := refreshTlogEntry(ctx, nil, lookupOf(nil)); r.source != SourceNone || r.entry != nil {
		t.Fatalf("%+v", r)
	}
}

func TestFindHashedRekord(t *testing.T) {
	// A signature's entry next to its attestation's, by exact payload hash and signature.
	log := map[int]any{99: intoto, 98: hashed("other", "sig"), 101: hashed("payload", "sig"), 102: hashed("payload", "other-sig")}
	var paths []string
	lookup := NewRekorLookup(func(_ context.Context, p string) ([]byte, error) {
		paths = append(paths, p)
		i, _ := strconv.Atoi(strings.SplitN(p, "logIndex=", 2)[1])
		if body, ok := log[i]; ok {
			return response(i, body, "aa"), nil
		}
		return []byte("{}"), nil
	})
	ctx := context.Background()
	if e := findHashedRekord(ctx, lookup, "payload", "sig", []float64{100}); e == nil || e.LogIndex != "101" {
		t.Fatalf("%+v", e)
	}
	if e := findHashedRekord(ctx, lookup, "payload", "nope", []float64{100}); e != nil {
		t.Fatalf("%+v", e)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/api/v1/log/entries?logIndex=") {
			t.Fatal(p)
		}
	}
	// Nearest first: 99 and 101 before 98 and 102.
	if strings.Join(paths[:4], " ") != "/api/v1/log/entries?logIndex=99 /api/v1/log/entries?logIndex=101 /api/v1/log/entries?logIndex=98 /api/v1/log/entries?logIndex=102" {
		t.Fatal(paths[:4])
	}
}

func TestClassifyVerification(t *testing.T) {
	// What sigstore said, as a code Tower explains.
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		verified bool
		err      *string
		source   TlogSource
		mismatch bool
		want     VerificationCode
	}{
		{true, nil, SourceRekor, false, "verified"},
		{false, s("inclusion promise could not be verified"), SourceEmbedded, false, "stale-receipt"},
		{false, s("invalid checkpoint signature"), SourceEmbedded, false, "stale-receipt"},
		{false, s("expected 1 timestamps, got 0"), SourceNone, false, "no-log-entry"},
		{false, nil, SourceEmbedded, true, "log-entry-mismatch"},
		{false, s("signature payload digest does not match this image"), SourceRekor, false, "digest-mismatch"},
		{false, s("could not load Fulcio trust root: x"), SourceNone, false, "trust-root-unavailable"},
		{false, s("signature verification failed"), SourceRekor, false, "signature-invalid"},
	} {
		if got := classifyVerification(c.verified, c.err, c.source, c.mismatch); got != c.want {
			t.Errorf("%v: %s, want %s", c.err, got, c.want)
		}
	}
}
