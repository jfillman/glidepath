package environments

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/releases"
)

// Tower's glidepathRollback.test.ts, case for case.
func rec(id string, data map[string]string) releases.Record {
	d := map[string]string{"releaseId": id + ":kind-prod/staging", "gitUrl": "https://github.com/o/sky-marshall.git", "gitRevision": id + "0000000"}
	for k, v := range data {
		d[k] = v
	}
	return releases.Record{Name: "release-tracking-" + id, Data: d}
}

func img(n int) string { return fmt.Sprintf("ghcr.io/o/sky-marshall:0.1.%d-abc", n) }

func ids(ts []Target, image bool) []string {
	out := []string{}
	for _, t := range ts {
		if image {
			out = append(out, t.Image)
		} else {
			out = append(out, t.ReleaseID)
		}
	}
	return out
}

func TestPlanRollback(t *testing.T) {
	plan := PlanRollback([]releases.Record{
		rec("r1", map[string]string{"state": "superseded", "image": img(1), "healthyAt": "2026-10-01T00:00:00Z", "prCreatedAt": "2026-10-01T00:00:00Z"}),
		rec("r2", map[string]string{"state": "superseded", "image": img(2), "healthyAt": "2026-10-02T00:00:00Z", "prCreatedAt": "2026-10-02T00:00:00Z"}),
		rec("r3", map[string]string{"state": "aborted", "image": img(3), "prCreatedAt": "2026-10-03T00:00:00Z"}),
	})
	if plan.Current == nil || *plan.Current.ReleaseID != "r3:kind-prod/staging" || plan.Current.State != "aborted" || *plan.Current.Image != img(3) {
		t.Fatalf("current: %+v", plan.Current)
	}
	if got := ids(plan.Targets, false); strings.Join(got, ",") != "r2:kind-prod/staging,r1:kind-prod/staging" {
		t.Fatalf("targets: %v", got)
	}

	// The same five-release window the gates check.
	var recs []releases.Record
	for i := 1; i <= 6; i++ {
		recs = append(recs, rec(fmt.Sprintf("r%d", i), map[string]string{"state": "superseded", "image": img(i),
			"healthyAt": fmt.Sprintf("2026-10-0%dT00:00:00Z", i), "prCreatedAt": fmt.Sprintf("2026-10-0%dT00:00:00Z", i)}))
	}
	recs = append(recs, rec("r7", map[string]string{"state": "progressing", "image": img(7), "prCreatedAt": "2026-10-08T00:00:00Z"}))
	if got := ids(PlanRollback(recs).Targets, true); strings.Join(got, ",") != strings.Join([]string{img(6), img(5), img(4), img(3), img(2)}, ",") {
		t.Fatalf("window: %v", got)
	}

	// Never the current image, a duplicate image, or a record from before phase 4.
	plan = PlanRollback([]releases.Record{
		rec("old", map[string]string{"state": "superseded", "healthyAt": "2026-10-01T00:00:00Z", "prCreatedAt": "2026-10-01T00:00:00Z"}),
		rec("a", map[string]string{"state": "superseded", "image": img(1), "healthyAt": "2026-10-02T00:00:00Z", "prCreatedAt": "2026-10-02T00:00:00Z"}),
		rec("b", map[string]string{"state": "superseded", "image": img(1), "healthyAt": "2026-10-03T00:00:00Z", "prCreatedAt": "2026-10-03T00:00:00Z"}),
		rec("cur", map[string]string{"state": "healthy", "image": img(1), "healthyAt": "2026-10-04T00:00:00Z", "prCreatedAt": "2026-10-04T00:00:00Z"}),
	})
	if *plan.Current.ReleaseID != "cur:kind-prod/staging" || len(plan.Targets) != 0 {
		t.Fatalf("exclusions: %+v", plan)
	}

	// Nothing ran: no current, and the JSON says so the way Tower's does.
	plan = PlanRollback([]releases.Record{rec("p", map[string]string{"state": "proposed", "image": img(1), "prCreatedAt": "2026-10-01T00:00:00Z"})})
	if raw, _ := json.Marshal(plan); string(raw) != `{"targets":[]}` {
		t.Fatalf("nothing ran: %s", raw)
	}
}

// A field the record lacks is absent from Tower's JSON, never "": a current release with no image or stateAt, and a
// target with no state.
func TestRollbackPlanJSONOmitsMissing(t *testing.T) {
	plan := PlanRollback([]releases.Record{
		{Name: "x", Data: map[string]string{"state": "healthy", "prCreatedAt": "2026-10-02T00:00:00Z"}},
		{Name: "y", Data: map[string]string{"releaseId": "y", "image": "i:1-abcdef0", "gitRevision": "g", "gitUrl": "u", "healthyAt": "2026-10-01T00:00:00Z"}},
	})
	raw, _ := json.Marshal(plan)
	if string(raw) != `{"current":{"state":"healthy"},"targets":[{"releaseId":"y","image":"i:1-abcdef0","gitRevision":"g","gitUrl":"u","healthyAt":"2026-10-01T00:00:00Z"}]}` {
		t.Fatalf("%s", raw)
	}
}

