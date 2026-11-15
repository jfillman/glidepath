#!/usr/bin/env python3
"""Tests for the cicd-run-shape and cicd-run-source-isolation admission policies.

Renders the chart, then evaluates the policies' own CEL (matchConditions, variables, validations) over fixtures with a
real CEL implementation, so a typo or a has()-less field access is caught here, not by a refused production request.
It proves: every .tekton template the platform delivers (and every generated flow file shape) is admitted, and the list of
Pipelines a .tekton file may start is exactly what the templates and the flow generator reference; each way a pull request could reach beyond the
catalog (inline spec, another ServiceAccount, a podTemplate volume, a rogue per-task override, a build run without the
source-isolation entries) is refused; a request not made by Pipelines-as-code, or outside a -cicd namespace, is not
this policy's concern; and no expression raises on a sparse object (an error under failurePolicy Fail refuses the
request).

Run:  python3 charts/glidepath-control-plane/tests/cicd_run_admission_test.py     (needs helm, pyyaml, cel-python)
"""
import copy
import glob
import json
import os
import re
import subprocess
import unittest

import celpy
import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CHART = os.path.join(ROOT, "charts", "glidepath-control-plane")
VALUES = os.path.join(CHART, "tests", "values.example.yaml")
PAC = "system:serviceaccount:tekton-pipelines:pipelines-as-code-controller"
NS = "app-sample-cicd"
SOURCE_TASKS = ["build-source", "unit-test", "build-image", "build-python-wheel-artifact", "build-go-binary-artifact"]
TEST_TASKS = ["run-integration-script"]
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


def evaluate(policy, obj, user=PAC, namespace=NS, version="v1", operation="CREATE"):
    """-> None when the policy does not apply, else the list of failing validation messages."""
    request = {"userInfo": {"username": user}, "namespace": namespace, "operation": operation, "resource": {"version": version}}
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


SHA = "b01565a54f78f0f28d13ec2a2fca114b3f9b3569"
REPO = "https://github.com/o/sample"


def meta(event="push", branch="main", pr="17"):
    return {
        "generateName": "x-",
        "annotations": {"pipelinesascode.tekton.dev/repo-url": REPO, "pipelinesascode.tekton.dev/sha": SHA,
                        "pipelinesascode.tekton.dev/event-type": event, "pipelinesascode.tekton.dev/branch": branch},
        "labels": {"pipelinesascode.tekton.dev/pull-request": pr},
    }


def ref(name):
    return {"resolver": "cluster", "params": [{"name": "kind", "value": "pipeline"}, {"name": "name", "value": name},
                                              {"name": "namespace", "value": "platform-catalog"}]}


def prm(**kw):
    return [{"name": k.replace("_", "-"), "value": v} for k, v in kw.items()]


BUILD_PARAMS = prm(git_url=REPO, git_revision=SHA, image_repo="ghcr.io/o/sample", pr_tag_only="false",
                   app_namespace=NS, app_name="sample", app_type="app", is_flow_terminal="false")


def run(name="build", metadata=None, **spec):
    base = {
        "apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
        "metadata": metadata or meta("pull_request", "main"),
        "spec": {
            "pipelineRef": ref(name),
            "params": list(BUILD_PARAMS),
            "taskRunTemplate": {"serviceAccountName": "pipeline-runner"},
            "workspaces": [{"name": "source", "volumeClaimTemplate": {"spec": {}}}],
            "taskRunSpecs": [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS],
        },
    }
    base["spec"].update(spec)
    for k in [k for k, v in base["spec"].items() if v is None]:
        del base["spec"][k]
    return base


def flow(name, **kw):
    """A generated .tekton/flow-*.yaml run for a non-build root stage (push only)."""
    return run(name, metadata=meta("push", "main"), taskRunSpecs=None, **kw)


TEMPLATES = os.path.join(ROOT, "charts", "glidepath-app", "files", "onboarding-templates")
SUBST = {"{{ repo_url }}": REPO, "{{ revision }}": SHA, "{{ pull_request_number }}": "17", "{{ body.before }}": SHA,
         "<APP_NAME>": "sample", "<APP_NAMESPACE>": NS, "<APP_TYPE>": "app", "<GITHUB_OWNER>": "o",
         "<GITOPS_REPO_URL>": "https://github.com/o/gitops-sample"}


