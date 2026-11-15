#!/usr/bin/env python3
"""Tests for the cicd-run-shape and cicd-run-source-isolation admission policies.

Renders the chart, then evaluates the policies' own CEL (matchConditions, variables, validations) over fixtures with a
real CEL implementation, so a typo or a has()-less field access is caught here, not by a refused production request.
It proves: every .tekton template the platform delivers is admitted; each way a pull request could reach beyond the
catalog (inline spec, another ServiceAccount, a podTemplate volume, a rogue per-task override, a build run without the
source-isolation entries) is refused; a request not made by Pipelines-as-code, or outside a -cicd namespace, is not
this policy's concern; and no expression raises on a sparse object (an error under failurePolicy Fail refuses the
request).

Run:  python3 charts/glidepath-control-plane/tests/cicd_run_admission_test.py     (needs helm, pyyaml, cel-python)
"""
import copy
import glob
import os
import subprocess
import unittest

import celpy
import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CHART = os.path.join(ROOT, "charts", "glidepath-control-plane")
VALUES = os.path.join(CHART, "tests", "values.example.yaml")
PAC = "system:serviceaccount:tekton-pipelines:pipelines-as-code-controller"
NS = "app-sample-cicd"
SOURCE_TASKS = ["build-source", "unit-test", "build-python-wheel-artifact", "build-go-binary-artifact"]
ENV = celpy.Environment()


