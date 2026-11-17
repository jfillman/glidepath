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