def delivered_templates():
    """(path, PipelineRun with the placeholders PaC and the onboarding task fill in, the event PaC would record)"""
    out = []
    for path in sorted(glob.glob(os.path.join(TEMPLATES, "*", "*.yaml"))):
        with open(path) as f:
            text = f.read()
        for k, v in SUBST.items():
            text = text.replace(k, v)
        for d in yaml.safe_load_all(text):
            if d and d.get("kind") == "PipelineRun":
                base = os.path.basename(path)
                event = "pull_request" if base.startswith("pull-request-") and base != "pull-request-merged.yaml" else "push"
                d["metadata"] = dict(meta(event, "main"), generateName=d["metadata"].get("generateName", "x-"))
                out.append((path, d))
    return out


def template_pipeline(d):
    return [p["value"] for p in d["spec"]["pipelineRef"]["params"] if p["name"] == "name"][0]


def generator_params():
    """The parameter names deliver-onboarding-files writes into a generated flow file (the heredoc and its helper lines)."""
    task = [d for d in render_catalog() if d["kind"] == "Task" and d["metadata"]["name"] == "deliver-onboarding-files"][0]
    script = "\n".join(s.get("script", "") for s in task["spec"]["steps"])
    a = script.index("local flow_name=")
    b = script.index("YAMLEOF", script.index("YAMLEOF") + 1)
    return sorted(set(re.findall(r"\{ name: ([a-z][a-z-]*), value: \\?\"[^}]*\}", script[a:b]))) , script[a:b]


_CATALOG = []


def render_catalog():
    if not _CATALOG:
        r = subprocess.run(["helm", "template", "c", os.path.join(ROOT, "charts", "glidepath-catalog"), "-f",
                            os.path.join(ROOT, "charts", "glidepath-catalog", "tests", "values.example.yaml")],
                           capture_output=True, text=True)
        assert r.returncode == 0, r.stderr
        _CATALOG.extend(d for d in yaml.safe_load_all(r.stdout) if d)
    return _CATALOG


def repo_code(task):
    return any(x.get("image") == "$(params.agent-image)" or "buildctl" in x.get("script", "") for x in task["spec"].get("steps", []))