// Both sorts are stable, like V8's: equal times keep the records' list order, and an unparsable time sorts as 0.
func TestPlanRollbackTies(t *testing.T) {
	plan := PlanRollback([]releases.Record{
		rec("a", map[string]string{"state": "healthy", "image": img(1), "healthyAt": "2026-10-01T00:00:00Z", "prCreatedAt": "2026-10-01T00:00:00Z"}),
		rec("b", map[string]string{"state": "healthy", "image": img(2), "healthyAt": "2026-10-01T00:00:00Z", "prCreatedAt": "2026-10-01T00:00:00Z"}),
		rec("c", map[string]string{"state": "healthy", "image": img(3), "healthyAt": "not a time", "prCreatedAt": "later"}),
	})
	if *plan.Current.ReleaseID != "a:kind-prod/staging" || strings.Join(ids(plan.Targets, false), ",") != "b:kind-prod/staging,c:kind-prod/staging" {
		t.Fatalf("%+v %v", plan.Current, ids(plan.Targets, false))
	}
}

func plan(t *testing.T, y string) *Plan {
	t.Helper()
	doc, err := unmarshal(y)
	if err != nil {
		t.Fatal(err)
	}
	return planOf(doc)
}

func TestPlanOf(t *testing.T) {
	p := plan(t, `
deploy:
  releaseFile: glidepath/envs/{env}.release.yaml
  environments:
    - { name: dev, tier: ground }
    - { name: staging, tier: flight }
    - { name: prod, tier: flight, cluster: hangar-prod, production: true }
`)
	raw, _ := json.Marshal(p)
	if string(raw) != `{"order":["dev","staging","prod"],"orderSource":"promotionOrder","lower":["dev"],"upper":[{"name":"staging"},{"name":"prod","cluster":"hangar-prod","production":true}],"releaseFile":"glidepath/envs/{env}.release.yaml"}` {
		t.Fatalf("%s", raw)
	}

	// An environment with neither tier makes the declared order untrusted: the release pipeline's steps decide, the
	// first-defined pipeline winning a tie (Tower's strict >).
	p = plan(t, `
deploy:
  environments:
    - { name: dev, tier: ground }
    - { name: qa, tier: lab }
pipelines:
  main:
    steps:
      - { stage: build }
      - { stage: deploy, env: dev }
      - { stage: release, env: staging }
      - { stage: release, env: staging }
  hotfix:
    steps:
      - { stage: deploy, env: prod }
      - { stage: release, env: dev }
      - { stage: test, env: qa }
      - { stage: deploy, env: qa }
`)
	if p.OrderSource != "pipeline-steps" || strings.Join(p.Order, ",") != "dev,staging" {
		t.Fatalf("fallback: %+v", p)
	}
	if _, err := unmarshal("a: 1\na: 2\n"); err == nil {
		t.Fatal("a duplicate key parsed")
	}
}

func TestExtractShortSha(t *testing.T) {
	for tag, want := range map[string]string{"1.2.3-abc1234": "abc1234", "abcdef012345": "abcdef012345", "0.1.0-ABC1234": "ABC1234"} {
		if got, err := ExtractShortSha(tag); err != nil || got != want {
			t.Errorf("%s: %s %v", tag, got, err)
		}
	}
	for _, tag := range []string{"latest", "1.2.3", "1.2.3-abc12"} {
		if _, err := ExtractShortSha(tag); err == nil {
			t.Errorf("%s: no error", tag)
		}
	}
}

// Tower's glidepathReleaseRun.test.ts: instantiate and runName.
func TestInstantiateAndRunName(t *testing.T) {
	resource := map[string]any{"kind": "PipelineRun",
		"metadata": map[string]any{"name": "$(tt.params.pipeline-run-name)", "annotations": map[string]any{"hangar.io/initiator": "$(tt.params.initiator)"}},
		"spec": map[string]any{"params": []any{map[string]any{"name": "env", "value": "$(tt.params.env)"}, map[string]any{"name": "app-name", "value": "shop"}},
			"workspaces": []any{map[string]any{"name": "source", "emptyDir": map[string]any{}}}}}
	out, err := Instantiate(resource, map[string]string{"pipeline-run-name": "promote-prod-abc", "initiator": "user:default/jf", "env": "prod"}, PromoteTemplate)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if string(raw) != `{"kind":"PipelineRun","metadata":{"annotations":{"hangar.io/initiator":"user:default/jf"},"name":"promote-prod-abc"},"spec":{"params":[{"name":"env","value":"prod"},{"name":"app-name","value":"shop"}],"workspaces":[{"emptyDir":{},"name":"source"}]}}` {
		t.Fatalf("%s", raw)
	}
	if _, err := Instantiate(resource, map[string]string{"pipeline-run-name": "x", "initiator": "y"}, PromoteTemplate); err == nil || !strings.Contains(err.Error(), "needs param 'env'") {
		t.Fatalf("missing param: %v", err)
	}

	h := KeyHash("shop", "promote", "pre-prod", "c", "k")
	if n := RunName("promote", "pre-prod", "a1a29e9f0123", h); !regexp.MustCompile(`^promote-pre-prod-a1a29e9f-[0-9a-f]{6}$`).MatchString(n) {
		t.Fatalf("name %s", n)
	}
	if n := RunName("promote", strings.Repeat("x", 31), strings.Repeat("a", 40), h); len(n) > 63 {
		t.Fatalf("too long: %s", n)
	}
	// Tower's release tracker (isReleaseRunName) follows the runs the API names.
	tower := regexp.MustCompile(`^(promote|rollback)-[a-z0-9-]{1,56}$`)
	if !tower.MatchString(RunName("rollback", "prod", "abcdef0123456789", h)) {
		t.Fatal("Tower's release tracker would refuse the name")
	}
}
