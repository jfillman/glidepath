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
PUSHER_TASKS = ["push-image"]
TEST_TASKS = ["run-integration-script"]
ENV = celpy.Environment()
# the volume claim shape the onboarding templates and the flow generator write
VCT = {"spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "2Gi"}}}}
# What a typed client (Pipelines-as-Code builds typed PipelineRuns; Tekton's WorkspaceBinding.VolumeClaimTemplate is a corev1.PersistentVolumeClaim)
# writes for the same template: Go's encoding/json emits a struct field even when it is empty, so a stored run carries
# `metadata: {creationTimestamp: null}` and `status: {}` (what `kubectl get pipelinerun -o yaml` shows for any run that has a volumeClaimTemplate).
REAL_VCT = {"metadata": {"creationTimestamp": None}, "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "2Gi"}}}, "status": {}}


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


def meta(event="push", branch="main", pr="17", source=None):
    """What PaC records on a run (live, hangar-dev): for a push, `branch` is the ref with refs/heads/ trimmed and `source-branch`
    the full ref; for a pull request `branch` is the target and `source-branch` the head branch's short name."""
    if source is None:
        if event == "push":
            source = branch if branch.startswith("refs/") else "refs/heads/" + branch
        else:
            source = "feature-x"
    return {
        "generateName": "x-",
        "annotations": {"pipelinesascode.tekton.dev/repo-url": REPO, "pipelinesascode.tekton.dev/sha": SHA,
                        "pipelinesascode.tekton.dev/event-type": event, "pipelinesascode.tekton.dev/branch": branch,
                        "pipelinesascode.tekton.dev/source-branch": source},
        "labels": {"pipelinesascode.tekton.dev/pull-request": pr, "pipelinesascode.tekton.dev/url-org": "o",
                   "pipelinesascode.tekton.dev/url-repository": "sample"},
    }


def ref(name):
    return {"resolver": "cluster", "params": [{"name": "kind", "value": "pipeline"}, {"name": "name", "value": name},
                                              {"name": "namespace", "value": "platform-catalog"}]}


def prm(**kw):
    return [{"name": k.replace("_", "-"), "value": v} for k, v in kw.items()]


BUILD_PARAMS = prm(git_url=REPO, git_revision=SHA, image_repo="registry.example/o/sample", pr_tag_only="false",
                   app_namespace=NS, app_name="sample", app_type="app", is_flow_terminal="false")


def with_params(params, **kw):
    """BUILD_PARAMS with some values replaced (names as keyword arguments, _ for -)."""
    new = {k.replace("_", "-"): v for k, v in kw.items()}
    return [dict(p, value=new.get(p["name"], p["value"])) for p in params]


# what the onboarding generator writes for a pull request's build: pr-tag-only and the -pr repository go together
PR_BUILD_PARAMS = with_params(BUILD_PARAMS, pr_tag_only="true", image_repo="registry.example/o/sample-pr")


def run(name="build", metadata=None, **spec):
    md = metadata or meta("pull_request", "main")
    pr_build = name == "build" and md.get("annotations", {}).get("pipelinesascode.tekton.dev/event-type") == "pull_request"
    base = {
        "apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
        "metadata": md,
        "spec": {
            "pipelineRef": ref(name),
            # the default run is a pull request's build, which is what the generator writes for one
            "params": list(PR_BUILD_PARAMS if pr_build else BUILD_PARAMS),
            "taskRunTemplate": {"serviceAccountName": "pipeline-runner"},
            "workspaces": [{"name": "source", "volumeClaimTemplate": copy.deepcopy(VCT)}],
            "taskRunSpecs": [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS]
                            + [{"pipelineTaskName": t, "serviceAccountName": "image-pusher"} for t in PUSHER_TASKS],
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


# The catalog renderings the onboarding task's task_names filter (the <CLEARANCE_REGISTER_TASK>, <ITSM_GATE_TASK> and <IF_...> markers of the
# templates' taskRunSpecs, pipeline trust T4 step 3) is run against: what is rendered decides which tasks the Pipelines have.
#   (name, helm args)
CATALOG_VARIANTS = [
    ("itsm, clearance advisory", ["--set", "itsm.provider=webhook"]),
    ("itsm advisory, clearance required", ["--set", "itsm.provider=webhook", "--set", "releaseGuardrailOverrides.clearance-gate.required=true",
                                          "--set", "releaseGuardrailOverrides.itsm.required=false"]),
    ("no change-management provider", []),
]


def task_names_filter(extra):
    """The task_names function deliver-onboarding-files renders for this catalog (a sed filter), as a callable text -> text."""
    docs = render_catalog_itsm(*extra) if extra else render_catalog()
    task = [d for d in docs if d["kind"] == "Task" and d["metadata"]["name"] == "deliver-onboarding-files"][0]
    fns = set()
    for s in task["spec"]["steps"]:
        fns |= set(re.findall(r"^task_names\(\) \{.*?^\}$", s.get("script", ""), re.S | re.M))
    assert len(fns) == 1, "both steps of deliver-onboarding-files must render the same task_names function: %s" % fns

    def apply(text):
        r = subprocess.run(["bash", "-c", fns.copy().pop() + "\ntask_names"], input=text, capture_output=True, text=True)
        assert r.returncode == 0, r.stderr
        return r.stdout
    return apply


def delivered_templates(variants=None):
    """(path, PipelineRun with the placeholders PaC and the onboarding task fill in, the event PaC would record), once per catalog variant"""
    out = []
    for _name, extra in (variants if variants is not None else CATALOG_VARIANTS):
        names = task_names_filter(extra)
        for path in sorted(glob.glob(os.path.join(TEMPLATES, "*", "*.yaml"))):
            with open(path) as f:
                text = f.read()
            for k, v in SUBST.items():
                text = text.replace(k, v)
            if os.path.basename(os.path.dirname(path)) == "gitops-repo":
                text = names(text)   # the gitops templates are the ones the onboarding task runs through task_names
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


_CATALOG_ITSM = {}


def render_catalog_itsm(*extra):
    """The catalog with a change-management provider set (the itsm tasks of the Pipelines render only then); `extra` are more helm args."""
    key = tuple(extra)
    if key not in _CATALOG_ITSM:
        r = subprocess.run(["helm", "template", "c", os.path.join(ROOT, "charts", "glidepath-catalog"), "--set", "itsm.provider=webhook", *extra, "-f",
                            os.path.join(ROOT, "charts", "glidepath-catalog", "tests", "values.example.yaml")],
                           capture_output=True, text=True)
        assert r.returncode == 0, r.stderr
        _CATALOG_ITSM[key] = [d for d in yaml.safe_load_all(r.stdout) if d]
    return _CATALOG_ITSM[key]


def repo_code(task):
    """Runs the repository's own code, or renders a chart it chooses (render-manifests: labelled, so a comment that names helm is not one)."""
    return task["metadata"].get("labels", {}).get("glidepath.io/renders-repository-content") == "true" or \
        any(x.get("image") == "$(params.agent-image)" or "buildctl" in x.get("script", "") for x in task["spec"].get("steps", []))


def schema_stages():
    """The stage names cicd.yaml's steps[].stage accepts (the enum that contains `build`), read from the schema."""
    with open(os.path.join(ROOT, "schemas", "cicd.schema.json")) as f:
        schema = json.load(f)
    found = []

    def walk(o):
        if isinstance(o, dict):
            st = o.get("properties", {}).get("stage") if isinstance(o.get("properties"), dict) else None
            if isinstance(st, dict) and "enum" in st:
                found.append(set(st["enum"]))
            for v in o.values():
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)
    walk(schema)
    stages = [f for f in found if "build" in f]
    assert len(stages) == 1, "expected exactly one stage enum in the schema, found %d" % len(stages)
    return stages[0]


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

    BINDINGS = {"cicd-run-shape": ("shapeActions", "CicdRunShapeNotEnforcing"),
                "cicd-run-source-isolation": ("sourceIsolationActions", "CicdRunSourceIsolationNotEnforcing"),
                "cicd-run-no-taskruns": ("noTaskRunsActions", "CicdRunNoTaskRunsNotEnforcing")}

    def bindings_of(self, docs):
        return {d["metadata"]["name"]: d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding"}

    def alerts_of(self, docs):
        return [r["alert"] for d in docs if d["kind"] == "PrometheusRule" and d["metadata"]["name"] == "cicd-run-admission-not-enforcing"
                for g in d["spec"]["groups"] for r in g["rules"]]

    def test_every_binding_takes_its_actions_from_a_value_and_a_non_deny_state_is_visible(self):
        for name, (value, alert) in self.BINDINGS.items():
            docs = render("--set", "cicdRunAdmission.%s={Warn,Audit}" % value, "--api-versions", "monitoring.coreos.com/v1")
            binds = self.bindings_of(docs)
            self.assertEqual(binds[name]["spec"]["validationActions"], ["Warn", "Audit"], name)
            self.assertIn("NOT ENFORCING", binds[name]["metadata"]["annotations"]["glidepath.io/enforcement"], name)
            self.assertEqual(self.alerts_of(docs), [alert], "only the binding that is not in Deny alerts")
            for other in set(self.BINDINGS) - {name}:
                self.assertEqual(binds[other]["spec"]["validationActions"], ["Deny"], other)
                self.assertNotIn("annotations", binds[other]["metadata"], other)
        # in Deny, the default: no annotation, no alert
        docs = render("--api-versions", "monitoring.coreos.com/v1")
        self.assertEqual(self.alerts_of(docs), [])
        for name, bind in self.bindings_of(docs).items():
            self.assertEqual(bind["spec"]["validationActions"], ["Deny"], name)
            self.assertNotIn("annotations", bind["metadata"], name)
        # without prometheus-operator the annotation still says it, and no alert is rendered
        docs = render("--set", "cicdRunAdmission.shapeActions={Warn}")
        self.assertEqual(self.alerts_of(docs), [])
        self.assertIn("NOT ENFORCING", self.bindings_of(docs)["cicd-run-shape"]["metadata"]["annotations"]["glidepath.io/enforcement"])

    def test_a_warn_and_audit_binding_that_also_denies_is_enforcing(self):
        docs = render("--set", "cicdRunAdmission.shapeActions={Deny,Audit}")
        self.assertNotIn("annotations", self.bindings_of(docs)["cicd-run-shape"]["metadata"])

    def test_the_build_pull_credential_is_rendered_only_when_named_and_is_never_the_push_credential(self):
        def ces(*extra):
            return [d for d in render(*extra) if d["kind"] == "ClusterExternalSecret"]
        base = {d["metadata"]["name"] for d in ces()}
        self.assertIn("registry-credentials", base)
        self.assertNotIn("build-pull-credentials", base, "nothing about a pull credential unless the operator names a key")
        docs = ces("--set", "buildPullCredential.remoteKey=build-pull-dockerconfigjson")
        pull = [d for d in docs if d["metadata"]["name"] == "build-pull-credentials"]
        self.assertEqual(len(pull), 1)
        spec = pull[0]["spec"]["externalSecretSpec"]
        self.assertEqual(spec["target"]["name"], "build-pull-credentials")
        self.assertEqual(spec["target"]["template"]["type"], "kubernetes.io/dockerconfigjson")
        self.assertEqual(spec["data"], [{"secretKey": ".dockerconfigjson", "remoteRef": {"key": "build-pull-dockerconfigjson"}}])
        # the push credential is untouched
        push = [d for d in docs if d["metadata"]["name"] == "registry-credentials"][0]
        self.assertEqual(push["spec"]["externalSecretSpec"]["data"][0]["remoteRef"]["key"], "registry-credentials-dockerconfigjson")
        # and the build pod's credential cannot be configured to be the push credential
        for bad in (("buildPullCredential.remoteKey=registry-credentials-dockerconfigjson",),
                    ("buildPullCredential.remoteKey=x", "buildPullCredential.secretName=registry-credentials")):
            r = subprocess.run(["helm", "template", "cp", CHART, "-f", VALUES] + [a for v in bad for a in ("--set", v)], capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0, bad)

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
        stages = schema_stages()
        self.assertEqual(set(self.values["pipelines"]), from_templates | stages)
        catalog = {d["metadata"]["name"] for d in render_catalog() if d["kind"] == "Pipeline"}
        self.assertLessEqual(set(self.values["pipelines"]), catalog)
        for must_not in ("release-outcome-notify", "release-progress-notify", "release-rollback-notify", "promote", "governance-check"):
            self.assertIn(must_not, catalog)
            self.assertNotIn(must_not, self.values["pipelines"])

    def test_the_flow_stages_are_the_schemas(self):
        self.assertEqual(schema_stages(), {"build", "test", "deploy", "release", "gitops-image-bump"})

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
        ):
            self.refused(run(taskRunSpecs=bad), msg="may run as")
        for bad in (
            [{"pipelineTaskName": "x"}],
            [{"pipelineTaskName": "build-source", "serviceAccountName": "source-runner", "podTemplate": {"volumes": []}}],
            [{"pipelineTaskName": "build-source", "serviceAccountName": "source-runner", "stepSpecs": []}],
        ):
            self.refused(run(taskRunSpecs=bad), msg="taskRunSpecs entries")

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

    def test_a_workspace_volume_claim_has_only_the_shape_the_templates_use(self):
        def vct(**spec):
            body = copy.deepcopy(VCT)
            body["spec"].update(spec)
            return [{"name": "gate", "volumeClaimTemplate": body}]
        self.admitted(run(workspaces=vct()))
        self.admitted(run(workspaces=[{"name": "gate", "emptyDir": {}}]))
        # the shape a typed client writes (Pipelines-as-Code): metadata.creationTimestamp null, empty status
        self.admitted(run(workspaces=[{"name": "gate", "volumeClaimTemplate": copy.deepcopy(REAL_VCT)}]))
        self.admitted(run(workspaces=[{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), metadata={})}]))
        self.admitted(run(workspaces=[{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(VCT), status={})}]))
        for storage in ("1Gi", "5Gi", "10Gi", "20Gi"):
            self.admitted(run(workspaces=vct(resources={"requests": {"storage": storage}})))
        refusals = {
            "dataSource (a clone of another volume)": vct(dataSource={"kind": "PersistentVolumeClaim", "name": "source-pvc"}),
            "dataSourceRef": vct(dataSourceRef={"kind": "PersistentVolumeClaim", "name": "source-pvc"}),
            "volumeName": vct(volumeName="pv-1"),
            "selector": vct(selector={"matchLabels": {"a": "b"}}),
            "volumeMode": vct(volumeMode="Block"),
            "a storageClassName that is not allowed": vct(storageClassName="fast"),
            "another access mode": vct(accessModes=["ReadWriteMany"]),
            "two access modes": vct(accessModes=["ReadWriteOnce", "ReadWriteMany"]),
            "a size over the limit": vct(resources={"requests": {"storage": "500Gi"}}),
            "a size in another unit": vct(resources={"requests": {"storage": "2048Mi"}}),
            "a limit as well": vct(resources={"requests": {"storage": "2Gi"}, "limits": {"storage": "2Gi"}}),
            "no size": vct(resources={}),
            "no spec": [{"name": "gate", "volumeClaimTemplate": {}}],
            "metadata with an annotation": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(VCT), metadata={"annotations": {"a": "b"}})}],
            "metadata with a name": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), metadata={"name": "x", "creationTimestamp": None})}],
            "metadata with a generateName": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), metadata={"generateName": "x-"})}],
            "metadata with labels": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), metadata={"labels": {"a": "b"}})}],
            "metadata with owner references": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), metadata={"ownerReferences": [{"name": "x"}]})}],
            "a creationTimestamp that is a value": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), metadata={"creationTimestamp": "2026-01-01T00:00:00Z"})}],
            "a status with content": [{"name": "gate", "volumeClaimTemplate": dict(copy.deepcopy(REAL_VCT), status={"phase": "Bound"})}],
            "a typed template that also clones a volume": [{"name": "gate", "volumeClaimTemplate": {"metadata": {"creationTimestamp": None}, "status": {}, "spec": dict(copy.deepcopy(VCT)["spec"], dataSourceRef={"kind": "PersistentVolumeClaim", "name": "x"})}}],
        }
        for what, ws in refusals.items():
            self.refused(run(workspaces=ws), msg="volumeClaimTemplate")

    def test_a_workspace_storage_class_can_be_allowed_by_value(self):
        pols, _ = policies("--set", "cicdRunAdmission.workspaceStorageClassNames={gp3}")
        ws = [{"name": "gate", "volumeClaimTemplate": {"spec": dict(copy.deepcopy(VCT)["spec"], storageClassName="gp3")}}]
        self.assertEqual(evaluate(pols["cicd-run-shape"], run(workspaces=ws)), [])
        ws[0]["volumeClaimTemplate"]["spec"]["storageClassName"] = "other"
        self.assertTrue(evaluate(pols["cicd-run-shape"], run(workspaces=ws)))

    def test_an_update_by_pac_is_held_to_the_same_shape(self):
        self.assertIn("UPDATE", self.pols["cicd-run-shape"]["matchConstraints"]["resourceRules"][0]["operations"])
        self.refused(run(pipelineSpec={"tasks": []}), operation="UPDATE")

    def test_a_run_pipelines_as_code_queues_is_admitted_pending_and_when_it_starts(self):
        # Repository concurrency_limit: PaC creates every run of the repository with spec.status PipelineRunPending and
        # state=queued, patches the execution order on it, and the watcher later clears spec.status and sets state=started.
        # Nothing in the policy reads spec.status or the state, so the same shape is admitted at each step.
        for event in ("pull_request", "push"):
            md = meta(event, "main")
            md.setdefault("labels", {})["pipelinesascode.tekton.dev/state"] = "queued"
            md["annotations"]["pipelinesascode.tekton.dev/state"] = "queued"
            md["annotations"]["pipelinesascode.tekton.dev/execution-order"] = "%s/a,%s/b" % (NS, NS)
            queued = run("build", metadata=md, status="PipelineRunPending")
            self.admitted(queued)
            self.admitted(queued, operation="UPDATE")
            started = copy.deepcopy(queued)
            del started["spec"]["status"]
            started["metadata"]["labels"]["pipelinesascode.tekton.dev/state"] = "started"
            started["metadata"]["annotations"]["pipelinesascode.tekton.dev/state"] = "started"
            self.admitted(started, operation="UPDATE")
        # and a queued run is held to the same shape as any other: pending does not excuse an inline pipelineSpec
        self.refused(run(pipelineSpec={"tasks": []}, status="PipelineRunPending"))

    def test_pac_never_creates_a_taskrun(self):
        pol = self.pols["cicd-run-no-taskruns"]
        tr = {"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "metadata": {}, "spec": {"taskRef": {"name": "x"}}}
        self.assertTrue(evaluate(pol, tr))
        self.assertTrue(evaluate(pol, tr, operation="UPDATE"))
        self.assertIsNone(evaluate(pol, tr, user="system:serviceaccount:tekton-pipelines:tekton-pipelines-controller"))
        self.assertIsNone(evaluate(pol, tr, user="kubernetes-admin"))

    # ---- second review ----
    def test_a_branch_named_like_a_tag_is_not_a_tag(self):
        # `git push origin evil:refs/heads/refs/tags/v1.0.0`: PaC trims refs/heads/, so `branch` reads refs/tags/v1.0.0, but
        # `source-branch` keeps the full ref. The decision is made on the full ref.
        params = list(BUILD_PARAMS) + prm(env="prod")
        lookalike = meta("push", "refs/tags/v1.0.0", source="refs/heads/refs/tags/v1.0.0")
        self.refused(run("release", metadata=lookalike, taskRunSpecs=None, params=params), msg="protected branch")
        real = meta("push", "refs/tags/v1.0.0", source="refs/tags/v1.0.0")
        self.admitted(run("release", metadata=real, taskRunSpecs=None, params=params))

    def test_a_tag_must_match_the_tag_pattern(self):
        params = list(BUILD_PARAMS) + prm(env="prod")
        for tag in ("refs/tags/anything", "refs/tags/v1.0", "refs/tags/v1.0.0-evil", "refs/tags/x/v1.0.0"):
            self.refused(run("release", metadata=meta("push", tag, source=tag), taskRunSpecs=None, params=params), msg="protected branch")

    # ---- the event shapes Pipelines-as-Code v0.52.0 records at runtime (parse_payload.go), not what the template files say ----
    def test_a_rerun_of_a_push_check_records_the_bare_branch_and_is_admitted(self):
        # check_run / check_suite "rerequested" on a push: event-type push, source-branch = the check suite's head_branch, bare
        # (parse_payload.go:765,824,846-848,851). This is what the gitops repos' bypass-check-42rhw (app-parachute-cicd) carried:
        # event-type push, source-branch main, branch main, on-event [push], on-target-branch [main].
        bare = meta("push", "main", source="main")
        params = list(BUILD_PARAMS) + prm(env="prod")
        self.admitted(run("bypass-merge-check", metadata=bare, taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA)))
        for stage in ("test", "deploy", "release", "gitops-image-bump"):
            self.admitted(run(stage, metadata=bare, taskRunSpecs=None, params=params))
        # the real push of the same ref still is
        self.admitted(run("bypass-merge-check", metadata=meta("push", "main"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA)))

    def test_a_push_to_any_other_branch_is_refused_in_either_spelling(self):
        params = list(BUILD_PARAMS) + prm(env="prod")
        for src in ("refs/heads/attacker-branch", "attacker-branch", "refs/heads/main-evil", "main-evil", "Main", "refs/heads/release-1",
                    "refs/tags/main", "refs/heads/refs/heads/main", "", "v1.0.0", "refs/tags/v1.0"):
            for name, kw in (("bypass-merge-check", prm(repo_url=REPO, revision=SHA)), ("release", params), ("deploy", params)):
                self.refused(run(name, metadata=meta("push", "attacker-branch", source=src), taskRunSpecs=None, params=kw),
                             msg="protected branch")
        # a bare tag name is not a tag: only the full refs/tags/ ref is (and a GitHub re-run of a tag push cannot exist, head_branch is null)
        self.refused(run("release", metadata=meta("push", "v1.0.0", source="v1.0.0"), taskRunSpecs=None, params=params), msg="protected branch")

    def test_a_merged_pull_request_is_a_push_and_a_closed_one_never_starts_a_run(self):
        # PaC never matches a template against pull_request closed (pipelineascode.go:81-82, it only cancels), so a merged PR starts
        # bypass-merge-check through the PUSH its merge makes. A pull_request-typed run into a branch that is not protected is refused.
        chk = run("bypass-merge-check", metadata=meta("pull_request", "side"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA))
        self.refused(chk, msg="protected branch")
        chk = run("bypass-merge-check", metadata=meta("pull_request", "main"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA))
        self.admitted(chk)
        # the push a merge into a side branch makes
        self.refused(run("bypass-merge-check", metadata=meta("push", "side"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA)),
                     msg="protected branch")
        self.refused(run("bypass-merge-check", metadata=meta("push", "side", source="side"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA)),
                     msg="protected branch")

    def test_source_branch_main_does_not_forge_a_push_or_a_pull_request(self):
        # A file can write any annotation, but Pipelines-as-Code overwrites every key it records (kubeinteraction/labels.go:112-114), so the
        # policy sees PaC's value. What a fork or a branch can still influence is the HEAD branch name of a pull request, which PaC records as
        # source-branch: a PR from a branch named main into `side`, or a push event whose branch is another one, must not pass.
        params = list(BUILD_PARAMS) + prm(env="prod")
        pr_from_main = meta("pull_request", "side", source="main")
        self.refused(run("sast-check", metadata=pr_from_main, taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA, pr_number="17")),
                     msg="protected branch")
        self.refused(run("release", metadata=pr_from_main, taskRunSpecs=None, params=params), msg="push event")
        # a release that asks for push-only from a comment-triggered event (retest-comment, incoming) stays refused
        for event in ("retest-comment", "test-comment", "ok-to-test-comment", "incoming", "cancel-comment"):
            self.refused(run("release", metadata=meta(event, "main", source="main"), taskRunSpecs=None, params=params), msg="push event")

    def test_comment_triggered_retests_of_a_pull_request_check(self):
        # /retest on a pull request: event-type retest-comment, branch = the PR's base, source-branch = its head (parse_payload.go:80-100)
        ok = run("sast-check", metadata=meta("retest-comment", "main", source="release-abc"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA, pr_number="17"))
        self.admitted(ok)
        bad = run("sast-check", metadata=meta("retest-comment", "side", source="release-abc"), taskRunSpecs=None, params=prm(repo_url=REPO, revision=SHA, pr_number="17"))
        self.refused(bad, msg="protected branch")

    def test_every_template_is_admitted_in_every_event_shape_pac_can_record_for_it(self):
        found = delivered_templates(CATALOG_VARIANTS[:1])
        self.assertGreater(len(found), 15, "vacuous")
        shapes_seen = 0
        for path, d in found:
            base = os.path.basename(path)
            if base.startswith("pull-request-") and base != "pull-request-merged.yaml":
                shapes = [("pull_request", "main", "feature-x"), ("retest-comment", "main", "feature-x"), ("pull_request_labeled", "main", "feature-x")]
                refused = [("pull_request", "side", "feature-x"), ("retest-comment", "side", "feature-x")]
            else:
                # a push (full ref) and a re-run of its check from the git host (bare head_branch)
                shapes = [("push", "main", "refs/heads/main"), ("push", "main", "main")]
                refused = [("push", "side", "refs/heads/side"), ("push", "side", "side"), ("pull_request", "side", "main")]
            for event, branch, source in shapes:
                dd = copy.deepcopy(d)
                dd["metadata"] = dict(meta(event, branch, source=source), generateName=d["metadata"].get("generateName", "x-"))
                self.admitted(dd)
                shapes_seen += 1
            for event, branch, source in refused:
                dd = copy.deepcopy(d)
                dd["metadata"] = dict(meta(event, branch, source=source), generateName=d["metadata"].get("generateName", "x-"))
                # the PR build is the one Pipeline that is not held to a branch
                if template_pipeline(d) in self.values["pipelines"] and self.values["pipelines"][template_pipeline(d)].get("anyBranch"):
                    continue
                self.refused(dd, msg="")
                shapes_seen += 1
        self.assertGreater(shapes_seen, 60, "vacuous")

    def test_the_recorded_annotation_format_is_pinned(self):
        # captured from a live push to main and a live pull request (values replaced); if Pipelines-as-Code changes how it
        # spells these, this fails here and not as a refused release
        with open(os.path.join(os.path.dirname(__file__), "fixtures", "pac-annotations.json")) as f:
            fx = json.load(f)
        for name in ("push_main", "pull_request_main"):
            md = {"generateName": "x-", "annotations": fx[name]["annotations"], "labels": fx[name]["labels"]}
            event = fx[name]["annotations"]["pipelinesascode.tekton.dev/event-type"]
            pipeline, params = ("build", list(PR_BUILD_PARAMS)) if event == "pull_request" else ("deploy", list(BUILD_PARAMS) + prm(env="dev"))
            params = [dict(p, value=fx[name]["annotations"]["pipelinesascode.tekton.dev/repo-url"]) if p["name"] == "git-url" else
                      dict(p, value=fx[name]["annotations"]["pipelinesascode.tekton.dev/sha"]) if p["name"] == "git-revision" else p for p in params]
            if pipeline == "build":
                self.admitted(run(pipeline, metadata=md, params=params))
            else:
                self.admitted(run(pipeline, metadata=md, taskRunSpecs=None, params=params))

    def test_the_image_repository_is_the_apps_own(self):
        push = meta("push", "main")
        for bad in ("registry.example/o/other-app", "registry.example/victim/sample", "registry.example/o/sample-buildcache",
                    "registry.example/o/sample/sub", "registry.example/o/base-image", "evil.example/o/sample-pr/x"):
            params = [p if p["name"] != "image-repo" else {"name": "image-repo", "value": bad} for p in BUILD_PARAMS]
            self.refused(run(metadata=push, params=params), msg="repository")
        for good in ("registry.example/o/sample", "registry.example/o/sample-pr", "ghcr.io:5000/o/sample"):
            params = [p if p["name"] != "image-repo" else {"name": "image-repo", "value": good} for p in BUILD_PARAMS]
            self.admitted(run(metadata=push, params=params))

    def test_the_image_repository_prefix_can_be_configured(self):
        pols, _ = policies("--set", "cicdRunAdmission.imageRepoPrefixes={registry.example/ground-ns/}")
        shape = pols["cicd-run-shape"]
        for repo, ok in (("registry.example/ground-ns/sample", True), ("registry.example/ground-ns/sample-pr", True),
                         ("registry.example/o/sample", False), ("registry.example/ground-ns/other", False)):
            params = [p if p["name"] != "image-repo" else {"name": "image-repo", "value": repo} for p in BUILD_PARAMS]
            out = evaluate(shape, run(metadata=meta("push", "main"), params=params))
            self.assertEqual(out == [], ok, (repo, out))

    def test_onboarding_resync_targets_only_this_apps_gitops_repository(self):
        good = prm(git_url=REPO, app_namespace=NS, app_name="sample", app_type="app", github_owner="o",
                   gitops_repo_url="https://github.com/o/gitops-sample")
        self.admitted(run("onboarding-resync", metadata=meta("push", "main"), taskRunSpecs=None, params=good))
        for field, bad in (("gitops-repo-url", "https://github.com/victim/gitops-sample"),
                           ("gitops-repo-url", "https://github.com/o/gitops-other"),
                           ("gitops-repo-url", "https://github.com/o/sample"),
                           ("gitops-repo-url", "https://evil.example/o/gitops-sample"),
                           ("github-owner", "victim")):
            params = [p if p["name"] != field else {"name": field, "value": bad} for p in good]
            self.refused(run("onboarding-resync", metadata=meta("push", "main"), taskRunSpecs=None, params=params), msg="own")
        # nothing in a pull request needs it (it runs when cicd.yaml changes on main), so a pull request cannot start it
        self.refused(run("onboarding-resync", metadata=meta("pull_request", "main"), taskRunSpecs=None, params=good), msg="push event")

    def test_a_service_account_is_allowed_only_on_the_tasks_that_need_it(self):
        deployer = [{"pipelineTaskName": "deploy-aws-lambda", "serviceAccountName": "glidepath-cloud-deployer"},
                    {"pipelineTaskName": "deploy-aws-ecs", "serviceAccountName": "glidepath-cloud-deployer"}]
        deploy_md = meta("push", "main")
        self.admitted(run("deploy", metadata=deploy_md, taskRunSpecs=deployer, params=list(BUILD_PARAMS) + prm(env="dev")))
        # the PR build cannot attach the cloud deployer, to any task
        for task in ("build-source", "build-image", "unit-test", "deploy-aws-lambda"):
            specs = [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS if t != task]
            specs.append({"pipelineTaskName": task, "serviceAccountName": "glidepath-cloud-deployer"})
            self.refused(run("build", taskRunSpecs=specs), msg="may run as")
        # source-runner only on the source tasks of its Pipeline
        self.refused(run("deploy", metadata=deploy_md, taskRunSpecs=[{"pipelineTaskName": "debrief", "serviceAccountName": "source-runner"}],
                         params=list(BUILD_PARAMS) + prm(env="dev")), msg="may run as")
        self.refused(run("build", taskRunSpecs=[{"pipelineTaskName": "preflight", "serviceAccountName": "source-runner"}] +
                         [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS]), msg="may run as")
        # the cloud deployer on a task of another Pipeline
        self.refused(run("release", metadata=deploy_md, taskRunSpecs=deployer, params=list(BUILD_PARAMS) + prm(env="dev")), msg="may run as")

    def test_the_task_accounts_are_what_the_catalog_and_the_generator_use(self):
        docs = render_catalog()
        pipelines = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Pipeline"}
        for pname, tasks in self.values["cloudDeployerTasks"].items():
            names = {t["name"] for t in pipelines[pname]["spec"]["tasks"]}
            self.assertLessEqual(set(tasks), names)
        _, gen = generator_params()
        for t in self.values["cloudDeployerTasks"]["deploy"]:
            self.assertIn("pipelineTaskName: " + t, gen)

    def test_the_isolation_message_names_every_task_in_the_values(self):
        msg = self.iso["validations"][0]["message"]
        for pipeline, tasks in self.values["sourceTasks"].items():
            for t in tasks:
                self.assertIn(t, msg, "message omits %s (%s)" % (t, pipeline))

    def test_a_file_cannot_set_the_chain_origin(self):
        # glidepath.io/origin-* are set by the Triggers on a chained run and read by verify-ref-protection: a run a .tekton file
        # creates may carry no glidepath.io/ annotation at all.
        # Promotion lineage (glidepath-api stamps these on a promote or rollback run; the release record and the trace join
        # trust them): a file can no more claim another flow's lineage than another commit's origin.
        for key in ("glidepath.io/origin-ref", "glidepath.io/origin-sha", "glidepath.io/anything",
                    "glidepath.io/origin-chain-id", "glidepath.io/origin-chain-slug", "glidepath.io/origin-trace-id",
                    "glidepath.io/origin-traceparent", "glidepath.io/origin-finished-at"):
            md = meta("push", "main")
            md["annotations"] = dict(md["annotations"], **{key: "refs/heads/main"})
            self.refused(run("release", metadata=md, taskRunSpecs=None), msg="glidepath.io")
        self.admitted(run("release", metadata=meta("push", "main"), taskRunSpecs=None))

    # ---- source isolation ----
    def test_source_isolation_for_every_pipeline_that_runs_repository_code(self):
        pusher = [{"pipelineTaskName": t, "serviceAccountName": "image-pusher"} for t in PUSHER_TASKS]
        for specs in (None, [], [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS[:3]] + pusher,
                      [{"pipelineTaskName": t, "serviceAccountName": "pipeline-runner"} for t in SOURCE_TASKS] + pusher):
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

    def test_the_gate_pipelines_must_run_their_render_as_source_runner(self):
        """policy-validation-check and clearance-gate-check render a chart the repository chooses in their `render` task, which holds
        no credential only as source-runner: a .tekton file that gives it to anyone else (or to nobody) is refused, and so is giving
        source-runner to the gate's own tasks."""
        for pname in ("policy-validation-check", "clearance-gate-check", "values-check"):
            params = prm(repo_url=REPO, revision=SHA, app_name="sample", pr_number="17")
            for specs in (None, []):
                self.refused(run(pname, taskRunSpecs=specs, params=params), self.iso, "render")
            for account in ("pipeline-runner", "image-pusher", "glidepath-cloud-deployer"):
                specs = [{"pipelineTaskName": "render", "serviceAccountName": account}]
                self.refused(run(pname, taskRunSpecs=specs, params=params), None, "may run as")
                self.refused(run(pname, taskRunSpecs=specs, params=params), self.iso, "render")
            for task in ("fetch", "evaluate", "register", "register-advisory", "render-check", "validate"):
                specs = [{"pipelineTaskName": "render", "serviceAccountName": "source-runner"}, {"pipelineTaskName": task, "serviceAccountName": "source-runner"}]
                self.refused(run(pname, taskRunSpecs=specs, params=params), None, "may run as")
            good = run(pname, taskRunSpecs=[{"pipelineTaskName": "render", "serviceAccountName": "source-runner"}], params=params)
            self.admitted(good)
            self.admitted(good, self.iso)

    def test_the_push_task_must_run_as_image_pusher(self):
        sources = [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS]
        for specs in (sources, sources + [{"pipelineTaskName": "push-image", "serviceAccountName": "pipeline-runner"}],
                      sources + [{"pipelineTaskName": "push-image", "serviceAccountName": "source-runner"}]):
            self.refused(run(taskRunSpecs=specs), self.iso, "image-pusher")
        self.admitted(run(), self.iso)

    def test_image_pusher_is_allowed_only_on_the_push_task_of_the_build_pipeline(self):
        sources = [{"pipelineTaskName": t, "serviceAccountName": "source-runner"} for t in SOURCE_TASKS]
        self.admitted(run(taskRunSpecs=sources + [{"pipelineTaskName": "push-image", "serviceAccountName": "image-pusher"}]))
        for task in ("build-image", "build-source", "unit-test", "preflight", "generate-sbom", "debrief"):
            specs = [s for s in sources if s["pipelineTaskName"] != task] + [{"pipelineTaskName": task, "serviceAccountName": "image-pusher"}]
            self.refused(run(taskRunSpecs=specs + [{"pipelineTaskName": "push-image", "serviceAccountName": "image-pusher"}]), msg="may run as")
        # source-runner (repository code) may not be given the push task, nor the pusher to another Pipeline's task
        self.refused(run(taskRunSpecs=sources + [{"pipelineTaskName": "push-image", "serviceAccountName": "source-runner"}]), msg="may run as")
        self.refused(run("deploy", metadata=meta("push", "main"), params=list(BUILD_PARAMS) + prm(env="dev"),
                         taskRunSpecs=[{"pipelineTaskName": "push-image", "serviceAccountName": "image-pusher"}]), msg="may run as")

    def test_a_pull_request_build_must_say_so_and_use_the_pr_repository(self):
        # the cache a build may write follows pr-tag-only; a pull request may not claim to be a main build, nor a main build's repository
        self.admitted(run(metadata=meta("pull_request", "main"), params=PR_BUILD_PARAMS))
        self.refused(run(metadata=meta("pull_request", "main"), params=BUILD_PARAMS), msg="pr-tag-only=true")
        self.refused(run(metadata=meta("pull_request", "main"), params=with_params(BUILD_PARAMS, pr_tag_only="true")), msg="-pr")
        self.refused(run(metadata=meta("pull_request", "main"), params=with_params(PR_BUILD_PARAMS, pr_tag_only="false")), msg="pr-tag-only=true")
        # an event that is neither push nor pull_request (a comment re-run) is held the same way
        self.refused(run(metadata=meta("incoming", "main"), params=BUILD_PARAMS), msg="pr-tag-only=true")
        # a push is a main build
        self.admitted(run(metadata=meta("push", "main"), params=BUILD_PARAMS))
        self.refused(run(metadata=meta("push", "main"), params=with_params(BUILD_PARAMS, pr_tag_only="true")), msg="-pr")
        self.admitted(run(metadata=meta("push", "main"), params=with_params(BUILD_PARAMS, image_repo="registry.example/o/sample-pr", pr_tag_only="true")))

    def test_the_pr_repository_is_exactly_the_apps_own_pr_repository(self):
        # an application whose own repository name ends in -pr: its main repository must not satisfy the pull request rule
        app = "sample-pr"
        ns = "app-sample-pr-cicd"
        base = with_params(BUILD_PARAMS, app_name=app, app_namespace=ns)
        md = meta("pull_request", "main")
        good = with_params(base, pr_tag_only="true", image_repo="registry.example/o/sample-pr-pr")
        main = with_params(base, pr_tag_only="true", image_repo="registry.example/o/sample-pr")
        self.admitted(run(metadata=md, params=good), namespace=ns)
        self.refused(run(metadata=md, params=main), msg="-pr image repository", namespace=ns)
        # and for an ordinary application another application's -pr repository is not its own
        other = with_params(BUILD_PARAMS, pr_tag_only="true", image_repo="registry.example/o/other-pr")
        self.refused(run(metadata=md, params=other), msg="repository")

    def test_pusher_tasks_are_derived_from_the_catalog(self):
        docs = render_catalog()
        tasks = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Task"}
        pipelines = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Pipeline"}
        for pname, listed in self.values["pusherTasks"].items():
            self.assertTrue(listed)
            by_name = {t["name"]: t for t in pipelines[pname]["spec"]["tasks"]}
            for t in listed:
                ref_name = [p["value"] for p in by_name[t]["taskRef"]["params"] if p["name"] == "name"][0]
                task = tasks[ref_name]["spec"]
                # holds the push credential, runs nothing of the repository's, reads the workspace read-only
                self.assertIn("registry-credentials", yaml.safe_dump(task["volumes"]))
                self.assertFalse(repo_code(tasks[ref_name]), t)
                self.assertTrue([w for w in task["workspaces"] if w.get("readOnly")], t)
            self.assertFalse(set(listed) & set(self.values["sourceTasks"].get(pname, [])), "a task cannot be both")
        # the generator hands exactly those tasks to image-pusher
        _, gen = generator_params()
        self.assertEqual(re.findall(r"pipelineTaskName: (\S+)\s+serviceAccountName: image-pusher", gen), self.values["pusherTasks"]["build"])

    # ---- pipeline trust T4, step 2: the three split task accounts (specs/plans/pipeline-trust-t4-split-runner.md) ----
    SPLIT = {"release-record-writer": "recordWriterTasks", "clearance-gate": "clearanceTasks", "test-runner": "testRunnerTasks"}
    # the Tasks (by catalog name) each account is for, in the plan's Target table
    SPLIT_CATALOG_TASKS = {"release-record-writer": {"open-release-pr", "mark-release-merged", "itsm-change", "itsm-gate", "join-release", "record-gate-verdict"},
                           "clearance-gate": {"clearance-gate", "agent-gates", "agent-definitions", "agent-release"},
                           "test-runner": {"run-testworkflow"}}

    def may_run_as(self, pipeline, task, account):
        """True when the shape policy has no 'may run as' objection to this per-task account (other messages are not this question)."""
        specs = [{"pipelineTaskName": task, "serviceAccountName": account}]
        out = evaluate(self.shape, run(pipeline, metadata=meta("push", "main"), taskRunSpecs=specs, params=list(BUILD_PARAMS) + prm(env="dev")))
        return not any("may run as" in m for m in out)

    def test_split_accounts_are_allowed_exactly_on_their_listed_tasks(self):
        for account, value in self.SPLIT.items():
            listed = self.values[value]
            self.assertTrue(listed, value)
            for pipeline, tasks in listed.items():
                self.assertIn(pipeline, self.values["pipelines"], "%s names %s, which a .tekton file cannot start" % (value, pipeline))
                for t in tasks:
                    self.assertTrue(self.may_run_as(pipeline, t, account), (account, pipeline, t))
                    # ... and no other account of the three takes that task, and a task of the same Pipeline that is not listed is refused
                    for other in set(self.SPLIT) - {account}:
                        self.assertFalse(self.may_run_as(pipeline, t, other) and t not in self.values[self.SPLIT[other]].get(pipeline, []),
                                         (other, pipeline, t))
                self.assertFalse(self.may_run_as(pipeline, "not-a-listed-task", account), (account, pipeline))
            # another Pipeline's task name is not allowed here
            self.assertFalse(self.may_run_as("build", "build-source", account), account)

    def test_split_accounts_are_derived_from_the_catalog(self):
        # Both catalog renderings: the clearance-gate guardrail required (the Pipeline task is `register`) and advisory
        # (`register-advisory` and `render-check`); the itsm gate likewise (`verify-change` / `verify-change-advisory`).
        renderings = [render_catalog_itsm(),
                      render_catalog_itsm("--set", "releaseGuardrailOverrides.clearance-gate.required=true",
                                          "--set", "releaseGuardrailOverrides.itsm.required=false")]
        seen = set()
        for docs in renderings:
            for d in docs:
                if d["kind"] == "Pipeline" and d["metadata"]["name"] == "clearance-gate-check":
                    seen |= {t["name"] for t in d["spec"]["tasks"]}
        self.assertLessEqual({"register", "register-advisory", "render-check"}, seen, "the two renderings must differ, or the check is vacuous")
        for account, value in self.SPLIT.items():
            want = {}
            for docs in renderings:
                pipelines = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Pipeline"}
                for pname in self.values["pipelines"]:
                    for t in pipelines[pname]["spec"].get("tasks", []) + pipelines[pname]["spec"].get("finally", []):
                        ref = [p["value"] for p in t.get("taskRef", {}).get("params", []) if p["name"] == "name"]
                        if ref and ref[0] in self.SPLIT_CATALOG_TASKS[account]:
                            want.setdefault(pname, set()).add(t["name"])
            self.assertEqual({k: sorted(v) for k, v in self.values[value].items()}, {k: sorted(v) for k, v in want.items()}, value)

    def test_every_delivered_template_maps_the_writer_and_gate_tasks_of_its_pipeline_exactly(self):
        # Pipeline trust T4, step 3: each .tekton template the platform delivers hands every record-writer, Clearance-token and test task of
        # its Pipeline to the plan's account, and nothing else to a split account; and names only tasks its Pipeline has in this catalog
        # (Tekton refuses a taskRunSpecs entry for a task the Pipeline lacks, so a gate's variant names, and a task only a provider-ful
        # catalog has, are decided by task_names). Both gate variants and with and without a provider are checked.
        account_of = {t: a for a, ts in self.SPLIT_CATALOG_TASKS.items() for t in ts}
        mapped_tasks = set()
        for name, extra in CATALOG_VARIANTS:
            docs = render_catalog_itsm(*extra) if extra else render_catalog()
            pipelines = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Pipeline"}
            found = delivered_templates([(name, extra)])
            self.assertGreater(len(found), 15, name)
            for path, d in found:
                pname = template_pipeline(d)
                tasks = {}
                for t in pipelines[pname]["spec"].get("tasks", []) + pipelines[pname]["spec"].get("finally", []):
                    ref = [p["value"] for p in t.get("taskRef", {}).get("params", []) if p["name"] == "name"]
                    tasks[t["name"]] = ref[0] if ref else None
                want = {k: account_of[v] for k, v in tasks.items() if v in account_of}
                got = {e["pipelineTaskName"]: e["serviceAccountName"] for e in d["spec"].get("taskRunSpecs", []) if e["serviceAccountName"] in self.SPLIT}
                self.assertEqual(got, want, "%s (%s): %s" % (os.path.basename(path), name, pname))
                for e in d["spec"].get("taskRunSpecs", []):
                    self.assertIn(e["pipelineTaskName"], tasks, "%s (%s): names a task %s does not have" % (os.path.basename(path), name, pname))
                # the run-wide account is still pipeline-runner, and a glidepath-api rerun copies the entries unchanged (T5)
                self.assertEqual(d["spec"]["taskRunTemplate"]["serviceAccountName"], "pipeline-runner")
                for task, account in got.items():
                    self.assertIn(task, self.values[self.SPLIT[account]].get(pname, []), (path, pname, task, account))
                    mapped_tasks.add((pname, task, account))
        # nothing the templates map is missing from the admission lists, and the lists name nothing no template maps (each is a PaC-started Pipeline task)
        # (the release and test Pipelines are started by a generated flow file, not a template: its generator maps open-release-pr and run-testworkflow)
        _, gen = generator_params()
        generated = set()
        for stage, task, account in (("release", "open-release-pr", "release-record-writer"), ("test", "run-testworkflow", "test-runner")):
            m = re.search(r'if \[\[ "\$\{root_stage\}" == "%s" \]\]; then(.*?)\n\s+fi' % stage, gen, re.S)
            self.assertIsNotNone(m, stage)
            self.assertIn((task, account), re.findall(r"pipelineTaskName: (\S+)\s+serviceAccountName: ([^\s\"]+)", m.group(1)), stage)
            generated.add((stage, task, account))
        listed = {(p, t, a) for a, v in self.SPLIT.items() for p, ts in self.values[v].items() for t in ts}
        self.assertEqual(mapped_tasks | generated, listed)

    def test_split_accounts_do_not_change_the_run_wide_account_or_who_else_is_allowed(self):
        for account in self.SPLIT:
            self.refused(run(taskRunTemplate={"serviceAccountName": account}), msg="pipeline-runner")
        # an unlisted account stays refused, and the PR build cannot attach a split account to a source task
        self.refused(run(taskRunSpecs=[{"pipelineTaskName": "build-source", "serviceAccountName": "release-record-writer"}]), msg="may run as")
        # defaults must not refuse: every binding is still Deny only through its own value, and no new binding or policy appeared
        self.assertEqual(sorted(n for n in self.binds if n.startswith("cicd-run-")), ["cicd-run-no-taskruns", "cicd-run-shape", "cicd-run-source-isolation"])

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