class CicdRunAdmission(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.pols, cls.binds = policies()
        cls.shape = cls.pols["cicd-run-shape"]
        cls.iso = cls.pols["cicd-run-source-isolation"]
        cls.values = yaml.safe_load(open(os.path.join(CHART, "values.yaml")))["cicdRunAdmission"]

    def refused(self, obj, policy=None, msg=None, **kw):
        out = evaluate(policy or self.shape, obj, **kw)
        self.assertTrue(out, "expected a refusal, got %r" % (out,))
        if msg:
            self.assertTrue(any(msg in m for m in out), out)

    def admitted(self, obj, policy=None, **kw):
        self.assertEqual(evaluate(policy or self.shape, obj, **kw), [])

    # ---- the policies as rendered ----
    def test_bindings_deny_and_are_failclosed_with_guards(self):
        self.assertEqual(self.binds["cicd-run-shape"]["validationActions"], ["Deny"])
        self.assertEqual(self.binds["cicd-run-source-isolation"]["validationActions"], ["Deny"])
        self.assertEqual(self.binds["cicd-run-no-taskruns"]["validationActions"], ["Deny"])
        for p in self.pols.values():
            self.assertEqual(p["failurePolicy"], "Fail")
            self.assertTrue(p["matchConditions"], "a policy with no matchConditions applies to every run")

    def test_isolation_actions_are_a_value_and_a_non_deny_state_is_visible(self):
        docs = render("--set", "cicdRunAdmission.sourceIsolationActions={Warn,Audit}", "--api-versions", "monitoring.coreos.com/v1")
        bind = [d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding" and d["metadata"]["name"] == "cicd-run-source-isolation"][0]
        self.assertEqual(bind["spec"]["validationActions"], ["Warn", "Audit"])
        self.assertIn("NOT ENFORCING", bind["metadata"]["annotations"]["glidepath.io/enforcement"])
        rules = [d for d in docs if d["kind"] == "PrometheusRule" and d["metadata"]["name"] == "cicd-run-source-isolation-not-enforcing"]
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]["spec"]["groups"][0]["rules"][0]["alert"], "CicdRunSourceIsolationNotEnforcing")
        # and in Deny: no annotation, no alert
        docs = render("--api-versions", "monitoring.coreos.com/v1")
        self.assertFalse([d for d in docs if d["kind"] == "PrometheusRule" and d["metadata"]["name"] == "cicd-run-source-isolation-not-enforcing"])
        bind = [d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding" and d["metadata"]["name"] == "cicd-run-source-isolation"][0]
        self.assertNotIn("annotations", bind["metadata"])

    def test_off_renders_nothing(self):
        pols, binds = policies("--set", "cicdRunAdmission.enabled=false")
        self.assertFalse([n for n in list(pols) + list(binds) if n.startswith("cicd-run-")])

    # ---- what is admitted ----
    def test_generated_build_run_is_admitted(self):
        self.admitted(run())
        self.admitted(run(), self.iso)

    def test_every_delivered_template_is_admitted(self):
        found = delivered_templates()
        self.assertGreater(len(found), 15, "the glob found too few templates: the comparison would be vacuous")
        for path, d in found:
            self.admitted(d)

    def test_generated_flow_files_for_every_root_stage_are_admitted(self):
        for stage in ("test", "deploy", "release", "gitops-image-bump"):
            params = list(BUILD_PARAMS) + prm(env="dev", test_name="integration", cluster="example-prod", name="x",
                                              pin_file="glidepath/releases/prod.yaml")
            self.admitted(flow(stage, params=params))
        tag = run("release", metadata=meta("push", "refs/tags/v1.2.3"), taskRunSpecs=None,
                  params=list(BUILD_PARAMS) + prm(git_tag="refs/tags/v1.2.3"))
        self.admitted(tag)

    # ---- the allowlist is what the platform delivers (cannot drift) ----
    def test_the_pipelines_a_file_may_start_are_exactly_the_ones_the_platform_starts_from_one(self):
        from_templates = {template_pipeline(d) for _, d in delivered_templates()}
        stages = set(json.load(open(os.path.join(ROOT, "schemas", "cicd.schema.json")))
                     ["properties"]["pipelines"]["additionalProperties"]["oneOf"][0]["properties"]["steps"]["items"]["properties"]["stage"]["enum"]) \
            if False else {"build", "test", "deploy", "release", "gitops-image-bump"}
        self.assertEqual(set(self.values["pipelines"]), from_templates | stages)
        catalog = {d["metadata"]["name"] for d in render_catalog() if d["kind"] == "Pipeline"}
        self.assertLessEqual(set(self.values["pipelines"]), catalog)
        for must_not in ("release-outcome-notify", "release-progress-notify", "release-rollback-notify", "promote", "governance-check"):
            self.assertIn(must_not, catalog)
            self.assertNotIn(must_not, self.values["pipelines"])

    def test_the_flow_stages_are_the_schemas(self):
        schema = json.load(open(os.path.join(ROOT, "schemas", "cicd.schema.json")))
        found = []

        def walk(o):
            if isinstance(o, dict):
                st = o.get("properties", {}).get("stage") if isinstance(o.get("properties"), dict) else None
                if st and "enum" in st:
                    found.append(set(st["enum"]))
                for v in o.values():
                    walk(v)
            elif isinstance(o, list):
                for v in o:
                    walk(v)
        walk(schema)
        self.assertTrue(found)
        # the schema's steps[].stage enum is the one with build in it
        stages = [f for f in found if "build" in f][0]
        self.assertEqual(stages, {"build", "test", "deploy", "release", "gitops-image-bump"})

    def test_each_templates_params_are_what_the_policy_lists(self):
        for path, d in delivered_templates():
            names = {p["name"] for p in d["spec"].get("params", [])}
            self.assertEqual(names, set(self.values["pipelines"][template_pipeline(d)]["params"]), path)

    def test_flow_generator_params_are_what_the_policy_lists_for_the_flow_stages(self):
        names, _ = generator_params()
        self.assertTrue(names)
        self.assertEqual(set(names), set(self.values["pipelines"]["build"]["params"]))
        for stage in ("test", "deploy", "release", "gitops-image-bump"):
            self.assertEqual(self.values["pipelines"][stage]["params"], self.values["pipelines"]["build"]["params"])

    def test_every_listed_param_has_a_pattern(self):
        for name, cfg in self.values["pipelines"].items():
            for p in cfg["params"]:
                self.assertIn(p, self.values["paramPatterns"], (name, p))

    def test_source_tasks_are_derived_from_the_catalog(self):
        docs = render_catalog()
        tasks = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Task"}
        pipelines = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Pipeline"}
        for pname, listed in self.values["sourceTasks"].items():
            runs_repo_code = set()
            for t in pipelines[pname]["spec"]["tasks"]:
                ref_name = [p["value"] for p in t["taskRef"]["params"] if p["name"] == "name"][0]
                if repo_code(tasks[ref_name]):
                    runs_repo_code.add(t["name"])
            self.assertEqual(runs_repo_code, set(listed), pname)
        # and no other catalog Pipeline a file may start has such a task
        for pname in self.values["pipelines"]:
            if pname in self.values["sourceTasks"]:
                continue
            for sec in ("tasks", "finally"):
                for t in pipelines[pname]["spec"].get(sec, []):
                    ref_name = [p["value"] for p in t["taskRef"]["params"] if p["name"] == "name"][0]
                    self.assertFalse(repo_code(tasks[ref_name]), (pname, t["name"]))

    # ---- what is refused ----
    def test_inline_pipeline_spec_refused(self):
        self.refused(run(pipelineSpec={"tasks": [{"name": "x", "taskSpec": {"steps": []}}]}), msg="inline pipelineSpec")

    def test_missing_pipeline_ref_refused(self):
        self.refused(run(pipelineRef=None, pipelineSpec={"tasks": []}))
        self.refused(run(pipelineRef=None))

    def test_pipeline_ref_other_than_the_allowlist_refused(self):
        for bad in (
            {"resolver": "git", "params": [{"name": "url", "value": "https://evil"}]},
            {"resolver": "cluster", "params": [{"name": "kind", "value": "pipeline"}, {"name": "name", "value": "build"},
                                               {"name": "namespace", "value": NS}]},
            {"resolver": "cluster", "params": [{"name": "kind", "value": "task"}, {"name": "name", "value": "build"},
                                               {"name": "namespace", "value": "platform-catalog"}]},
            {"name": "build"},
        ):
            self.refused(run(pipelineRef=bad), msg="pipelineRef")

    def test_catalog_pipelines_the_platform_starts_are_not_startable_from_a_file(self):
        # the review's case: a pipeline that writes release state, with attacker-chosen parameters
        for name in ("release-outcome-notify", "release-progress-notify", "release-rollback-notify", "promote", "governance-check",
                     "not-a-pipeline"):
            self.refused(run(name, params=prm(app_namespace=NS, app_name="sample", env="prod", status="Failed")), msg="pipelineRef")

    def test_parameters_are_checked(self):
        meta_push = meta("push", "main")
        # unknown name, duplicate name, wrong shape, shell metacharacters
        for bad in (BUILD_PARAMS + prm(config_json="{}"), BUILD_PARAMS + prm(git_url=REPO), prm(git_url=REPO + "; id"),
                    [p for p in BUILD_PARAMS if p["name"] != "image-repo"] + prm(image_repo="x y"),
                    [p for p in BUILD_PARAMS if p["name"] != "is-flow-terminal"] + prm(is_flow_terminal="maybe")):
            self.refused(run(params=bad), msg="parameters")
        self.refused(run("release", metadata=meta_push, taskRunSpecs=None, params=list(BUILD_PARAMS) + prm(pin_file="../../x.yaml")), msg="parameters")
        self.refused(run("release", metadata=meta_push, taskRunSpecs=None, params=list(BUILD_PARAMS) + prm(env="prod;rm")), msg="parameters")
        self.refused(run("release", metadata=meta_push, taskRunSpecs=None, params=list(BUILD_PARAMS) + prm(git_tag="v1")), msg="parameters")

    def test_the_event_decides_not_the_file(self):
        msg = "recorded for this event"
        self.refused(run(params=[p if p["name"] != "git-revision" else {"name": "git-revision", "value": "0" * 40} for p in BUILD_PARAMS]), msg=msg)
        self.refused(run(params=[p if p["name"] != "git-url" else {"name": "git-url", "value": "https://github.com/victim/repo"} for p in BUILD_PARAMS]), msg=msg)
        self.refused(run(params=[p if p["name"] != "app-namespace" else {"name": "app-namespace", "value": "app-other-cicd"} for p in BUILD_PARAMS]), msg=msg)
        self.refused(run(params=[p if p["name"] != "app-name" else {"name": "app-name", "value": "other"} for p in BUILD_PARAMS]), msg=msg)
        self.refused(run(params=[p if p["name"] != "app-type" else {"name": "app-type", "value": "infra"} for p in BUILD_PARAMS]), msg=msg)
        chk = run("cicd-validate-check", taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA, pr_number="99"))
        self.refused(chk, msg=msg)               # the event's pull request is 17
        chk["spec"]["params"] = prm(repo_url=REPO, revision=SHA, pr_number="17")
        self.admitted(chk)
        # a file that omits PaC's own record cannot satisfy a binding either
        bare = run(metadata={"generateName": "x-"})
        self.refused(bare, msg=msg)

    def test_state_writing_pipelines_start_only_from_a_push_to_a_protected_ref(self):
        params = list(BUILD_PARAMS) + prm(env="prod")
        for stage in ("test", "deploy", "release", "gitops-image-bump"):
            self.admitted(flow(stage, params=params))
            self.refused(run(stage, metadata=meta("pull_request", "main"), taskRunSpecs=None, params=params), msg="push event")
            self.refused(run(stage, metadata=meta("push", "attacker-branch"), taskRunSpecs=None, params=params), msg="protected branch")
        self.admitted(run("release", metadata=meta("push", "refs/tags/v1.0.0"), taskRunSpecs=None, params=params))
        # the PR build is the one flow stage a pull request starts, and it runs the isolated tasks
        self.admitted(run("build", metadata=meta("pull_request", "feature-x")))
        # a check is held to a protected target branch too
        chk = run("sast-check", metadata=meta("pull_request", "side"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA, pr_number="17"))
        self.refused(chk, msg="protected branch")

    def test_only_v1(self):
        self.refused(run(), version="v1beta1", msg="v1")
        # v1beta1 spells the run-wide settings at the top level
        old = run(taskRunTemplate=None, serviceAccountName="default", podTemplate={"volumes": []})
        self.refused(old, version="v1beta1")
        self.refused(old)        # and even asked as v1, no taskRunTemplate: refused

    def test_other_service_accounts_refused(self):
        for sa in ("default", "glidepath-cloud-deployer", "pipeline-trigger", "source-runner"):
            self.refused(run(taskRunTemplate={"serviceAccountName": sa}), msg="pipeline-runner")
        self.refused(run(taskRunTemplate={}), msg="pipeline-runner")
        self.refused(run(taskRunTemplate=None), msg="pipeline-runner")

    def test_task_overrides_limited(self):
        ok = [{"pipelineTaskName": "deploy-aws-lambda", "serviceAccountName": "glidepath-cloud-deployer"}]
        self.admitted(run("deploy", metadata=meta("push", "main"), taskRunSpecs=ok))
        for bad in (
            [{"pipelineTaskName": "x", "serviceAccountName": "pipeline-trigger"}],
            [{"pipelineTaskName": "x", "serviceAccountName": "default"}],
            [{"pipelineTaskName": "x"}],
            [{"pipelineTaskName": "build-source", "serviceAccountName": "source-runner", "podTemplate": {"volumes": []}}],
            [{"pipelineTaskName": "build-source", "serviceAccountName": "source-runner", "stepSpecs": []}],
        ):
            self.refused(run(taskRunSpecs=bad), msg="taskRunSpecs")

    def test_a_task_listed_twice_refused(self):
        twice = [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS]
        twice.append({"pipelineTaskName": "build-source", "serviceAccountName": "glidepath-cloud-deployer"})
        self.refused(run(taskRunSpecs=twice), msg="once per task")

    def test_pod_template_only_host_aliases(self):
        ok = {"serviceAccountName": "pipeline-runner", "podTemplate": {"hostAliases": [{"ip": "10.0.0.1", "hostnames": ["a"]}]}}
        self.admitted(run(taskRunTemplate=ok))
        bad = {"serviceAccountName": "pipeline-runner", "podTemplate": {"volumes": [{"name": "s", "secret": {"secretName": "x"}}]}}
        self.refused(run(taskRunTemplate=bad), msg="podTemplate")

    def test_workspaces_limited(self):
        self.refused(run(workspaces=[{"name": "source", "secret": {"secretName": "app-secrets"}}]), msg="Workspaces")
        self.refused(run(workspaces=[{"name": "source", "persistentVolumeClaim": {"claimName": "x"}}]), msg="Workspaces")

    def test_an_update_by_pac_is_held_to_the_same_shape(self):
        self.assertIn("UPDATE", self.pols["cicd-run-shape"]["matchConstraints"]["resourceRules"][0]["operations"])
        self.refused(run(pipelineSpec={"tasks": []}), operation="UPDATE")

    def test_pac_never_creates_a_taskrun(self):
        pol = self.pols["cicd-run-no-taskruns"]
        tr = {"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "metadata": {}, "spec": {"taskRef": {"name": "x"}}}
        self.assertTrue(evaluate(pol, tr))
        self.assertTrue(evaluate(pol, tr, operation="UPDATE"))
        self.assertIsNone(evaluate(pol, tr, user="system:serviceaccount:tekton-pipelines:tekton-pipelines-controller"))
        self.assertIsNone(evaluate(pol, tr, user="kubernetes-admin"))

    # ---- source isolation ----
    def test_source_isolation_for_every_pipeline_that_runs_repository_code(self):
        for specs in (None, [], [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS[:3]],
                      [{"pipelineTaskName": t, "serviceAccountName": "pipeline-runner"} for t in SOURCE_TASKS]):
            self.refused(run(taskRunSpecs=specs), self.iso, "source-runner")
        # the review's case: the test Pipeline, no taskRunSpecs, runs ./integration-test.sh with pipeline-runner's token
        params = list(BUILD_PARAMS) + prm(env="dev", test_name="integration")
        t = run("test", metadata=meta("push", "main"), taskRunSpecs=None, params=params)
        self.refused(t, self.iso, "run-integration-script")
        t["spec"]["taskRunSpecs"] = [{"pipelineTaskName": x, "serviceAccountName": "source-runner"} for x in TEST_TASKS]
        self.admitted(t, self.iso)
        self.admitted(t)
        other = run("cicd-validate-check", taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA, pr_number="17"))
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
        for spec in ({}, {"pipelineRef": {}}, {"pipelineRef": {"params": []}}, {"taskRunSpecs": [{}]}, {"params": [{}]},
                     {"params": [{"name": "git-url"}]}, {"params": [{"name": "git-url", "value": ["a"]}]},
                     {"taskRunTemplate": {"podTemplate": {}}}, {"workspaces": [{}]}, {"pipelineRef": {"params": [{}]}},
                     {"pipelineRef": {"params": [{"name": "name"}]}}, {"pipelineRef": {"params": [{"name": "name", "value": 3}]}},
                     {"pipelineRef": {"params": [{"name": "name", "value": "build"}]}},
                     {"pipelineRef": {"params": [{"name": "name", "value": "test"}]}, "taskRunSpecs": [{}]}):
            for md in ({}, {"annotations": {}, "labels": {}}, meta()):
                obj = {"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "metadata": md, "spec": spec}
                self.assertIsInstance(evaluate(self.shape, obj), list)
                evaluate(self.iso, obj)
        for obj in ({"spec": {}}, {}):
            for pol in (self.shape, self.iso):
                evaluate(pol, copy.deepcopy(obj))


if __name__ == "__main__":
    unittest.main()
