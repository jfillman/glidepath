// Checks the config-only build exemption that deliver-onboarding-files writes into every push flow's
// on-cel-expression, with cel-go (the CEL library Pipelines-as-Code evaluates it with), against push payloads shaped
// like GitHub's. The expression is read from the template itself, so this tests what ships.
//
// Rules it holds (owner's decisions): a push touching only cicd.yaml, glidepath/ or .tekton/ builds nothing - in
// particular a cicd.yaml change never starts a build, directly or through the resync PR it causes - except the merge
// of an app's first onboarding PR, which starts its first build.
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

func main() {
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
	if failed > 0 {
		os.Exit(1)
	}
}
