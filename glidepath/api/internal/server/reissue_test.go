package server

// Reissue (docs/admin/api.md#reissue-a-release-pr): a release PR closed without merging is replaced by a fresh one. Every
// refusal, the happy path, replay, the one-reissue-per-release rule, authorization, and the list of candidates.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	oldChain  = "11111111-2222-4333-8444-555555555555"
	newChain  = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	oldPR     = "https://github.com/jfillman/gitops-shop/pull/7"
	oldBranch = "release-prod-abc1234d" // short(devSHA)
)

var reissuers = map[string]bool{
	"rel promote environments app-shop-cicd": true, "rel list releases app-shop-cicd": true, "rel get releases app-shop-cicd": true,
	"reader list releases app-shop-cicd": true, "reader get releases app-shop-cicd": true,
	"op rerun runs app-shop-cicd": true,
	"tower *":                     true,
}

func pull(n int, state string, merged bool, head string) map[string]any {
	return map[string]any{"number": n, "state": state, "merged": merged, "html_url": fmt.Sprintf("https://github.com/jfillman/gitops-shop/pull/%d", n),
		"head": map[string]any{"ref": head}}
}

// closedRelease is the record of a promotion whose PR #7 was closed without merging.
func closedRelease(over map[string]string) map[string]string {
	d := map[string]string{"releaseId": oldChain + ":hangar-prod/prod", "kind": "promote", "state": "proposed", "stateAt": "2026-11-10T10:00:00Z",
		"image": "ghcr.io/jfillman/shop:1.2.3-abc1234", "gitRevision": devSHA, "gitUrl": "https://github.com/jfillman/shop",
		"env": "prod", "cluster": "hangar-prod", "prUrl": oldPR, "prCreatedAt": "2026-11-10T10:00:00Z"}
	for k, v := range over {
		d[k] = v
	}
	return d
}

// withReissue is withEnvs with the records replaced by recs and PR #7 closed unmerged.
func withReissue(t *testing.T, allow map[string]bool, recs ...runtime.Object) (*Server, *fakeStore, *ghStub, *strings.Builder) {
	s, store, gh, logs := withEnvs(t, allow)
	s.Envs.Records = releases.Reader{CS: fake.NewSimpleClientset(recs...)}
	gh.pulls["jfillman/gitops-shop#7"] = pull(7, "closed", false, oldBranch)
	return s, store, gh, logs
}

func reissuePath(chain string) string { return "/v1/apps/shop/releases/" + chain + "/reissue" }

func TestReissueHappyPathReplayAndOnce(t *testing.T) {
	s, store, _, logs := withReissue(t, reissuers, record(oldChain, prodLabels, closedRelease(nil)))
	h := hdr("rel", "k1")
	// The body is ignored: nothing in it chooses an image, a commit, an environment or a chain.
	st, out, body := postJSON(t, s, reissuePath(oldChain), h, `{"image":"evil:1","gitRevision":"deadbeef","env":"staging","cluster":"x","chainId":"zzz"}`)
	if st != 201 || out["reissues"] != oldChain || out["env"] != "prod" || out["linked"] != true || out["closedPrUrl"] != oldPR {
		t.Fatalf("reissue: %d %s", st, body)
	}
	name := out["pipelineRun"].(string)
	if !strings.HasPrefix(name, "promote-prod-abc1234d-") {
		t.Fatalf("name %s", name)
	}
	run := store.objs[name]
	for p, v := range map[string]string{"image-ref": "ghcr.io/jfillman/shop:1.2.3-abc1234", "git-revision": devSHA, "git-url": "https://github.com/jfillman/shop",
		"env": "prod", "cluster": "hangar-prod", "initiator": "rel"} {
		if param(run, p) != v {
			t.Errorf("param %s = %v, want %s", p, param(run, p), v)
		}
	}
	ann := run["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[pipelineruns.AnnotationReissueOf] != oldChain || ann[pipelineruns.AnnotationOriginRef] != "refs/heads/main" || ann[pipelineruns.AnnotationOriginSha] != devSHA {
		t.Fatalf("a reissue stamps the closed release's chain and the origin as promote does: %v", ann)
	}
	if run["metadata"].(map[string]any)["labels"].(map[string]any)["hangar.io/flow"] != "promote" || run["spec"].(map[string]any)["taskRunTemplate"].(map[string]any)["serviceAccountName"] != "pipeline-runner" {
		t.Fatalf("the app's own promote template, unchanged: %v", run["metadata"])
	}
	if !strings.Contains(logs.String(), `"op":"reissueRelease"`) || !strings.Contains(logs.String(), `"reissues":"`+oldChain+`"`) {
		t.Fatalf("audit: %s", logs)
	}

	// The same key replays; another key is refused, naming the run (a release is reissued once).
	if st, out, _ := postJSON(t, s, reissuePath(oldChain), h, ""); st != 200 || out["replayed"] != true || out["pipelineRun"] != name {
		t.Fatalf("replay: %d %v", st, out)
	}
	before := len(store.objs)
	st, out, body = postJSON(t, s, reissuePath(oldChain), hdr("rel", "k2"), "")
	if st != 422 || !strings.Contains(body, "was already reissued (run "+name+")") || len(store.objs) != before {
		t.Fatalf("second reissue: %d %s", st, body)
	}
}

