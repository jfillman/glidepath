// Checks the config-only build exemption that deliver-onboarding-files writes into every push flow's
// on-cel-expression, with cel-go (the CEL library Pipelines-as-Code evaluates it with), against push payloads shaped
// like GitHub's. The expression is read from the template itself, so this tests what ships.
//
// Rules it holds (owner's decisions): a push touching only cicd.yaml, glidepath/ or .tekton/ builds nothing - in
// particular a cicd.yaml change never starts a build, directly or through the resync PR it causes - except the merge
// of an app's first onboarding PR, which starts its first build.
//
// It also checks the propose-merge audit trigger (push-propose-merge-check.yaml): it must NOT start for a push whose every
// changed file is a machine-written release file (glidepath/envs/<env>.release.yaml), and must start for every other push,
// in particular for glidepath/envs/<env>.yaml, cicd.yaml and .tekton/ changes, a Hangar-Merge-marked commit and a PR merge.
//
// And the gitops repo's clearance-gate trigger (pull-request-clearance-gate.yaml, owner decision I-84 option (a)): it must
// start for every PR to main that changes deployable content, whatever its branch, and for every release-* PR; never for a
// release-record-* PR, a PR to another branch, or a PR that changes only .tekton/, releases/ or docs.
//
// Usage: (cd hack/ci/build-exemption-cel && go run .)   - its own module, so cel-go stays out of the repo root.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/google/cel-go/cel"
)

const template = "../../../charts/glidepath-catalog/templates/tasks/deliver-onboarding-files.yaml"
const proposeTemplate = "../../../charts/glidepath-app/files/onboarding-templates/app-repo/push-propose-merge-check.yaml"

type payloadCase struct {
	name  string
	build bool
	body  string
}

var cases = []payloadCase{
	{"first onboarding PR merged (merge commit + PR commit)", true,
		`{"commits":[{"added":[".tekton/flow-ci.yaml",".tekton/onboarding-resync.yaml"]},{"modified":[]}]}`},
	{"first onboarding PR squash-merged", true, `{"commits":[{"added":[".tekton/flow-ci.yaml",".tekton/onboarding-resync.yaml"]}]}`},
	{"first onboarding PR of a Kubernetes app (with env files)", true,
		`{"commits":[{"added":[".tekton/flow-ci.yaml",".tekton/onboarding-resync.yaml","glidepath/envs/dev.yaml","glidepath/pr-env.yaml"]}]}`},
	{"resync PR merged", false, `{"commits":[{"modified":[".tekton/flow-ci.yaml",".tekton/onboarding-resync.yaml"]}]}`},
	{"resync adds a promote flow for a new Flight env", false,
		`{"commits":[{"added":[".tekton/flow-promote-prod.yaml"],"modified":[".tekton/flow-ci.yaml"]}]}`},
	{"cicd.yaml change adds a push flow; its resync merged", false,
		`{"commits":[{"added":[".tekton/flow-nightly.yaml"],"modified":[".tekton/flow-ci.yaml"]}]}`},
	{"cicd.yaml edited with the resync it caused, one push", false,
		`{"commits":[{"modified":["cicd.yaml"]},{"added":[".tekton/flow-nightly.yaml"]}]}`},
	{"cicd.yaml only", false, `{"commits":[{"modified":["cicd.yaml"]}]}`},
	{"glidepath env values only", false, `{"commits":[{"modified":["glidepath/envs/dev.yaml"]}]}`},
	{"a flow file re-added by a resync", false, `{"commits":[{"added":[".tekton/flow-ci.yaml"]}]}`},
	{"a flow file removed", false, `{"commits":[{"removed":[".tekton/flow-ci.yaml"]}]}`},
	{"developer code change", true, `{"commits":[{"modified":["main.go"]}]}`},
	{"developer change alongside a resync commit", true, `{"commits":[{"modified":[".tekton/flow-ci.yaml"]},{"modified":["main.go"]}]}`},
	{"empty commit", true, `{"commits":[{}]}`},
	{"no commits key", true, `{}`},
}

