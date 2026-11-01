package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// The fleet lists (GET /v1/live, /v1/releases, /v1/releases/events) page and carry an ETag (docs/admin/api.md#paging).
//
// Without limit a list is whole, in the order it always had. With limit it is sorted by its key (namespace, then
// name) and cut after `limit` items; `continue` is the last key served, so the next page starts after it even if
// items came or went in between (keyset, never an offset). A list's ETag is a hash of the answer's bytes: a poller
// that sends it back in If-None-Match gets 304 and no body while nothing changed.

const maxPageLimit = 1000

type pageReq struct {
	limit int    // 0: the whole list
	after string // the key the previous page ended on
}

type pageInvalid struct{ what string }

func (e pageInvalid) Error() string { return e.what }

func readPage(r *http.Request) (pageReq, error) {
	q := r.URL.Query()
	var p pageReq
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			return p, pageInvalid{fmt.Sprintf("limit must be an integer from 1 to %d", maxPageLimit)}
		}
		p.limit = n
	}
	if v := q.Get("continue"); v != "" {
		if p.limit == 0 {
			return p, pageInvalid{"continue needs the limit the previous page was asked with"}
		}
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(b) == 0 {
			return p, pageInvalid{"continue is not a token this API gave"}
		}
		p.after = string(b)
	}
	return p, nil
}

// paginate returns the page of items p asks for and the continue token for the next one ("" on the last page).
func paginate[T any](items []T, key func(T) string, p pageReq) ([]T, string) {
	if p.limit == 0 {
		return items, ""
	}
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(a, b T) int { return strings.Compare(key(a), key(b)) })
	start := 0
	if p.after != "" {
		start, _ = slices.BinarySearchFunc(sorted, p.after, func(it T, k string) int { return strings.Compare(key(it), k) })
		for start < len(sorted) && key(sorted[start]) <= p.after {
			start++
		}
	}
	end := min(start+p.limit, len(sorted))
	page := sorted[start:end]
	if end == len(sorted) {
		return page, ""
	}
	return page, base64.RawURLEncoding.EncodeToString([]byte(key(page[len(page)-1])))
}

// writeList answers a fleet list: 304 when the caller's If-None-Match names this answer's ETag, else the body with it.
func writeList(w http.ResponseWriter, r *http.Request, body map[string]any, next string) {
	if next != "" {
		body["continue"] = next
	}
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	sum := sha256.Sum256(buf.Bytes())
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	// Every caller revalidates: the list is per-identity (what it may see) and changes every few seconds.
	w.Header().Set("Cache-Control", "private, no-cache")
	if matches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// matches reads If-None-Match leniently: weak (W/) and unquoted values count, since a hand-typed header (curl) or a
// client that strips quotes means the same answer.
func matches(inm, etag string) bool {
	bare := strings.Trim(etag, `"`)
	for _, t := range strings.Split(inm, ",") {
		t = strings.Trim(strings.TrimPrefix(strings.TrimSpace(t), "W/"), `"`)
		if t == bare || t == "*" {
			return true
		}
	}
	return false
}

func objKey(o map[string]any) string {
	md, _ := o["metadata"].(map[string]any)
	ns, _ := md["namespace"].(string)
	name, _ := md["name"].(string)
	return ns + "/" + name
}