func TestReissueDryRunWritesNothing(t *testing.T) {
	s, store, _, _ := withReissue(t, reissuers, record(oldChain, prodLabels, closedRelease(nil)))
	h := hdr("rel", "kd")
	h["X-Glidepath-Dry-Run"] = "true"
	st, out, body := postJSON(t, s, reissuePath(oldChain), h, "")
	if st != 200 || out["dryRun"] != true || out["object"] == nil || len(store.writes()) != 0 {
		t.Fatalf("dry run: %d %s writes=%v", st, body, store.writes())
	}
	// A dry run refuses what the real call would.
	s, _, _, _ = withReissue(t, reissuers, record(oldChain, prodLabels, closedRelease(map[string]string{"state": "healthy"})))
	if st, _, _ := postJSON(t, s, reissuePath(oldChain), h, ""); st != 422 {
		t.Fatalf("dry run of a refusable reissue: %d", st)
	}
}

func TestReissueRefusals(t *testing.T) {
	newer := record(newChain, prodLabels, closedRelease(map[string]string{"releaseId": newChain + ":hangar-prod/prod", "prCreatedAt": "2026-11-11T10:00:00Z", "state": "proposed", "prUrl": "https://github.com/jfillman/gitops-shop/pull/8"}))
	mergedLater := record(newChain, prodLabels, closedRelease(map[string]string{"releaseId": newChain + ":hangar-prod/prod", "prCreatedAt": "2026-11-09T10:00:00Z", "state": "healthy", "mergedAt": "2026-11-10T11:00:00Z"}))
	for name, c := range map[string]struct {
		over   map[string]string
		extra  *recordFixture
		pr     map[string]any // replaces PR #7
		open   map[string]any // another open PR
		cicd   string
		status int
		in     string
	}{
		"merged state":            {over: map[string]string{"state": "merged"}, status: 422, in: "is merged: only a release whose PR was closed without merging"},
		"healthy":                 {over: map[string]string{"state": "healthy"}, status: 422, in: "is healthy"},
		"progressing":             {over: map[string]string{"state": "progressing"}, status: 422, in: "is progressing"},
		"superseded":              {over: map[string]string{"state": "superseded"}, status: 422, in: "is superseded"},
		"rolled back":             {over: map[string]string{"state": "rolled-back"}, status: 422, in: "is rolled-back"},
		"PR still open":           {pr: pull(7, "open", false, oldBranch), status: 422, in: "release PR #7 is still open"},
		"PR merged":               {pr: pull(7, "closed", true, oldBranch), status: 422, in: "release PR #7 was merged"},
		"another PR is open":      {open: pull(9, "open", false, "release-prod-1234567a"), status: 422, in: "a release PR for prod is already open (https://github.com/jfillman/gitops-shop/pull/9)"},
		"a newer release":         {extra: &recordFixture{newer}, status: 422, in: "a newer release of shop to prod on hangar-prod was opened since"},
		"merged since":            {extra: &recordFixture{mergedLater}, status: 422, in: "was merged after this one was opened"},
		"a rollback":              {over: map[string]string{"kind": "rollback"}, status: 422, in: "is a rollback: only a promotion's PR can be reissued"},
		"another app's image":     {over: map[string]string{"image": "ghcr.io/jfillman/other:1.2.3-abc1234"}, status: 422, in: "not an image of shop"},
		"image of another commit": {over: map[string]string{"image": "ghcr.io/jfillman/shop:1.2.3-ffff000"}, status: 422, in: "the image's own sha is not that commit"},
		"a short commit":          {over: map[string]string{"gitRevision": "abc1234"}, status: 422, in: "records no full commit sha"},
		"another source repo":     {over: map[string]string{"gitUrl": "https://github.com/jfillman/other"}, status: 422, in: "records source"},
		"another repo's PR":       {over: map[string]string{"prUrl": "https://github.com/jfillman/other/pull/7"}, status: 422, in: "records no release PR in jfillman/gitops-shop"},
		"no PR":                   {over: map[string]string{"prUrl": ""}, status: 422, in: "records no release PR"},
		"another branch":          {pr: pull(7, "closed", false, "feature-x"), status: 422, in: `feature-x`},
		"no creation time":        {over: map[string]string{"prCreatedAt": ""}, status: 422, in: "no PR creation time"},
		"no image":                {over: map[string]string{"image": ""}, status: 422, in: "records no image"},
		"env left the order":      {cicd: "deploy:\n  environments:\n    - { name: dev, tier: ground }\n    - { name: staging, tier: flight, cluster: hangar-prod }\n", status: 422, in: "no longer in shop's cicd.yaml release order"},
		"a ground env":            {cicd: "deploy:\n  environments:\n    - { name: prod, tier: ground }\n", status: 422, in: "is a Ground environment"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := []runtime.Object{record(oldChain, prodLabels, closedRelease(c.over))}
			if c.extra != nil {
				objs = append(objs, c.extra.cm)
			}
			s, store, gh, _ := withReissue(t, reissuers, objs...)
			if c.pr != nil {
				gh.pulls["jfillman/gitops-shop#7"] = c.pr
			}
			if c.open != nil {
				gh.pulls["jfillman/gitops-shop#9"] = c.open
			}
			if c.cicd != "" {
				gh.files["jfillman/shop/cicd.yaml"] = c.cicd
			}
			st, out, body := postJSON(t, s, reissuePath(oldChain), hdr("rel", "k-"+strings.NewReplacer(" ", "-", "'", "").Replace(name)), "")
			if st != c.status || !strings.Contains(body, c.in) || out["rule"] != "GP-API-422" {
				t.Fatalf("%d %s\nwant %d containing %q", st, body, c.status, c.in)
			}
			if len(store.writes()) != 0 {
				t.Fatalf("a refused reissue starts nothing: %v", store.writes())
			}
		})
	}

	// A release that does not exist for the app, and a malformed chain id.
	s, _, _, _ := withReissue(t, reissuers, record(oldChain, prodLabels, closedRelease(nil)))
	if st, _, _ := postJSON(t, s, reissuePath(newChain), hdr("rel", "k-nf"), ""); st != 404 {
		t.Fatalf("no such release: %d", st)
	}
	if st, _, _ := postJSON(t, s, reissuePath("Bad_Chain"), hdr("rel", "k-bad"), ""); st != 400 {
		t.Fatalf("malformed chain id: %d", st)
	}
	// Another app's record is not found through this app.
	other := record(newChain, map[string]string{"hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod", "hangar.io/app": "other"}, closedRelease(nil))
	s, _, _, _ = withReissue(t, reissuers, other)
	if st, _, _ := postJSON(t, s, reissuePath(newChain), hdr("rel", "k-other"), ""); st != 404 {
		t.Fatalf("another app's record: %d", st)
	}
	// No Idempotency-Key, or a bad one.
	if st, _, _ := postJSON(t, s, reissuePath(oldChain), hdr("rel", ""), ""); st != 400 {
		t.Fatalf("no key: %d", st)
	}
}