// Push payloads for the propose-merge audit: build here means "the audit starts".
var proposeCases = []payloadCase{
	{"deploy write-back: only dev.release.yaml", false,
		`{"ref":"refs/heads/main","head_commit":{"message":"Deploy parachute to dev: ghcr.io/x/parachute:1","author":{"email":"deploy@glidepath.invalid"}},"commits":[{"message":"Deploy parachute to dev: ghcr.io/x/parachute:1","modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"two release files in one push", false,
		`{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/qa.release.yaml"]}]}`},
	{"human env file glidepath/envs/dev.yaml", true, `{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.yaml"]}]}`},
	{"release file plus the env file it migrated", true,
		`{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.release.yaml","glidepath/envs/dev.yaml"]}]}`},
	{"cicd.yaml", true, `{"ref":"refs/heads/main","commits":[{"modified":["cicd.yaml"]}]}`},
	{".tekton file", true, `{"ref":"refs/heads/main","commits":[{"modified":[".tekton/push-propose-merge-check.yaml"]}]}`},
	{"glidepath/base.yaml", true, `{"ref":"refs/heads/main","commits":[{"modified":["glidepath/base.yaml"]}]}`},
	{"release file in a nested folder is not the pipeline's", true, `{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/x/dev.release.yaml"]}]}`},
	{"a release.yaml outside glidepath/envs", true, `{"ref":"refs/heads/main","commits":[{"modified":["prod/release.yaml"]}]}`},
	{"release file plus a source file, one commit", true,
		`{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.release.yaml","main.go"]}]}`},
	{"release file commit then a source commit", true,
		`{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["main.go"]}]}`},
	{"PR merge commit touching a release file only", true,
		`{"ref":"refs/heads/main","head_commit":{"message":"Merge pull request #7 from o/branch\n\nbump"},"commits":[{"message":"bump","modified":["glidepath/envs/dev.release.yaml"]},{"message":"Merge pull request #7 from o/branch","modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"squash merge touching a release file only", true,
		`{"ref":"refs/heads/main","head_commit":{"message":"Pin image (#12)"},"commits":[{"message":"Pin image (#12)","modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"release-only commit marked Hangar-Merge: person", true,
		`{"ref":"refs/heads/main","head_commit":{"message":"x"},"commits":[{"message":"x\n\nHangar-Merge: person","modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"empty commit", true, `{"ref":"refs/heads/main","commits":[{}]}`},
	{"no commits key", true, `{"ref":"refs/heads/main"}`},
	{"20 release-only commits (GitHub's cap) is never skipped", true, `{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"19 release-only commits is skipped", false, `{"ref":"refs/heads/main","commits":[{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"head_commit carries the trailer, visible commits release-only", true,
		`{"ref":"refs/heads/main","head_commit":{"message":"x\\n\\nHangar-Merge: person"},"commits":[{"message":"x","modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"rebase-style push: release-only visible commits, count at the cap", true,
		`{"ref":"refs/heads/main","head_commit":{"message":"Deploy a to dev: i"},"commits":[{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]},{"modified":["glidepath/envs/dev.release.yaml"]}]}`},
	{"push to another branch", false, `{"ref":"refs/heads/feature","commits":[{"modified":["cicd.yaml"]}]}`},
}

const clearanceGateTemplate = "../../../charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-clearance-gate.yaml"

// A pull_request event as Pipelines-as-Code exposes it to on-cel-expression: event, target_branch, source_branch, files.all.
type prCase struct {
	name   string
	start  bool
	target string
	source string
	files  []string
}

var clearanceGateCases = []prCase{
	{"hand PR: staging replicas from a patch branch", true, "main", "someone-patch-1", []string{"prod-cluster/staging/values.yaml"}},
	{"hand PR: release.yaml", true, "main", "fix-image", []string{"prod-cluster/prod/release.yaml"}},
	{"hand PR: a cluster's base.yaml", true, "main", "tune", []string{"prod-cluster/base.yaml"}},
	{"hand PR: values-<env>.yaml", true, "main", "tune", []string{"values-prod.yaml"}},
	{"hand PR: a manifest under an env dir", true, "main", "netpol", []string{"prod-cluster/prod/networkpolicy.yaml"}},
	{"hand PR: a JSON manifest under an env dir", true, "main", "x", []string{"prod-cluster/prod/extra/cm.json"}},
	{"hand PR: kustomization.yaml", true, "main", "x", []string{"overlays/kustomization.yaml"}},
	{"hand PR: kustomization.yml at the root", true, "main", "x", []string{"kustomization.yml"}},
	{"hand PR: Kustomization", true, "main", "x", []string{"overlays/prod/Kustomization"}},
	{"hand PR: README and values", true, "main", "x", []string{"README.md", "prod-cluster/staging/values.yaml"}},
	{"agent PR branch: values", true, "main", "autopilot/run-12", []string{"prod-cluster/staging/values.yaml"}},
	{"release PR (unchanged behaviour)", true, "main", "release-gate-api-prod-1", []string{"prod-cluster/prod/release.yaml"}},
	{"release PR with no deployable file still runs", true, "main", "release-gate-api-prod-1", []string{"README.md"}},
	{"release-record PR never", false, "main", "release-record-gate-api-1", []string{"releases/gate-api@1.0.0-abc.yaml", "prod-cluster/prod/release.yaml"}},
	{"PR to another branch", false, "staging", "x", []string{"prod-cluster/staging/values.yaml"}},
	{"onboarding resync (.tekton only)", false, "main", "onboarding-resync", []string{".tekton/pull-request-values.yaml", ".tekton/pull-request-clearance-gate.yaml"}},
	{"release ledger only", false, "main", "x", []string{"releases/gate-api@1.0.0-abc.yaml"}},
	{"README only", false, "main", "x", []string{"README.md"}},
	{"clearance.yaml only (registers no environment)", false, "main", "x", []string{"clearance.yaml"}},
	{"no files", false, "main", "x", []string{}},
}

func runPR(name string, expr string, cs []prCase) int {
	env, err := cel.NewEnv(cel.Variable("event", cel.StringType), cel.Variable("target_branch", cel.StringType),
		cel.Variable("source_branch", cel.StringType), cel.Variable("files", cel.DynType))
	if err != nil {
		panic(err)
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		fmt.Fprintln(os.Stderr, name, "does not compile:", iss.Err())
		os.Exit(1)
	}
	prg, err := env.Program(ast)
	if err != nil {
		panic(err)
	}
	failed := 0
	for _, c := range cs {
		all := make([]any, len(c.files))
		for i, f := range c.files {
			all[i] = f
		}
		out, _, err := prg.Eval(map[string]any{"event": "pull_request", "target_branch": c.target, "source_branch": c.source,
			"files": map[string]any{"all": all}})
		got := err == nil && out.Value() == true
		if err != nil || got != c.start {
			failed++
			fmt.Printf("FAIL %-55s start=%v want=%v err=%v\n", c.name, got, c.start, err)
		} else {
			fmt.Printf("ok   %-55s start=%v\n", c.name, got)
		}
	}
	fmt.Printf("%s: %d events, %d failing\n", name, len(cs), failed)
	return failed
}

func run(name string, expr string, cs []payloadCase) int {
	env, err := cel.NewEnv(cel.Variable("body", cel.DynType))
	if err != nil {
		panic(err)
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		fmt.Fprintln(os.Stderr, name, "does not compile:", iss.Err())
		os.Exit(1)
	}
	prg, err := env.Program(ast)
	if err != nil {
		panic(err)
	}
	failed := 0
	for _, c := range cs {
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			panic(err)
		}
		out, _, err := prg.Eval(map[string]any{"body": body})
		got := err == nil && out.Value() == true
		if err != nil || got != c.build {
			failed++
			fmt.Printf("FAIL %-55s start=%v want=%v err=%v\n", c.name, got, c.build, err)
		} else {
			fmt.Printf("ok   %-55s start=%v\n", c.name, got)
		}
	}
	fmt.Printf("%s: %d payloads, %d failing\n", name, len(cs), failed)
	return failed
}

func main() {
	psrc, err := os.ReadFile(proposeTemplate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run from hack/ci/build-exemption-cel:", err)
		os.Exit(2)
	}
	pm := regexp.MustCompile(`(?m)^\s*pipelinesascode\.tekton\.dev/on-cel-expression: '(.*)'\s*$`).FindSubmatch(psrc)
	if pm == nil {
		fmt.Fprintln(os.Stderr, "no on-cel-expression line in", proposeTemplate)
		os.Exit(2)
	}
	proposeFailed := run("propose-merge-check trigger", string(pm[1]), proposeCases)

	csrc, err := os.ReadFile(clearanceGateTemplate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run from hack/ci/build-exemption-cel:", err)
		os.Exit(2)
	}
	cm := regexp.MustCompile(`(?m)^\s*pipelinesascode\.tekton\.dev/on-cel-expression: '(.*)'\s*$`).FindSubmatch(csrc)
	if cm == nil {
		fmt.Fprintln(os.Stderr, "no on-cel-expression line in", clearanceGateTemplate)
		os.Exit(2)
	}
	proposeFailed += runPR("clearance-gate trigger", string(cm[1]), clearanceGateCases)

	src, err := os.ReadFile(template)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run from hack/ci/build-exemption-cel:", err)
		os.Exit(2)
	}
	m := regexp.MustCompile(`(?m)^\s*local exempt_guard='(.*)'\s*$`).FindSubmatch(src)
	if m == nil {
		fmt.Fprintln(os.Stderr, "no `local exempt_guard='...'` line in", template)
		os.Exit(2)
	}
	env, err := cel.NewEnv(cel.Variable("body", cel.DynType))
	if err != nil {
		panic(err)
	}
	ast, iss := env.Compile(string(m[1]))
	if iss.Err() != nil {
		fmt.Fprintln(os.Stderr, "the exemption does not compile:", iss.Err())
		os.Exit(1)
	}
	prg, err := env.Program(ast)
	if err != nil {
		panic(err)
	}
	failed := 0
	for _, c := range cases {
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			panic(err)
		}
		out, _, err := prg.Eval(map[string]any{"body": body})
		got := err == nil && out.Value() == true
		if err != nil || got != c.build {
			failed++
			fmt.Printf("FAIL %-55s build=%v want=%v err=%v\n", c.name, got, c.build, err)
		} else {
			fmt.Printf("ok   %-55s build=%v\n", c.name, got)
		}
	}
	fmt.Printf("%d payloads, %d failing\n", len(cases), failed)
	if failed > 0 || proposeFailed > 0 {
		os.Exit(1)
	}
}
