package results

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The cache keeps runs as JSON (pack). Served, a packed run must be byte for byte what the decoded one was, and
// Get (explain's read) must hand back the fields.
func TestPackedRunServesTheSameJSON(t *testing.T) {
	r := &Run{
		Result:      "0b9d1a2e-1111-4c4c-9999-aaaaaaaaaaaa",
		PipelineRun: Obj{"metadata": Obj{"name": "p", "labels": Obj{"a": "<b&c>"}}, "status": Obj{"completionTime": "e"}},
		TaskRuns:    []Obj{{"metadata": Obj{"name": "p-build"}}, {"metadata": Obj{"name": "p-scan"}}},
		logs:        map[string]string{"p-build": "log-1"},
	}
	p, err := pack(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.PipelineRun != nil || p.TaskRuns != nil || p.logs["p-build"] != "log-1" {
		t.Fatalf("packed copy holds decoded fields or lost its logs: %+v", p)
	}
	want, _ := json.Marshal(map[string]any{"runs": []*Run{r}})
	got, _ := json.Marshal(map[string]any{"runs": []*Run{p}})
	if string(got) != string(want) {
		t.Fatalf("served JSON differs:\n got %s\nwant %s", got, want)
	}
	u, err := unpack(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(u.PipelineRun, r.PipelineRun) || len(u.TaskRuns) != 2 || u.logs["p-build"] != "log-1" {
		t.Fatalf("unpacked run differs: %+v", u)
	}
}

func TestCacheKeepsThePackedCopy(t *testing.T) {
	c := &Client{}
	r := &Run{Result: "r", PipelineRun: Obj{"status": Obj{"completionTime": "e"}}, TaskRuns: []Obj{}}
	if got := c.cacheRun("ns", r); got.raw == nil {
		t.Fatal("cacheRun returned the decoded run")
	}
	v, ok := c.cache.get(runKey("ns", "r"))
	if !ok || v.(*Run).raw == nil || v.(*Run).PipelineRun != nil {
		t.Fatalf("cache holds %+v", v)
	}
}
