#!/usr/bin/env python3
"""A repository's glidepath/<name>.yaml is applied as a TestWorkflow into the shared testkube namespace, where it runs as Testkube's own
executor with this app's Secret copy materialised. run-testworkflow checks it first (testworkflow_violations): one TestWorkflow, in the
testkube namespace, no service account selection, no host or secret/projected/PVC/CSI volume, no envFrom, secrets referenced only through
the __APP_SECRETS_NAME__ placeholder, image pull secrets only registry-credentials. What the allowlist cannot express is in
docs/admin/security.md. Runs the rendered helper with jq. Needs helm, pyyaml, jq, bash.
Run:  python3 charts/glidepath-catalog/tests/testworkflow_policy_test.py
"""
import json
import os
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")], capture_output=True, text=True)
assert r.returncode == 0, r.stderr
TASK = [d for d in yaml.safe_load_all(r.stdout) if d and d["kind"] == "Task" and d["metadata"]["name"] == "run-testworkflow"][0]
SCRIPT = "\n".join(s.get("script", "") for s in TASK["spec"]["steps"])

GOOD = {
    "apiVersion": "testworkflows.testkube.io/v1", "kind": "TestWorkflow", "metadata": {"name": "x"},
    "spec": {"config": {"url": {"type": "string"}},
             "container": {"image": "alpine", "env": [{"name": "K", "valueFrom": {"secretKeyRef": {"name": "__APP_SECRETS_NAME__", "key": "k"}}}]},
             "steps": [{"name": "run", "shell": "echo ok", "container": {"env": [{"name": "A", "value": "b"}]}}],
             "pod": {"imagePullSecrets": [{"name": "registry-credentials"}]}},
}


def violations(*docs):
    a = SCRIPT.index("# BEGIN testworkflow-policy")
    b = SCRIPT.index("# END testworkflow-policy")
    with tempfile.TemporaryDirectory() as d:
        h = os.path.join(d, "h.sh")
        open(h, "w").write(SCRIPT[a:b])
        p = subprocess.run(["bash", "-c", 'source "%s"; testworkflow_violations' % h], input="\n".join(json.dumps(x) for x in docs),
                           capture_output=True, text=True)
    return p.returncode, [l for l in p.stdout.split("\n") if l]


def mutated(fn):
    import copy
    d = copy.deepcopy(GOOD)
    fn(d)
    return d


class TestWorkflowPolicy(unittest.TestCase):
    def test_a_normal_workflow_passes(self):
        code, v = violations(GOOD)
        self.assertEqual((code, v), (0, []))

    def test_the_check_runs_before_apply(self):
        self.assertLess(SCRIPT.index("| testworkflow_violations)"), SCRIPT.index("kubectl -n testkube apply"))

    def test_refusals(self):
        cases = {
            "service account": lambda d: d["spec"]["pod"].update(serviceAccountName="testkube-admin"),
            "step service account": lambda d: d["spec"]["steps"][0].update(pod={"serviceAccountName": "x"}),
            "hostPath": lambda d: d["spec"]["pod"].update(volumes=[{"name": "h", "hostPath": {"path": "/"}}]),
            "secret volume": lambda d: d["spec"]["pod"].update(volumes=[{"name": "s", "secret": {"secretName": "other-app-app-secrets"}}]),
            "projected volume": lambda d: d["spec"]["pod"].update(volumes=[{"name": "s", "projected": {"sources": []}}]),
            "pvc": lambda d: d["spec"]["pod"].update(volumes=[{"name": "s", "persistentVolumeClaim": {"claimName": "x"}}]),
            "envFrom": lambda d: d["spec"]["container"].update(envFrom=[{"secretRef": {"name": "other-app-app-secrets"}}]),
            "other secret": lambda d: d["spec"]["container"]["env"].append({"name": "Z", "valueFrom": {"secretKeyRef": {"name": "victim-app-secrets", "key": "k"}}}),
            "step other secret": lambda d: d["spec"]["steps"][0]["container"]["env"].append({"name": "Z", "valueFrom": {"secretKeyRef": {"name": "victim", "key": "k"}}}),
            "host network": lambda d: d["spec"]["pod"].update(hostNetwork=True),
            "privileged": lambda d: d["spec"]["container"].update(securityContext={"privileged": True}),
            "pull secret": lambda d: d["spec"]["pod"].update(imagePullSecrets=[{"name": "other-app-registry"}]),
            "kind": lambda d: d.update(kind="TestWorkflowTemplate"),
            "namespace": lambda d: d["metadata"].update(namespace="kube-system"),
            "api group": lambda d: d.update(apiVersion="v1"),
        }
        for name, fn in cases.items():
            code, v = violations(mutated(fn))
            self.assertTrue(v, "expected a refusal: " + name)

    def test_more_than_one_document_is_refused(self):
        code, v = violations(GOOD, GOOD)
        self.assertTrue(v)

    def test_the_workflow_file_may_not_be_a_symlink(self):
        self.assertIn('-L "${WORKFLOW_FILE}"', SCRIPT)


if __name__ == "__main__":
    unittest.main()
