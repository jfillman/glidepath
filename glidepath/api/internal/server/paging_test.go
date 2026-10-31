package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPaginateKeyset(t *testing.T) {
	id := func(s string) string { return s }
	items := []string{"b/2", "a/1", "c/3", "a/2"}
	page, next := paginate(items, id, pageReq{limit: 2})
	if !slices.Equal(page, []string{"a/1", "a/2"}) || next == "" {
		t.Fatalf("first page %v %q", page, next)
	}
	// An item before the cursor appears and one after it goes: the next page neither repeats nor skips.
	r := httptest.NewRequest(http.MethodGet, "/v1/releases?limit=2&continue="+next, nil)
	p, err := readPage(r)
	if err != nil {
		t.Fatal(err)
	}
	page, next = paginate([]string{"a/0", "a/1", "a/2", "c/3", "d/4"}, id, p)
	if !slices.Equal(page, []string{"c/3", "d/4"}) || next != "" {
		t.Fatalf("second page %v %q", page, next)
	}
	if page, next := paginate(items, id, pageReq{}); !slices.Equal(page, items) || next != "" {
		t.Fatalf("no limit is the whole list, unsorted: %v %q", page, next)
	}
}

func TestReadPageRefuses(t *testing.T) {
	for _, q := range []string{"limit=0", "limit=1001", "limit=x", "continue=YQ", "limit=5&continue=***"} {
		if _, err := readPage(httptest.NewRequest(http.MethodGet, "/v1/live?"+q, nil)); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}

func TestFleetReleasesPagedWithETag(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	tower := map[string]string{"Authorization": "Bearer t-tower"}

	_, first, _ := call(t, s, "/v1/releases?limit=1", tower)
	recs := first["releases"].([]any)
	cont, _ := first["continue"].(string)
	if len(recs) != 1 || recs[0].(map[string]any)["name"] != "release-tracking-other-1" || cont == "" {
		t.Fatalf("first page: %v", first)
	}
	_, second, _ := call(t, s, "/v1/releases?limit=1&continue="+cont, tower)
	recs = second["releases"].([]any)
	if len(recs) != 1 || recs[0].(map[string]any)["name"] != "release-tracking-shop-1" || second["continue"] != nil {
		t.Fatalf("last page: %v", second)
	}
	if st, out, _ := call(t, s, "/v1/releases?limit=1001", tower); st != 400 || out["rule"] != "GP-API-400" {
		t.Fatalf("bad limit: %d %v", st, out)
	}

	// The same answer twice: the ETag comes back, and sending it gets 304 with no body.
	req := httptest.NewRequest(http.MethodGet, "/v1/releases", nil)
	req.Header.Set("Authorization", "Bearer t-tower")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	etag := rec.Header().Get("ETag")
	if rec.Code != 200 || etag == "" {
		t.Fatalf("whole list: %d etag %q", rec.Code, etag)
	}
	req.Header.Set("If-None-Match", `"other", `+etag)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Fatalf("revalidation: %d %q", rec.Code, rec.Body.String())
	}
	// A caller who may not see the list gets no 304, whatever it sends.
	req.Header.Set("Authorization", "Bearer t-agent")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized revalidation: %d", rec.Code)
	}
}