// recordFixture wraps a second ConfigMap so the table above can carry one.
type recordFixture struct{ cm runtime.Object }

func TestReissueRefusesAnOlderReleaseWhenANewerOneIsOpen(t *testing.T) {
	// The newer release's PR is open: it is the one to merge; the closed older one is not reissuable.
	s, _, gh, _ := withReissue(t, reissuers,
		record(oldChain, prodLabels, closedRelease(nil)),
		record(newChain, prodLabels, closedRelease(map[string]string{"releaseId": newChain + ":hangar-prod/prod", "prCreatedAt": "2026-11-12T10:00:00Z", "prUrl": "https://github.com/jfillman/gitops-shop/pull/8"})))
	gh.pulls["jfillman/gitops-shop#8"] = pull(8, "open", false, "release-prod-1234567a")
	st, _, body := postJSON(t, s, reissuePath(oldChain), hdr("rel", "k1"), "")
	if st != 422 || !strings.Contains(body, "newer release") {
		t.Fatalf("%d %s", st, body)
	}
}

func TestReissueAuthorization(t *testing.T) {
	allow := map[string]bool{
		"reader get environments app-shop-cicd": true, "reader list releases app-shop-cicd": true,
		"promoter promote environments app-shop-cicd":    true,
		"op rerun runs app-shop-cicd":                    true,
		"rollbacker rollback environments app-shop-cicd": true,
		"tower *": true,
	}
	s, store, _, logs := withReissue(t, allow, record(oldChain, prodLabels, closedRelease(nil)))
	for _, c := range []struct {
		user   string
		status int
	}{{"reader", 403}, {"op", 403}, {"rollbacker", 403}} {
		st, out, body := postJSON(t, s, reissuePath(oldChain), hdr(c.user, "k-"+c.user), "")
		if st != c.status {
			t.Fatalf("%s: %d %s", c.user, st, body)
		}
		if st == 403 && (out["rule"] != "GP-AUTHZ-001" || !strings.Contains(out["hint"].(string), "glidepath-api-releaser")) {
			t.Fatalf("hint names the releaser role: %v", out)
		}
	}
	if len(store.writes()) != 0 {
		t.Fatalf("a refused caller starts nothing: %v", store.writes())
	}
	// Tower may reissue, but only naming the person, who is then the run's initiator.
	if st, out, _ := postJSON(t, s, reissuePath(oldChain), hdr("tower", "k-t1"), ""); st != 400 || out["rule"] != "GP-AUTH-004" {
		t.Fatalf("delegate without on-behalf-of: %d %v", st, out)
	}
	h := hdr("tower", "k-t2")
	h[auth.HeaderOnBehalf] = "user:default/jfillman"
	st, out, body := postJSON(t, s, reissuePath(oldChain), h, "")
	if st != 201 {
		t.Fatalf("delegate: %d %s", st, body)
	}
	if run := store.objs[out["pipelineRun"].(string)]; param(run, "initiator") != "user:default/jfillman" {
		t.Fatalf("initiator: %v", param(run, "initiator"))
	}
	if !strings.Contains(logs.String(), `"caller":"tower","onBehalfOf":"user:default/jfillman"`) {
		t.Fatalf("audit names both: %s", logs)
	}
}

