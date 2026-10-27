package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "jfillman-gitops-hangar-prod-abc1234/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "jfillman-gitops-hangar-prod-abc1234/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestPolicyDocumentsFromTarball(t *testing.T) {
	tb := tarball(t, map[string]string{
		"30-policy/breakglass/policies.yaml": `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: no-exec}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: no-exec}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: not-a-policy}
`,
		"30-policy/kyverno-policies/app.yaml":    "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: kyverno-policies}\n",
		"30-policy/kyverno-policies/vpol.yml":    "apiVersion: policies.kyverno.io/v1\nkind: ValidatingPolicy\nmetadata: {name: a}\n---\napiVersion: policies.kyverno.io/v1\nkind: ImageValidatingPolicy\nmetadata: {name: image}\n",
		"30-policy/kyverno-policies/legacy.yaml": "apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata: {name: legacy}\n",
		"30-policy/kyverno-policies/broken.yaml": "apiVersion: policies.kyverno.io/v1\nkind: ValidatingPolicy\nmetadata: [unclosed\n",
		"30-policy/README.md":                    "kind: ValidatingPolicy\n",
		"cluster.yaml":                           "generated:\n  password: s3cret\n",
		"00-bootstrap/x/vap.yaml":                "apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingAdmissionPolicy\nmetadata: {name: outside-the-path}\n",
		"30-policy-other/vpol.yaml":              "apiVersion: policies.kyverno.io/v1\nkind: ValidatingPolicy\nmetadata: {name: prefix-only}\n",
	})
	docs, files, err := policyDocumentsFromTarball(tb, "30-policy")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range docs {
		got[d["kind"].(string)+"/"+d["metadata"].(map[string]interface{})["name"].(string)] = true
	}
	want := []string{"ValidatingAdmissionPolicy/no-exec", "ValidatingAdmissionPolicyBinding/no-exec", "ValidatingPolicy/a", "ClusterPolicy/legacy"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %s (got %v)", w, got)
		}
	}
	if len(docs) != len(want) {
		t.Errorf("got %d documents %v, want exactly %v", len(docs), got, want)
	}
	if files != 3 {
		t.Errorf("files = %d, want 3 (policies.yaml, vpol.yml, legacy.yaml; app.yaml has no policy, broken.yaml does not parse)", files)
	}
}

func TestResolvePolicySource(t *testing.T) {
	s := &policySources{
		policySource: policySource{RepoURL: "https://github.com/{owner}/gitops-{cluster}.git", Path: "30-policy", Revision: "main"},
		Clusters:     map[string]policySource{"edge": {RepoURL: "https://github.com/acme/platform-edge", Path: "../policies/"}},
	}
	src, err := s.resolve("hangar-prod", "jfillman")
	if err != nil || src.RepoURL != "https://github.com/jfillman/gitops-hangar-prod.git" || src.Path != "30-policy" || src.Revision != "main" {
		t.Errorf("default: %+v %v", src, err)
	}
	src, err = s.resolve("edge", "jfillman")
	if err != nil || src.RepoURL != "https://github.com/acme/platform-edge" || src.Path != "policies" || src.Revision != "main" {
		t.Errorf("override (path cleaned, cannot climb out): %+v %v", src, err)
	}
	bad := &policySources{policySource: policySource{RepoURL: "file:///etc/{cluster}"}}
	if _, err := bad.resolve("x", "y"); err == nil {
		t.Error("a non-GitHub source must be refused")
	}
	for _, c := range []string{"hangar-prod", "a"} {
		if !clusterNameRE.MatchString(c) {
			t.Errorf("%s should be a valid cluster name", c)
		}
	}
	for _, c := range []string{"../x", "Hangar", "a/b", ""} {
		if clusterNameRE.MatchString(c) {
			t.Errorf("%q should be refused as a cluster name", c)
		}
	}
}