def render(*extra):
    r = subprocess.run(["helm", "template", "cp", CHART, "-f", VALUES, *extra], capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    return [d for d in yaml.safe_load_all(r.stdout) if d]


def policies(*extra):
    docs = render(*extra)
    pols = {d["metadata"]["name"]: d["spec"] for d in docs if d["kind"] == "ValidatingAdmissionPolicy"}
    binds = {d["metadata"]["name"]: d["spec"] for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding"}
    return pols, binds


def cel(expr, act):
    return ENV.program(ENV.compile(" ".join(expr.split())), functions=None).evaluate(act)


def evaluate(policy, obj, user=PAC, namespace=NS):
    """-> None when the policy does not apply, else the list of failing validation messages."""
    request = {"userInfo": {"username": user}, "namespace": namespace, "operation": "CREATE"}
    act = {"object": celpy.json_to_cel(obj), "request": celpy.json_to_cel(request), "variables": celpy.json_to_cel({})}
    for mc in policy["matchConditions"]:
        if not cel(mc["expression"], act):
            return None
    variables = {}
    for v in policy.get("variables", []):
        variables[v["name"]] = cel(v["expression"], act)
        act["variables"] = _wrap(variables)
    failed = []
    for val in policy["validations"]:
        if not cel(val["expression"], act):
            failed.append(val["message"])
    return failed


def _wrap(variables):
    # a CEL map of already-evaluated CEL values
    return celpy.celtypes.MapType({celpy.celtypes.StringType(k): v for k, v in variables.items()})


def run(**spec):
    base = {
        "apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
        "metadata": {"generateName": "x-"},
        "spec": {
            "pipelineRef": {"resolver": "cluster", "params": [
                {"name": "kind", "value": "pipeline"}, {"name": "name", "value": "build"},
                {"name": "namespace", "value": "platform-catalog"}]},
            "params": [{"name": "git-url", "value": "https://github.com/o/sample"}],
            "taskRunTemplate": {"serviceAccountName": "pipeline-runner"},
            "workspaces": [{"name": "source", "volumeClaimTemplate": {"spec": {}}}],
            "taskRunSpecs": [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS],
        },
    }
    base["spec"].update(spec)
    for k in [k for k, v in base["spec"].items() if v is None]:
        del base["spec"][k]
    return base


class CicdRunAdmission(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.pols, cls.binds = policies()
        cls.shape = cls.pols["cicd-run-shape"]
        cls.iso = cls.pols["cicd-run-source-isolation"]

    def refused(self, obj, policy=None, msg=None):
        out = evaluate(policy or self.shape, obj)
        self.assertTrue(out, "expected a refusal, got %r" % (out,))
        if msg:
            self.assertTrue(any(msg in m for m in out), out)

    def test_bindings_deny_and_are_failclosed_with_guards(self):
        self.assertEqual(self.binds["cicd-run-shape"]["validationActions"], ["Deny"])
        self.assertEqual(self.binds["cicd-run-source-isolation"]["validationActions"], ["Deny"])
        for p in self.pols.values():
            self.assertEqual(p["failurePolicy"], "Fail")
            self.assertTrue(p["matchConditions"], "a policy with no matchConditions applies to every PipelineRun")

    def test_isolation_actions_are_a_value(self):
        _, binds = policies("--set", "cicdRunAdmission.sourceIsolationActions={Warn,Audit}")
        self.assertEqual(binds["cicd-run-source-isolation"]["validationActions"], ["Warn", "Audit"])
        self.assertEqual(binds["cicd-run-shape"]["validationActions"], ["Deny"])

    def test_off_renders_nothing(self):
        pols, binds = policies("--set", "cicdRunAdmission.enabled=false")
        self.assertFalse([n for n in list(pols) + list(binds) if n.startswith("cicd-run-")])

    def test_generated_build_run_is_admitted(self):
        self.assertEqual(evaluate(self.shape, run()), [])
        self.assertEqual(evaluate(self.iso, run()), [])

    def test_every_delivered_template_is_admitted(self):
        seen = 0
        for path in sorted(glob.glob(os.path.join(ROOT, "charts", "glidepath-app", "files", "onboarding-templates", "*", "*.yaml"))):
            with open(path) as f:
                docs = list(yaml.safe_load_all(f))
            for d in docs:
                if not d or d.get("kind") != "PipelineRun":
                    continue
                seen += 1
                self.assertEqual(evaluate(self.shape, d), [], path)
        self.assertGreater(seen, 15, "the glob found too few templates: the comparison would be vacuous")

    def test_inline_pipeline_spec_refused(self):
        self.refused(run(pipelineSpec={"tasks": [{"name": "x", "taskSpec": {"steps": []}}]}), msg="inline pipelineSpec")

    def test_missing_pipeline_ref_refused(self):
        self.refused(run(pipelineRef=None, pipelineSpec={"tasks": []}))
        self.refused(run(pipelineRef=None))

    def test_pipeline_ref_other_than_catalog_refused(self):
        for bad in (
            {"resolver": "git", "params": [{"name": "url", "value": "https://evil"}]},
            {"resolver": "cluster", "params": [{"name": "kind", "value": "pipeline"}, {"name": "name", "value": "x"},
                                               {"name": "namespace", "value": NS}]},
            {"resolver": "cluster", "params": [{"name": "kind", "value": "task"}, {"name": "name", "value": "x"},
                                               {"name": "namespace", "value": "platform-catalog"}]},
            {"name": "build"},
        ):
            self.refused(run(pipelineRef=bad), msg="pipelineRef")

    def test_other_service_accounts_refused(self):
        for sa in ("default", "glidepath-cloud-deployer", "pipeline-trigger", "source-runner"):
            self.refused(run(taskRunTemplate={"serviceAccountName": sa}), msg="pipeline-runner")
        self.refused(run(taskRunTemplate={}), msg="pipeline-runner")
        self.refused(run(taskRunTemplate=None), msg="pipeline-runner")

    def test_task_overrides_limited(self):
        ok = [{"pipelineTaskName": "deploy-aws-lambda", "serviceAccountName": "glidepath-cloud-deployer"}]
        self.assertEqual(evaluate(self.shape, run(taskRunSpecs=ok)), [])
        for bad in (
            [{"pipelineTaskName": "x", "serviceAccountName": "pipeline-trigger"}],
            [{"pipelineTaskName": "x", "serviceAccountName": "default"}],
            [{"pipelineTaskName": "x"}],
            [{"pipelineTaskName": "build-source", "serviceAccountName": "source-runner", "podTemplate": {"volumes": []}}],
            [{"pipelineTaskName": "build-source", "serviceAccountName": "source-runner", "stepSpecs": []}],
        ):
            self.refused(run(taskRunSpecs=bad), msg="taskRunSpecs")

    def test_pod_template_only_host_aliases(self):
        ok = {"serviceAccountName": "pipeline-runner", "podTemplate": {"hostAliases": [{"ip": "10.0.0.1", "hostnames": ["a"]}]}}
        self.assertEqual(evaluate(self.shape, run(taskRunTemplate=ok)), [])
        bad = {"serviceAccountName": "pipeline-runner", "podTemplate": {"volumes": [{"name": "s", "secret": {"secretName": "x"}}]}}
        self.refused(run(taskRunTemplate=bad), msg="podTemplate")

    def test_workspaces_limited(self):
        self.refused(run(workspaces=[{"name": "source", "secret": {"secretName": "app-secrets"}}]), msg="Workspaces")
        self.refused(run(workspaces=[{"name": "source", "persistentVolumeClaim": {"claimName": "x"}}]), msg="Workspaces")

    def test_source_isolation_required_for_the_build_pipeline_only(self):
        for specs in (None, [], [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS[:3]],
                      [{"pipelineTaskName": t, "serviceAccountName": "pipeline-runner"} for t in SOURCE_TASKS]):
            self.refused(run(taskRunSpecs=specs), self.iso, "source-runner")
        other = run(taskRunSpecs=None)
        other["spec"]["pipelineRef"]["params"][1]["value"] = "cicd-validate-check"
        self.assertIsNone(evaluate(self.iso, other))

    def test_not_this_policys_concern(self):
        hostile = run(pipelineSpec={"tasks": []}, taskRunTemplate={"serviceAccountName": "default"})
        for user in ("system:serviceaccount:platform-system:glidepath-api", "system:serviceaccount:app-sample-cicd:pipeline-trigger",
                     "system:serviceaccount:tekton-pipelines:pipelines-as-code-watcher", "kubernetes-admin"):
            self.assertIsNone(evaluate(self.shape, hostile, user=user), user)
            self.assertIsNone(evaluate(self.iso, hostile, user=user), user)
        for ns in ("platform-catalog", "app-sample-prod", "app-sample-cicd-extra", "kube-system", ""):
            self.assertIsNone(evaluate(self.shape, hostile, namespace=ns), ns)

    def test_sparse_objects_do_not_raise(self):
        # An error under failurePolicy Fail refuses the request cluster-wide: every shape must evaluate, not raise.
        for spec in ({}, {"pipelineRef": {}}, {"pipelineRef": {"params": []}}, {"taskRunSpecs": [{}]},
                     {"taskRunTemplate": {"podTemplate": {}}}, {"workspaces": [{}]}, {"pipelineRef": {"params": [{}]}}):
            obj = {"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "metadata": {}, "spec": spec}
            self.assertIsInstance(evaluate(self.shape, obj), list)
            evaluate(self.iso, obj)
        for obj in ({"spec": {}}, {}):
            for pol in (self.shape, self.iso):
                evaluate(pol, copy.deepcopy(obj))


if __name__ == "__main__":
    unittest.main()
