package results

import (
	"encoding/json"
	"testing"
)

// Tower's TypeScript keeps null and "" and omits only absent fields; these pin the Go side to that.

func TestPickKeepsNull(t *testing.T) {
	got, _ := json.Marshal(pick(Obj{"a": nil, "b": 1}, "a", "b", "c"))
	if string(got) != `{"a":null,"b":1}` {
		t.Fatal(string(got))
	}
}

func TestRowEmptyVersusAbsent(t *testing.T) {
	var s summary
	json.Unmarshal([]byte(`{"name":"p/results/r1","annotations":{"object.metadata.name":"","tekton.dev/pipeline":""},
		"summary":{"type":"tekton.dev/v1.PipelineRun","status":"SUCCESS","start_time":"","annotations":{"commit":""}}}`), &s)
	row, ok := toRow(s)
	if !ok {
		t.Fatal("not a PipelineRun row")
	}
	got, _ := json.Marshal(row)
	want := `{"result":"r1","name":"","pipeline":"","status":"succeeded","startTime":"","commit":""}`
	if string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	var s2 summary
	json.Unmarshal([]byte(`{"name":"p/results/r2","summary":{"type":"tekton.dev/v1.PipelineRun"}}`), &s2)
	row, _ = toRow(s2)
	got, _ = json.Marshal(row)
	if string(got) != `{"result":"r2","name":"r2","status":"unknown"}` {
		t.Fatalf("absent name falls back to the result id: %s", got)
	}
}