// An older admission policy for glidepath-api's runs does not know glidepath.io/reissue-of: the reissue goes ahead without
// the link (never refused for it) and says so.
func TestReissueWithoutTheAnnotationAllowed(t *testing.T) {
	s, store, _, _ := withReissue(t, reissuers, record(oldChain, prodLabels, closedRelease(nil)))
	store.refuseWhen = func(obj map[string]any) error {
		if _, ok := obj["metadata"].(map[string]any)["annotations"].(map[string]any)[pipelineruns.AnnotationReissueOf]; ok {
			return apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", errors.New("A run glidepath-api starts may carry only the annotations glidepath.io/origin-ref"))
		}
		return nil
	}
	st, out, body := postJSON(t, s, reissuePath(oldChain), hdr("rel", "k-old"), "")
	if st != 201 || out["linked"] != false {
		t.Fatalf("%d %s", st, body)
	}
	ann := store.objs[out["pipelineRun"].(string)]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if _, ok := ann[pipelineruns.AnnotationReissueOf]; ok || ann[pipelineruns.AnnotationOriginSha] != devSHA {
		t.Fatalf("the run is the same less the link: %v", ann)
	}
}

func TestListReissuable(t *testing.T) {
	// prod: the newest release's PR was closed unmerged -> listed, eligible. staging: newest release's PR is open -> not listed.
	stagingLabels := map[string]string{"hangar.io/env": "staging", "hangar.io/cluster": "hangar-prod"}
	s, _, gh, _ := withReissue(t, reissuers,
		record(oldChain, prodLabels, closedRelease(nil)),
		record("99999999-aaaa-4bbb-8ccc-dddddddddddd", stagingLabels, closedRelease(map[string]string{"env": "staging", "releaseId": "99999999-aaaa-4bbb-8ccc-dddddddddddd:hangar-prod/staging", "prUrl": "https://github.com/jfillman/gitops-shop/pull/12"})))
	gh.pulls["jfillman/gitops-shop#12"] = pull(12, "open", false, "release-staging-abc1234d")
	st, out, body := call(t, s, "/v1/apps/shop/releases/reissuable", hdr("rel", ""))
	list, _ := out["releases"].([]any)
	if st != 200 || len(list) != 1 {
		t.Fatalf("%d %s", st, body)
	}
	e := list[0].(map[string]any)
	if e["chainId"] != oldChain || e["eligible"] != true || e["env"] != "prod" || e["prUrl"] != oldPR {
		t.Fatalf("%v", e)
	}

	// With another open PR for the environment, the candidate is listed with the reason it is not eligible.
	gh.pulls["jfillman/gitops-shop#9"] = pull(9, "open", false, "release-prod-1234567a")
	_, out, body = call(t, s, "/v1/apps/shop/releases/reissuable", hdr("rel", ""))
	e = out["releases"].([]any)[0].(map[string]any)
	if e["eligible"] != false || !strings.Contains(fmt.Sprint(e["reason"]), "already open") {
		t.Fatalf("%s", body)
	}

	// A reader may list, a caller with neither verb may not.
	if st, _, _ := call(t, s, "/v1/apps/shop/releases/reissuable", hdr("reader", "")); st != 200 {
		t.Fatalf("reader: %d", st)
	}
	if st, _, _ := call(t, s, "/v1/apps/shop/releases/reissuable", hdr("op", "")); st != 403 {
		t.Fatalf("op: %d", st)
	}
}
