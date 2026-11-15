// vap-compile compiles every CEL expression of every ValidatingAdmissionPolicy in the YAML it is given, with the Kubernetes API server's own
// compiler (k8s.io/apiserver's plugin/cel, the composited compiler the admission plugin uses, strict type checking, the NewExpressions environment
// the API validation uses when a policy is created). cel-python accepts what this refuses (it does not type-check map literals or reject an invalid
// string escape the way the API server does), so a policy that passes the behaviour tests can still leave a cluster unable to sync it.
//
//	helm template ... | go run . [-minor 36] [files...]      (stdin when there are no files)
//
// Exit 1 and one line per invalid expression, "<policy>: <where>: <error>", when any does not compile. Exit 0 with a count otherwise.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/util/version"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"
)

type expr struct {
	name string
	text string
	ret  []*cel.Type
}

func (e expr) GetExpression() string    { return e.text }
func (e expr) ReturnTypes() []*cel.Type { return e.ret }
func (e expr) GetName() string          { return e.name }

type policy struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ParamKind       *struct{} `json:"paramKind"`
		MatchConditions []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		} `json:"matchConditions"`
		Variables []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		} `json:"variables"`
		Validations []struct {
			Expression        string `json:"expression"`
			MessageExpression string `json:"messageExpression"`
		} `json:"validations"`
		AuditAnnotations []struct {
			Key             string `json:"key"`
			ValueExpression string `json:"valueExpression"`
		} `json:"auditAnnotations"`
	} `json:"spec"`
}

func main() {
	minor := flag.Int("minor", 36, "Kubernetes minor version whose CEL environment is used (the cluster's)")
	flag.Parse()
	var docs [][]byte
	if flag.NArg() == 0 {
		b, _ := io.ReadAll(bufio.NewReader(os.Stdin))
		docs = append(docs, b)
	}
	for _, f := range flag.Args() {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		docs = append(docs, b)
	}
	envSet := environment.MustBaseEnvSet(version.MajorMinor(1, uint(*minor)))
	bad, n := 0, 0
	for _, all := range docs {
		for _, p := range policies(all) {
			n++
			bad += check(envSet, p)
		}
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "no ValidatingAdmissionPolicy in the input: the check would be vacuous")
		os.Exit(2)
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "%d invalid expression(s) in %d policies\n", bad, n)
		os.Exit(1)
	}
	fmt.Printf("ok: %d ValidatingAdmissionPolicy compile under the API server's CEL environment (1.%d)\n", n, *minor)
}

// policies are the ValidatingAdmissionPolicy documents of a multi-document YAML.
func policies(all []byte) []policy {
	var out []policy
	for _, d := range bytes.Split(all, []byte("\n---")) {
		var p policy
		if err := yaml.Unmarshal(d, &p); err != nil || p.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// condense keeps every line of a compile error, and cuts a long echo of the source (a line behind "|") to 160 characters so a policy with a big
// expression does not print it whole under each error. The echo and its caret line stay: they are where the position is.
func condense(msg string) string {
	var keep []string
	for _, l := range strings.Split(msg, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "|") && len(t) > 160 {
			l = t[:160] + " ..."
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n    ")
}

func check(envSet *environment.EnvSet, p policy) int {
	bad := 0
	report := func(where string, err error) {
		bad++
		msg := condense(err.Error())
		fmt.Printf("%s: %s: %s\n", p.Metadata.Name, where, msg)
	}
	compiler, err := plugincel.NewCompositedCompiler(envSet)
	if err != nil {
		report("compiler", err)
		return bad
	}
	opts := plugincel.OptionalVariableDeclarations{HasParams: p.Spec.ParamKind != nil, HasAuthorizer: true}
	// matchConditions run without `variables`, in their own environment; variables, validations and the rest compose
	for i, m := range p.Spec.MatchConditions {
		r := compiler.CompileCELExpression(expr{name: m.Name, text: m.Expression, ret: []*cel.Type{cel.BoolType}}, plugincel.OptionalVariableDeclarations{HasParams: opts.HasParams, HasAuthorizer: true}, environment.NewExpressions)
		if r.Error != nil {
			report(fmt.Sprintf("matchConditions[%d] (%s)", i, m.Name), r.Error)
		}
	}
	for i, v := range p.Spec.Variables {
		r := compiler.CompileAndStoreVariable(expr{name: v.Name, text: v.Expression, ret: []*cel.Type{cel.AnyType}}, opts, environment.NewExpressions)
		if r.Error != nil {
			report(fmt.Sprintf("variables[%d] (%s)", i, v.Name), r.Error)
		}
	}
	for i, v := range p.Spec.Validations {
		r := compiler.CompileCELExpression(expr{text: v.Expression, ret: []*cel.Type{cel.BoolType}}, opts, environment.NewExpressions)
		if r.Error != nil {
			report(fmt.Sprintf("validations[%d].expression", i), r.Error)
		}
		if v.MessageExpression != "" {
			r := compiler.CompileCELExpression(expr{text: v.MessageExpression, ret: []*cel.Type{cel.StringType}}, opts, environment.NewExpressions)
			if r.Error != nil {
				report(fmt.Sprintf("validations[%d].messageExpression", i), r.Error)
			}
		}
	}
	for i, a := range p.Spec.AuditAnnotations {
		r := compiler.CompileCELExpression(expr{text: a.ValueExpression, ret: []*cel.Type{cel.StringType, cel.NullType}}, opts, environment.NewExpressions)
		if r.Error != nil {
			report(fmt.Sprintf("auditAnnotations[%d] (%s)", i, a.Key), r.Error)
		}
	}
	return bad
}
