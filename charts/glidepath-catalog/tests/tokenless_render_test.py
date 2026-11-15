#!/usr/bin/env python3
"""Rendering is not done where the credentials are (docs/admin/security.md, "Rendering is not done where the credentials are").

`helm template` of a chart the repository chooses used to run in policy-validation-gate and clearance-gate, whose pods hold a
Kubernetes token (and, in clearance-gate, a projected token Clearance trusts). It now runs in render-manifests as source-runner, and the
gates read what it wrote as data. This file checks:

  structure   the render Task holds nothing; the gates no longer render; every Pipeline that renders gives the render to source-runner
              in the control plane's admission values and in the onboarding templates; no two tasks touch the run's volume at once; no
              credentialed task of any Pipeline invokes helm template / kustomize build (one known follow-up is named, and the test
              fails if it is not still true);
  render      the real render script with real helm: hardened flags, bounded output, a symlink or `.Files.Get` cannot put a file
              of the workspace into the output, failure statuses;
  ingest      the real ingest step of both gates against hostile trees: symlinks, a changed or extra file, a missing manifest,
              tags, an alias bomb, a recursive anchor, depth, document count, names with shell metacharacters and newlines;
  digest      the tree digest Clearance registers is byte-identical to what the gate produced BEFORE this change, for the same chart
              and values (fixtures/render/golden.json, made by the old code at main 3a7e099).

Needs helm, pyyaml, bash, jq.   Run:  python3 charts/glidepath-catalog/tests/tokenless_render_test.py
"""
import json
import os
import re
import subprocess
import tempfile
import time
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
FX = os.path.join(CATALOG, "tests", "fixtures", "render")
CP_VALUES = os.path.join(ROOT, "charts", "glidepath-control-plane", "values.yaml")
TEMPLATES = os.path.join(ROOT, "charts", "glidepath-app", "files", "onboarding-templates", "gitops-repo")

r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")], capture_output=True, text=True)
assert r.returncode == 0, r.stderr
DOCS = [d for d in yaml.safe_load_all(r.stdout) if d]
TASKS = {d["metadata"]["name"]: d for d in DOCS if d["kind"] == "Task"}
PIPELINES = {d["metadata"]["name"]: d for d in DOCS if d["kind"] == "Pipeline"}
CP = yaml.safe_load(open(CP_VALUES))["cicdRunAdmission"]
GATE_PIPELINES = {"policy-validation-check": "policy-validation-gate", "clearance-gate-check": "clearance-gate", "values-check": "validate-values"}
# a command in command position (line start, or after ; & | ( ! exec then do if while), not the words in a message or a table
HAZARD = re.compile(r"(?:^|[;&|(!]|\bexec|\bthen|\bdo|\bif|\bwhile)[ \t]*!?[ \t]*"
                    r"(?:helm[ \t]+(?:template|lint|dependency|pull|install|upgrade|show)|kustomize[ \t]+build|kubectl[ \t]+(?:kustomize|apply[ \t]+-k|diff)|jsonnet)\b", re.M)
# Credentialed tasks that still render repository content. A follow-up, named here so it cannot grow unseen (and so this test fails
# when it is fixed and the entry is left behind).
KNOWN_FOLLOW_UP = set()


def steps(task):
    return TASKS[task]["spec"]["steps"]


def code(text):
    """A script without its comment lines: what a step would run."""
    return "\n".join(l for l in text.split("\n") if not l.strip().startswith("#"))


def script(task, step):
    return [s for s in steps(task) if s["name"] == step][0]["script"]


def task_of(pipeline_task):
    ref = pipeline_task["taskRef"]
    return ref["name"] if "name" in ref else [p["value"] for p in ref["params"] if p["name"] == "name"][0]


def pipeline_tasks(name):
    return PIPELINES[name]["spec"]["tasks"] + PIPELINES[name]["spec"].get("finally", [])


def literal_env(task, step):
    """The step's environment as the pod would have it, for what a test can know: literals, plus the phase/run a test chooses."""
    env = {}
    for e in TASKS[task]["spec"].get("stepTemplate", {}).get("env", []) + [s for s in steps(task) if s["name"] == step][0].get("env", []):
        if "value" in e and "$(" not in e["value"]:
            env[e["name"]] = e["value"]
    return env


PATHS = re.compile(r"/(work|rendered|trusted|publish|out)\b")


def localize(text, base):
    """The script with the volume paths it hardcodes moved under `base` (one pass, so a path that contains /work is not rewritten twice)."""
    text = PATHS.sub(lambda m: os.path.join(base, m.group(1)), text)
    return text.replace("$(results.outcome.path)", base + "/outcome").replace("$(results.findings-summary.path)", base + "/summary") \
               .replace("/opt/airframe/charts/airframe-application", os.path.join(FX, "chart")).replace("/opt/airframe/xrds", base + "/xrds")


def run_script(text, base, env, timeout=120):
    path = os.path.join(base, "step.sh")
    with open(path, "w") as f:
        f.write(localize(text, base))
    e = dict(os.environ, **env)
    t = time.time()
    p = subprocess.run(["bash", path], cwd=base, env=e, capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout + p.stderr, time.time() - t


# ---------------------------------------------------------------------------------------------------------------- structure
class Structure(unittest.TestCase):
    def test_the_render_task_holds_nothing(self):
        spec = TASKS["render-manifests"]["spec"]
        self.assertEqual(TASKS["render-manifests"]["metadata"]["labels"]["glidepath.io/renders-repository-content"], "true")
        self.assertNotIn("volumes", spec, "no Secret, no projected token, no volume but the two workspaces")
        text = code("\n".join(s.get("script", "") for s in spec["steps"])) + yaml.safe_dump([{k: v for k, v in s.items() if k != "script"} for s in spec["steps"]])
        for banned in ("serviceAccountToken", "secretName", "secret:", "projected", "kubectl", "serviceAccountName", "curl ", "wget ", "oras ", "git "):
            self.assertNotIn(banned, text, banned)
        self.assertNotIn("KUBECONFIG", text.replace("unset KUBECONFIG", ""))
        self.assertEqual({w["name"]: w.get("readOnly", False) for w in spec["workspaces"]}, {"inputs": True, "output": False})
        for s in spec["steps"]:
            self.assertFalse([m for m in s.get("volumeMounts", [])], "no volume mounted but the workspaces")

    def test_the_render_script_is_hardened(self):
        text = code(script("render-manifests", "render"))
        for banned in ("--post-renderer", "--set-file", "--dependency-update", "--dry-run=server", "--validate", "--skip-tests", r"helm\s+plugin", r"helm\s+dependency", r"helm\s+repo\b", r"helm\s+pull"):
            self.assertIsNone(re.search(banned, text), banned)
        self.assertIn("HELM_PLUGINS=", text)
        self.assertIn("ulimit -f", text)
        self.assertIn("ulimit -t", text)
        self.assertIn("# BEGIN chart-safety", script("render-manifests", "render"))
        self.assertEqual(len(re.findall(r"exec helm template", text)), 1, "one place renders")

    def test_the_gates_no_longer_render_and_read_the_output_read_only(self):
        for name in GATE_PIPELINES.values():
            text = code("\n".join(s.get("script", "") for s in steps(name)))
            self.assertIsNone(HAZARD.search(text), name)
            self.assertNotIn("chart_safe", text, name)
            ws = {w["name"]: w for w in TASKS[name]["spec"]["workspaces"]}
            self.assertEqual(set(ws), {"publish", "trusted", "rendered"})
            self.assertTrue(all(w.get("optional") for w in ws.values()))
            self.assertTrue(ws["trusted"]["readOnly"] and ws["rendered"]["readOnly"])
            self.assertFalse(ws["publish"].get("readOnly"))
            # the only step that writes /publish is publish, and it does nothing in the decide phase
            for s in steps(name):
                if s["name"] != "publish":
                    self.assertNotIn("/publish", s.get("script", ""), (name, s["name"]))
            self.assertIn('[[ "${PHASE}" == fetch ]] || exit 0', steps(name)[0]["script"])
            self.assertIn('[[ "${PHASE}" == fetch ]] || exit 0', script(name, "publish"))

    def test_decide_tasks_bind_only_read_only_workspaces_and_render_gets_the_account_free_split(self):
        for pname, gate in GATE_PIPELINES.items():
            tasks = {t["name"]: t for t in pipeline_tasks(pname)}
            by_task = {}
            for t in pipeline_tasks(pname):
                by_task.setdefault(task_of(t), []).append(t)
            self.assertEqual([t["name"] for t in by_task["render-manifests"]], ["render"])
            render = tasks["render"]
            self.assertEqual({w["name"]: (w["workspace"], w.get("subPath")) for w in render["workspaces"]},
                             {"inputs": ("gate", "inputs"), "output": ("gate", "rendered")})
            for t in by_task[gate]:
                phase = [p["value"] for p in t["params"] if p["name"] == "phase"][0]
                bound = {w["name"]: w.get("subPath") for w in t["workspaces"]}
                if phase == "fetch":
                    self.assertEqual(bound, {"publish": None}, t["name"])
                else:
                    self.assertEqual(phase, "decide")
                    self.assertEqual(bound, {"trusted": "trusted", "rendered": "rendered"}, t["name"])

    def test_nothing_touches_the_volume_beside_another_task(self):
        # Every pair of tasks that bind the run's volume is ordered by runAfter: the gate reads what render wrote after it has
        # finished, and nothing else writes the volume in between (the finally tasks bind nothing and run after all of them).
        for pname in GATE_PIPELINES:
            tasks = {t["name"]: t for t in PIPELINES[pname]["spec"]["tasks"]}
            after = {n: set(t.get("runAfter", [])) for n, t in tasks.items()}

            def ancestors(n, seen=None):
                seen = seen if seen is not None else set()
                for a in after[n]:
                    if a not in seen:
                        seen.add(a)
                        ancestors(a, seen)
                return seen
            binders = [n for n, t in tasks.items() if any(w.get("workspace") == "gate" for w in t.get("workspaces", []))]
            self.assertGreaterEqual(len(binders), 3, pname)
            for a in binders:
                for b in binders:
                    if a < b:
                        self.assertTrue(a in ancestors(b) or b in ancestors(a), (pname, a, b, "run at the same time on one volume"))
            for t in PIPELINES[pname]["spec"].get("finally", []):
                self.assertFalse([w for w in t.get("workspaces", [])], (pname, t["name"], "a finally task binds the volume"))

    def test_the_admission_values_and_the_templates_give_the_render_to_source_runner(self):
        for pname in GATE_PIPELINES:
            self.assertEqual(CP["sourceTasks"][pname], ["render"])
        for fname, pname in (("pull-request-policy-validation.yaml", "policy-validation-check"), ("pull-request-clearance-gate.yaml", "clearance-gate-check")):
            run = yaml.safe_load(open(os.path.join(TEMPLATES, fname)))
            self.assertEqual(run["spec"]["taskRunSpecs"], [{"pipelineTaskName": "render", "serviceAccountName": "source-runner"}], fname)
            self.assertEqual(run["spec"]["taskRunTemplate"]["serviceAccountName"], "pipeline-runner")
            ws = run["spec"]["workspaces"]
            self.assertEqual([w["name"] for w in ws], ["gate"])
            self.assertIn("volumeClaimTemplate", ws[0])
            self.assertIn(run["spec"]["pipelineRef"]["params"][1]["value"], PIPELINES)

    def test_no_credentialed_task_renders_repository_content(self):
        """Every task of every Pipeline that invokes helm template, kustomize build and the like must run as source-runner
        (listed in the control plane's sourceTasks for that Pipeline) and mount nothing. The exceptions are named."""
        found = set()
        for pname, p in PIPELINES.items():
            for t in pipeline_tasks(pname):
                name = task_of(t)
                text = code("\n".join(s.get("script", "") for s in steps(name)))
                if not HAZARD.search(text):
                    continue
                found.add(name)
                if name in KNOWN_FOLLOW_UP:
                    continue
                self.assertIn(t["name"], CP["sourceTasks"].get(pname, []), "%s renders in %s as a credentialed task" % (name, pname))
                spec = TASKS[name]["spec"]
                self.assertFalse(spec.get("volumes"), name)
                self.assertTrue(TASKS[name]["metadata"]["labels"].get("glidepath.io/renders-repository-content"), name)
        self.assertIn("render-manifests", found, "the check found nothing: it would be vacuous")
        self.assertEqual(found & KNOWN_FOLLOW_UP, KNOWN_FOLLOW_UP, "a follow-up is done: remove it from KNOWN_FOLLOW_UP")
        # the converse: nothing that is credentialed is listed as a source task
        for pname, listed in CP["sourceTasks"].items():
            for t in pipeline_tasks(pname):
                if t["name"] in listed and task_of(t) == "render-manifests":
                    self.assertFalse(TASKS["render-manifests"]["spec"].get("volumes"))

    def test_values_check_no_longer_renders_in_its_own_pod(self):
        # the follow-up of the first version of this slice: validate-values' own-chart step ran helm template as pipeline-runner
        text = code("\n".join(s.get("script", "") for s in steps("validate-values")))
        self.assertIsNone(HAZARD.search(text))
        self.assertEqual(CP["sourceTasks"]["values-check"], ["render"])
        self.assertIn("render-manifests", [task_of(t) for t in pipeline_tasks("values-check")])


# ---------------------------------------------------------------------------------------------------------------- the render
def lay_out_inputs(base, plan, head_values="replicas: 2\n", base_values="replicas: 1\n", observability=None, with_base=True, published=True):
    w = os.path.join(base, "work")
    for tree in ("repo", "base") if with_base else ("repo",):
        d = os.path.join(w, tree, "hangar-prod", "prod")
        os.makedirs(d)
        open(os.path.join(d, "values.yaml"), "w").write(head_values if tree == "repo" else base_values)
        open(os.path.join(d, "release.yaml"), "w").write('image: {tag: "2.1.0"}\n')
    os.makedirs(os.path.join(w, "observability"))
    open(os.path.join(w, "observability", "hangar-prod.yaml"), "w").write(observability or "")
    open(os.path.join(w, "plan.txt"), "w").write(plan)
    if published:
        open(os.path.join(w, "PUBLISHED"), "w").write("run-1\n")
    os.makedirs(os.path.join(base, "out"))
    return w


def render_env(profile, **extra):
    env = {"PROFILE": profile, "APP_NAME": "x", "NAMESPACE": "app-x-cicd", "RENDER_CPU_SECONDS": "60", "RENDER_MAX_FILE_KB": "32768",
           "RENDER_MAX_ENVIRONMENTS": "50", "PIPELINE_RUN": "run-1", "RENDER_TMP": tempfile.mkdtemp(prefix="render-tmp-")}
    env.update(extra)
    return env


def read_manifest(base):
    return open(os.path.join(base, "out", "MANIFEST")).read()


class Render(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.base = self.tmp.name

    def tearDown(self):
        self.tmp.cleanup()

    def run_render(self, profile, plan, **kw):
        lay_out_inputs(self.base, plan, **{k: v for k, v in kw.items() if k in ("head_values", "base_values", "observability", "with_base")})
        env = render_env(profile, **{k: v for k, v in kw.items() if k.startswith("RENDER_")})
        return run_script(script("render-manifests", "render"), self.base, env)

    def out(self, *p):
        return os.path.join(self.base, "out", *p)

    def test_clearance_profile_writes_head_base_status_and_a_manifest_of_every_file(self):
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|default|true\n")
        self.assertEqual(rc, 0, log)
        d = "hangar-prod__prod"
        self.assertEqual(sorted(os.listdir(self.out(d))), ["base.yaml", "head.yaml", "status"])
        self.assertEqual(open(self.out(d, "status")).read().strip(), "ok")
        self.assertIn("replicas: 2", open(self.out(d, "head.yaml")).read())
        self.assertIn("replicas: 1", open(self.out(d, "base.yaml")).read())
        import hashlib
        want = sorted("%s  %s/%s" % (hashlib.sha256(open(self.out(d, f), "rb").read()).hexdigest(), d, f) for f in os.listdir(self.out(d)))
        self.assertEqual(sorted(read_manifest(self.base).strip().split("\n")), want)
        self.assertEqual(sorted(os.listdir(self.out())), ["MANIFEST", d], "nothing but the environment directories and the manifest")

    def test_policy_validation_profile_writes_resources_with_the_apps_release_name(self):
        rc, log, _ = self.run_render("policy-validation", "hangar-prod|prod|default\n")
        self.assertEqual(rc, 0, log)
        self.assertEqual(sorted(os.listdir(self.out("hangar-prod__prod"))), ["resources.yaml", "status"])
        self.assertIn('release: "x"', open(self.out("hangar-prod__prod", "resources.yaml")).read())

    def test_clearance_release_name_is_app_dash_env(self):
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|default|true\n")
        self.assertEqual(rc, 0, log)
        self.assertIn('release: "x-prod"', open(self.out("hangar-prod__prod", "head.yaml")).read())

    def test_the_render_refuses_inputs_this_runs_fetch_did_not_publish(self):
        # an advisory Pipeline goes on after a failed fetch: nothing it left is rendered
        lay_out_inputs(self.base, "hangar-prod|prod|default|true\n", published=False)
        rc, log, _ = run_script(script("render-manifests", "render"), self.base, render_env("clearance-gate"))
        self.assertEqual(rc, 1, log)
        self.assertIn("not published by this run", log)
        self.assertEqual(os.listdir(self.out()), [])
        self.tearDown(); self.setUp()
        lay_out_inputs(self.base, "hangar-prod|prod|default|true\n")
        rc, log, _ = run_script(script("render-manifests", "render"), self.base, render_env("clearance-gate", PIPELINE_RUN="another-run"))
        self.assertEqual(rc, 1, log)
        self.assertEqual(os.listdir(self.out()), [])

    def test_values_profile_renders_with_the_apps_release_name_and_says_whether_the_app_is_released(self):
        rc, log, _ = self.run_render("values", "hangar-prod|prod|default\n")
        self.assertEqual(rc, 0, log)
        self.assertEqual(sorted(os.listdir(self.out("hangar-prod__prod"))), ["released", "resources.yaml", "status"])
        self.assertEqual(open(self.out("hangar-prod__prod", "released")).read(), "false")
        self.assertIn("namespace: app-x-prod", open(self.out("hangar-prod__prod", "resources.yaml")).read())

    def test_an_unprotected_environment_is_not_rendered(self):
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|default|false\n")
        self.assertEqual(rc, 0, log)
        self.assertEqual(os.listdir(self.out()), ["MANIFEST"])
        self.assertEqual(read_manifest(self.base), "")

    def test_a_chart_that_cannot_render_fails_the_environment_and_the_run(self):
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|/nonexistent/chart|true\n")
        self.assertEqual(rc, 1, log)
        self.assertEqual(os.listdir(self.out("hangar-prod__prod")), ["status"])
        self.assertEqual(open(self.out("hangar-prod__prod", "status")).read().strip(), "render-failed")
        self.assertIn("FAIL hangar-prod/prod", log)

    def test_a_values_file_that_is_a_symlink_is_refused(self):
        lay_out_inputs(self.base, "hangar-prod|prod|default|true\n")
        v = os.path.join(self.base, "work", "repo", "hangar-prod", "prod", "values.yaml")
        os.remove(v)
        open(os.path.join(self.base, "secret"), "w").write("SECRETTOKEN")
        os.symlink(os.path.join(self.base, "secret"), v)
        rc, log, _ = run_script(script("render-manifests", "render"), self.base, render_env("clearance-gate"))
        self.assertEqual(rc, 1, log)
        self.assertNotIn("SECRETTOKEN", log)
        self.assertNotIn("SECRETTOKEN", "".join(open(os.path.join(root, f)).read() for root, _, fs in os.walk(self.out()) for f in fs))

    def chart_with(self, template, extra_files=None):
        c = os.path.join(self.base, "chart")
        os.makedirs(os.path.join(c, "templates"))
        open(os.path.join(c, "Chart.yaml"), "w").write("apiVersion: v2\nname: hostile\nversion: 0.1.0\n")
        open(os.path.join(c, "templates", "t.yaml"), "w").write(template)
        for rel, content in (extra_files or {}).items():
            os.makedirs(os.path.dirname(os.path.join(c, rel)), exist_ok=True)
            open(os.path.join(c, rel), "w").write(content)
        return c

    def test_a_template_symlink_out_of_the_chart_is_refused_and_its_content_never_reaches_the_output(self):
        secret = os.path.join(self.base, "secret")
        open(secret, "w").write("SECRETTOKEN")
        c = self.chart_with('apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {v: {{ .Files.Get "leak.txt" | quote }}}\n')
        os.symlink(secret, os.path.join(c, "leak.txt"))
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|%s|true\n" % c)
        self.assertEqual(rc, 1, log)
        self.assertIn("symlink that leaves the chart directory", log)
        self.assertNotIn("SECRETTOKEN", log + "".join(open(os.path.join(r_, f)).read() for r_, _, fs in os.walk(self.out()) for f in fs))

    def test_files_get_of_a_path_outside_the_chart_reads_nothing(self):
        secret = os.path.join(self.base, "secret")
        open(secret, "w").write("SECRETTOKEN")
        t = ('apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata:\n  a: {{ .Files.Get "../secret" | quote }}\n  b: {{ .Files.Get "%s" | quote }}\n'
             '  c: {{ .Files.Get "/etc/passwd" | quote }}\n' % secret)
        c = self.chart_with(t)
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|%s|true\n" % c)
        out = open(self.out("hangar-prod__prod", "head.yaml")).read() if rc == 0 else ""
        self.assertNotIn("SECRETTOKEN", log + out)
        self.assertNotIn("root:", out, "/etc/passwd must not be readable through .Files")

    def test_lookup_finds_nothing_because_there_is_no_cluster(self):
        t = 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {n: {{ (lookup "v1" "Namespace" "" "kube-system") | toJson | quote }}}\n'
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|%s|true\n" % self.chart_with(t))
        self.assertEqual(rc, 0, log)
        self.assertIn('n: "{}"', open(self.out("hangar-prod__prod", "head.yaml")).read())

    def test_a_huge_render_is_stopped_at_the_output_limit(self):
        t = 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: big}\ndata:\n{{- range until 200000 }}\n  k{{ . }}: "{{ repeat 40 "x" }}"\n{{- end }}\n'
        rc, log, took = self.run_render("clearance-gate", "hangar-prod|prod|%s|true\n" % self.chart_with(t), RENDER_MAX_FILE_KB="256")
        self.assertEqual(rc, 1, log)
        self.assertEqual(open(self.out("hangar-prod__prod", "status")).read().strip(), "render-failed")
        self.assertEqual(os.listdir(self.out("hangar-prod__prod")), ["status"], "the partial output is removed")
        self.assertIn("output limit", log)
        self.assertLess(took, 60)

    def test_a_template_that_never_ends_is_stopped_by_the_cpu_limit(self):
        t = '{{ define "loop" }}{{ include "loop" . }}{{ end }}{{ include "loop" . }}\n'
        rc, log, took = self.run_render("clearance-gate", "hangar-prod|prod|%s|true\n" % self.chart_with(t), RENDER_CPU_SECONDS="5")
        self.assertEqual(rc, 1, log)
        self.assertLess(took, 60)

    def test_an_environment_name_with_a_newline_or_shell_metacharacters_is_not_rendered(self):
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod;touch PWNED|default|true\n")
        self.assertEqual(rc, 1, log)
        self.assertFalse(os.path.exists(os.path.join(self.base, "PWNED")))
        self.assertEqual(os.listdir(self.out()), ["MANIFEST"])

    def test_too_many_environments_fail_before_any_render(self):
        rc, log, _ = self.run_render("clearance-gate", "hangar-prod|prod|default|true\n" * 3, RENDER_MAX_ENVIRONMENTS="2")
        self.assertEqual(rc, 1, log)
        self.assertEqual(os.listdir(self.out()), [])

    def test_an_empty_plan_renders_nothing_and_writes_no_manifest(self):
        lay_out_inputs(self.base, "")
        rc, log, _ = run_script(script("render-manifests", "render"), self.base, render_env("clearance-gate"))
        self.assertEqual(rc, 0, log)
        self.assertEqual(os.listdir(self.out()), [])


# ---------------------------------------------------------------------------------------------------------------- the ingest
def sha(b):
    import hashlib
    return hashlib.sha256(b).hexdigest()


GOOD_HEAD = b"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: app-x-prod}\ndata: {k: v}\n"
GOOD_BASE = b"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a, namespace: app-x-prod}\ndata: {k: old}\n"


class Ingest(unittest.TestCase):
    """The ingest step of each gate, run for real: /trusted and /rendered laid out the way the Pipeline leaves them."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.base = self.tmp.name

    def tearDown(self):
        self.tmp.cleanup()

    def lay_out(self, gate, files, plan=None, manifest="auto", published="run-1"):
        b = self.base
        for d in ("trusted", "rendered", "work", "xrds"):
            os.makedirs(os.path.join(b, d), exist_ok=True)
        if published is not None:
            open(os.path.join(b, "trusted", "PUBLISHED"), "w").write(published + "\n")
        if plan is None:
            plan = "hangar-prod|prod|default|true\n" if gate == "clearance-gate" else "hangar-prod|prod|default\n"
        open(os.path.join(b, "trusted", "plan.txt"), "w").write(plan)
        json.dump({"owner": "o", "repo": "gitops-x", "pr": 7, "head": "a" * 40, "base": "b" * 40, "author": "dana",
                   "files": [{"filename": "hangar-prod/prod/release.yaml", "status": "modified", "additions": 1, "deletions": 1}],
                   "looksAgent": False, "provenance": None, "checkRuns": [], "policyCommit": ""}, open(os.path.join(b, "trusted", "meta.json"), "w"))
        os.makedirs(os.path.join(b, "trusted", "policies"))
        open(os.path.join(b, "trusted", "policies", "unreadable"), "w").close()
        lines = []
        for rel, data in files.items():
            p = os.path.join(b, "rendered", rel)
            os.makedirs(os.path.dirname(p), exist_ok=True)
            if data is None:
                continue
            if isinstance(data, tuple) and data[0] == "symlink":
                os.symlink(data[1], p)
                continue
            open(p, "wb").write(data)
            lines.append("%s  %s" % (sha(data), rel))
        if manifest == "auto":
            open(os.path.join(b, "rendered", "MANIFEST"), "w").write("".join(l + "\n" for l in lines))
        elif manifest is not None:
            open(os.path.join(b, "rendered", "MANIFEST"), "w").write(manifest)

    def ingest(self, gate, **env):
        task = "clearance-gate" if gate == "clearance-gate" else "policy-validation-gate"
        e = literal_env(task, "ingest")
        e.update(PHASE="decide", PIPELINE_RUN="run-1", APP_NAME="x", NAMESPACE="app-x-cicd", RENDER_ONLY="false")
        e.update(env)
        return run_script(script(task, "ingest"), self.base, e)

    def good(self, gate="clearance-gate"):
        d = "hangar-prod__prod/"
        if gate == "clearance-gate":
            return {d + "head.yaml": GOOD_HEAD, d + "base.yaml": GOOD_BASE, d + "status": b"ok\n"}
        return {d + "resources.yaml": GOOD_HEAD, d + "status": b"ok\n"}

    def refused(self, gate, why, **env):
        rc, log, took = self.ingest(gate, **env)
        self.assertNotEqual(rc, 0, log)
        self.assertIn(why, log)
        self.assertFalse(os.path.exists(os.path.join(self.base, "work", "eval", "hangar-prod__prod", "change.json")), "no change request from a refused tree")
        return log

    def test_a_good_tree_is_read_by_policy_validation_and_prepared_for_kyverno(self):
        self.lay_out("policy-validation", self.good("policy-validation"))
        rc, log, _ = self.ingest("policy-validation")
        self.assertEqual(rc, 0, log)
        e = os.path.join(self.base, "work", "eval", "hangar-prod__prod")
        self.assertEqual(open(os.path.join(e, "resources.yaml"), "rb").read(), GOOD_HEAD)
        self.assertTrue(os.path.exists(os.path.join(e, "crds.yaml")) and os.path.exists(os.path.join(e, "values.yaml")), "the inputs the evaluate step reads")

    def test_clearance_ingest_writes_the_change_request_from_the_bytes_it_read(self):
        self.lay_out("clearance-gate", self.good())
        rc, log, _ = self.ingest("clearance-gate")
        self.assertEqual(rc, 0, log)
        e = os.path.join(self.base, "work", "eval", "hangar-prod__prod")
        self.assertEqual(open(os.path.join(e, "head.yaml"), "rb").read(), GOOD_HEAD)
        self.assertTrue(os.path.islink(os.path.join(e, "resources.yaml")), "the admission step's input, a link inside /work/eval")
        c = json.load(open(os.path.join(e, "change.json")))
        self.assertEqual([o["op"] for o in c["objects"]], ["changed"])
        self.assertEqual(open(os.path.join(e, "env")).read(), "hangar-prod\nprod\n")

    def test_no_marker_for_this_run_is_refused(self):
        self.lay_out("clearance-gate", self.good(), published=None)
        self.refused("clearance-gate", "not on the workspace")
        self.tearDown(); self.setUp()
        self.lay_out("clearance-gate", self.good(), published="another-run")
        self.refused("clearance-gate", "not on the workspace")

    def test_the_fetch_phase_does_nothing_here_and_marks_the_rest_to_skip(self):
        self.lay_out("clearance-gate", self.good())
        os.makedirs(os.path.join(self.base, "work"), exist_ok=True)
        rc, log, _ = self.ingest("clearance-gate", PHASE="fetch")
        self.assertEqual(rc, 0, log)
        self.assertTrue(os.path.exists(os.path.join(self.base, "work", "skip-rest")))
        self.assertFalse(os.path.exists(os.path.join(self.base, "work", "eval")))

    def test_a_missing_manifest_is_refused(self):
        self.lay_out("clearance-gate", self.good(), manifest=None)
        self.refused("clearance-gate", "no MANIFEST")

    def test_a_file_that_differs_from_the_manifest_is_refused(self):
        self.lay_out("clearance-gate", self.good())
        p = os.path.join(self.base, "rendered", "hangar-prod__prod", "head.yaml")
        open(p, "ab").write(b"# changed after the manifest\n")
        self.refused("clearance-gate", "does not match its digest")

    def test_a_file_the_manifest_does_not_list_is_refused(self):
        files = self.good()
        self.lay_out("clearance-gate", files)
        open(os.path.join(self.base, "rendered", "hangar-prod__prod", "base.yaml"), "wb").write(GOOD_BASE)
        os.remove(os.path.join(self.base, "rendered", "hangar-prod__prod", "status"))
        self.refused("clearance-gate", "does not list exactly")

    def test_a_file_this_gate_does_not_read_is_refused(self):
        files = self.good()
        files["hangar-prod__prod/extra.yaml"] = b"a: 1\n"
        self.lay_out("clearance-gate", files)
        self.refused("clearance-gate", "a file this gate does not read")

    def test_a_directory_the_plan_does_not_name_is_refused(self):
        files = self.good()
        files["other__env/status"] = b"ok\n"
        self.lay_out("clearance-gate", files)
        self.refused("clearance-gate", "does not name")

    def test_a_symlink_to_the_pods_own_token_is_never_followed(self):
        token = os.path.join(self.base, "token")
        open(token, "w").write("PODTOKEN")
        files = self.good()
        files["hangar-prod__prod/head.yaml"] = ("symlink", token)
        self.lay_out("clearance-gate", files, manifest="")
        log = self.refused("clearance-gate", "not a plain file")
        self.assertNotIn("PODTOKEN", log)
        self.assertFalse([1 for r_, _, fs in os.walk(os.path.join(self.base, "work")) for f in fs if "PODTOKEN" in open(os.path.join(r_, f), errors="ignore").read()])

    def test_a_directory_symlink_and_a_pipe_are_refused(self):
        files = self.good()
        files["linked"] = ("symlink", self.base)
        self.lay_out("clearance-gate", files, manifest="")
        self.refused("clearance-gate", "not an environment's directory")
        self.tearDown(); self.setUp()
        self.lay_out("clearance-gate", self.good())
        os.remove(os.path.join(self.base, "rendered", "hangar-prod__prod", "status"))
        os.mkfifo(os.path.join(self.base, "rendered", "hangar-prod__prod", "status"))
        self.refused("clearance-gate", "not a plain file")

    def test_a_render_that_said_failed_is_a_failure_with_a_fixed_message(self):
        files = self.good()
        files["hangar-prod__prod/status"] = b"render-failed\n"
        self.lay_out("clearance-gate", files)
        log = self.refused("clearance-gate", "did not render this environment")
        self.assertNotIn("head.yaml", log.replace("head.yaml\n", ""))

    def bad_yaml(self, text, why, gate="clearance-gate", **env):
        files = self.good(gate)
        key = "hangar-prod__prod/head.yaml" if gate == "clearance-gate" else "hangar-prod__prod/resources.yaml"
        files[key] = text if isinstance(text, bytes) else text.encode()
        self.lay_out(gate, files)
        return self.refused(gate, why, **env)

    def test_a_python_tag_is_refused_and_executes_nothing(self):
        for gate in ("clearance-gate", "policy-validation"):
            self.tearDown(); self.setUp()
            self.bad_yaml("a: !!python/object/apply:os.system ['touch PWNED']\nkind: ConfigMap\nmetadata: {name: x}\n", "YAML tag other than the core ones", gate)
            self.assertFalse(os.path.exists(os.path.join(self.base, "PWNED")))
        self.tearDown(); self.setUp()
        self.bad_yaml("a: !custom {x: 1}\n", "YAML tag other than the core ones")

    def test_an_alias_bomb_is_refused_quickly(self):
        lines = ['a: &a0 ["lol","lol","lol","lol","lol","lol","lol","lol","lol"]']
        for i in range(1, 10):
            lines.append("b%d: &a%d [%s]" % (i, i, ",".join(["*a%d" % (i - 1)] * 9)))
        lines.append("kind: ConfigMap\nmetadata: {name: bomb}")
        # the anchors are at the top level of one mapping so each level refers to the one above it
        text = "x:\n" + "\n".join("  " + l for l in lines[:-1]) + "\n" + lines[-1] + "\n"
        t = time.time()
        self.bad_yaml(text, "more than")
        self.assertLess(time.time() - t, 30, "refused by counting, not by expanding")

    def test_a_recursive_anchor_is_refused(self):
        self.bad_yaml("a: &a [*a]\nkind: ConfigMap\nmetadata: {name: x}\n", "FAIL")

    def test_nesting_too_deep_is_refused(self):
        self.bad_yaml("a: " + "[" * 200 + "]" * 200 + "\nkind: ConfigMap\nmetadata: {name: x}\n", "nested more than")

    def test_too_many_documents_are_refused(self):
        self.bad_yaml("---\nkind: ConfigMap\nmetadata: {name: x}\n" * 50, "more than 20 documents", RENDER_MAX_DOCUMENTS="20")

    def test_a_file_over_the_size_limit_is_refused(self):
        self.bad_yaml("kind: ConfigMap\nmetadata: {name: x}\ndata: {k: %s}\n" % ("y" * 5000), "larger than the limit", RENDER_MAX_FILE_BYTES="1000")

    def test_invalid_yaml_and_invalid_utf8_are_refused(self):
        self.bad_yaml("a: [unclosed\n", "not valid YAML")
        self.tearDown(); self.setUp()
        self.bad_yaml(b"a: \xff\xfe\n", "not UTF-8")

    def test_a_scalar_the_parser_cannot_build_is_refused_with_a_fixed_message(self):
        # a timestamp that is not a date, a float or int that is not a number, a bad binary: PyYAML's constructors raise ValueError,
        # TypeError or binascii.Error, whose text quotes the repository's string
        for text in ("a: 2023-13-45\nkind: ConfigMap\nmetadata: {name: x}\n", "a: !!float abc\n", "a: !!int xyz\n",
                     "a: !!timestamp nope\n", "a: !!set {x: 1}\nb: !!omap [1]\n"):
            self.tearDown(); self.setUp()
            log = self.bad_yaml(text, "not valid YAML")
            for secret in ("2023-13-45", "abc", "xyz", "nope"):
                self.assertNotIn(secret, log)

    def test_documents_that_are_not_mappings_or_lack_a_kind_are_read_and_ignored_as_before(self):
        text = (b"---\n# only a comment\n---\n\n---\njust a scalar\n---\n- a\n- list\n---\nkind: NoMetadata\n---\n"
                b"apiVersion: v1\nmetadata: {name: nokind}\n---\n" + GOOD_HEAD)
        files = self.good()
        files["hangar-prod__prod/head.yaml"] = text
        self.lay_out("clearance-gate", files)
        rc, log, _ = self.ingest("clearance-gate")
        self.assertEqual(rc, 0, log)
        c = json.load(open(os.path.join(self.base, "work", "eval", "hangar-prod__prod", "change.json")))
        self.assertEqual([o["name"] for o in c["objects"]], ["a"])

    def hostile_names(self, gate):
        name = "a$(touch PWNED); `touch PWNED2` | @pr-lens\nsecond line"
        doc = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": name, "namespace": "app-x-prod"}, "data": {"k": "$(touch PWNED3)\n; rm -rf /"}}
        text = json.dumps(doc).encode()
        files = self.good(gate)
        files["hangar-prod__prod/head.yaml" if gate == "clearance-gate" else "hangar-prod__prod/resources.yaml"] = text
        self.lay_out(gate, files)
        return name

    def test_object_names_with_shell_metacharacters_and_newlines_are_data_not_code_and_never_printed(self):
        for gate in ("clearance-gate", "policy-validation"):
            self.tearDown(); self.setUp()
            name = self.hostile_names(gate)
            rc, log, _ = self.ingest(gate)
            self.assertEqual(rc, 0, log)
            for f in ("PWNED", "PWNED2", "PWNED3"):
                self.assertFalse(os.path.exists(os.path.join(self.base, f)), f)
            self.assertNotIn("touch", log)
            self.assertNotIn("pr-lens", log)
            if gate == "clearance-gate":
                c = json.load(open(os.path.join(self.base, "work", "eval", "hangar-prod__prod", "change.json")))
                self.assertEqual(sorted(o["name"] for o in c["objects"]), sorted(["a", name]), "the exact name is registered, as data (and the base's object is listed as removed)")

    def test_an_environment_name_that_is_not_a_plain_name_is_refused_for_that_environment(self):
        self.lay_out("clearance-gate", {}, plan="hangar-prod|pr$(touch PWNED)|default|true\n")
        rc, log, _ = self.ingest("clearance-gate")
        self.assertEqual(rc, 1, log)
        self.assertIn("not a plain name", log)
        self.assertFalse(os.path.exists(os.path.join(self.base, "PWNED")))
        self.assertRegex(log, r"^[A-Za-z0-9 ._/:?'()\n-]*$", "only plain characters are printed from a name")

    def test_the_observability_that_could_not_be_read_is_decided_here_from_trusted_data(self):
        self.lay_out("clearance-gate", self.good())
        os.makedirs(os.path.join(self.base, "trusted", "observability"))
        open(os.path.join(self.base, "trusted", "observability", "hangar-prod.yaml.unreadable"), "w").close()
        self.refused("clearance-gate", "observability facts could not be read")

    def test_an_unprotected_environment_is_skipped_without_reading_the_render(self):
        self.lay_out("clearance-gate", {}, plan="hangar-prod|prod|default|false\n")
        rc, log, _ = self.ingest("clearance-gate")
        self.assertEqual(rc, 0, log)
        self.assertIn("skipped", log)
        self.assertEqual(open(os.path.join(self.base, "work", "eval", "hangar-prod__prod", "status")).read().strip(), "skipped")

    def test_an_empty_plan_ends_the_step_quietly(self):
        self.lay_out("clearance-gate", {}, plan="")
        rc, log, _ = self.ingest("clearance-gate")
        self.assertEqual(rc, 0, log)
        rc, log, _ = self.ingest("clearance-gate", RENDER_ONLY="true")
        self.assertTrue(os.path.exists(os.path.join(self.base, "work", "skip-rest")))


class ValuesIngest(unittest.TestCase):
    """validate-values' own-chart step: the Namespace and Rollout checks it used to make on its own helm output, now on the render's."""

    def run_step(self, resources, released, status=b"ok\n"):
        with tempfile.TemporaryDirectory() as base:
            for d in ("trusted", "rendered", "work"):
                os.makedirs(os.path.join(base, d))
            open(os.path.join(base, "trusted", "PUBLISHED"), "w").write("run-1\n")
            open(os.path.join(base, "trusted", "plan.txt"), "w").write("hangar-prod|prod|/work/charts/1\n")
            files = {"hangar-prod__prod/resources.yaml": resources, "hangar-prod__prod/released": released, "hangar-prod__prod/status": status}
            lines = []
            for rel, data in files.items():
                os.makedirs(os.path.dirname(os.path.join(base, "rendered", rel)), exist_ok=True)
                open(os.path.join(base, "rendered", rel), "wb").write(data)
                lines.append("%s  %s" % (sha(data), rel))
            open(os.path.join(base, "rendered", "MANIFEST"), "w").write("".join(l + "\n" for l in lines))
            e = literal_env("validate-values", "own-chart")
            e.update(PHASE="decide", PIPELINE_RUN="run-1", APP_NAME="x")
            return run_script(script("validate-values", "own-chart"), base, e)

    NS = b"apiVersion: v1\nkind: Namespace\nmetadata: {name: app-x-prod}\n"
    ROLLOUT = b"---\napiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: x}\n"

    def test_a_namespace_is_required_and_a_rollout_once_released(self):
        self.assertEqual(self.run_step(self.NS, b"false")[0], 0)
        self.assertEqual(self.run_step(self.NS + self.ROLLOUT, b"true")[0], 0)
        rc, log, _ = self.run_step(self.ROLLOUT, b"true")
        self.assertEqual(rc, 1)
        self.assertIn("a Namespace", log)
        rc, log, _ = self.run_step(self.NS, b"true")
        self.assertEqual(rc, 1)
        self.assertIn("a Rollout named x", log)

    def test_a_render_that_failed_or_a_hostile_file_fails_the_step(self):
        self.assertEqual(self.run_step(self.NS, b"false", status=b"render-failed\n")[0], 1)
        rc, log, _ = self.run_step(b"a: !!python/object/apply:os.system ['touch PWNED']\n", b"false")
        self.assertEqual(rc, 1)
        self.assertIn("YAML tag", log)


# ---------------------------------------------------------------------------------------------------------------- the digest
class DigestEquality(unittest.TestCase):
    """Render the fixture chart through the new path (render-manifests, then the clearance-gate ingest step) with real helm and
    require the tree digest and every object digest in the Clearance request to equal what the gate produced before this change
    (fixtures/render/golden.json). Clearance matches the gate's tree digest against its Argo CD plugin's digest of the same objects, so
    a change here breaks every clearance."""

    def test_the_tree_and_object_digests_are_what_they_were(self):
        golden = json.load(open(os.path.join(FX, "golden.json")))
        with tempfile.TemporaryDirectory() as base:
            w = os.path.join(base, "work")
            for tree, values in (("repo", "replicas: 2\n"), ("base", "replicas: 1\n")):
                d = os.path.join(w, tree, "hangar-prod", "prod")
                os.makedirs(d)
                open(os.path.join(d, "values.yaml"), "w").write(values)
                open(os.path.join(d, "release.yaml"), "w").write(open(os.path.join(FX, "gitops", "hangar-prod", "prod", "release.yaml")).read())
            os.makedirs(os.path.join(w, "observability"))
            open(os.path.join(w, "observability", "hangar-prod.yaml"), "w").close()
            open(os.path.join(w, "plan.txt"), "w").write("hangar-prod|prod|default|true\n")
            open(os.path.join(w, "PUBLISHED"), "w").write("run-1\n")
            os.makedirs(os.path.join(base, "out"))
            rc, log, _ = run_script(script("render-manifests", "render"), base, render_env("clearance-gate", APP_NAME=golden["app"], NAMESPACE=golden["namespace"]))
            self.assertEqual(rc, 0, log)
            # the Pipeline's move: out/ is what the ingest step sees as /rendered
            os.rename(os.path.join(base, "out"), os.path.join(base, "rendered"))
            os.makedirs(os.path.join(base, "trusted"))
            open(os.path.join(base, "trusted", "PUBLISHED"), "w").write("run-1\n")
            open(os.path.join(base, "trusted", "plan.txt"), "w").write("hangar-prod|prod|default|true\n")
            json.dump({"owner": "jfillman", "repo": "gitops-x", "pr": 7, "head": "a" * 40, "base": "b" * 40, "author": "dana",
                       "files": [{"filename": "hangar-prod/prod/release.yaml", "status": "modified", "additions": 1, "deletions": 1}],
                       "looksAgent": False, "provenance": None, "checkRuns": [], "policyCommit": ""}, open(os.path.join(base, "trusted", "meta.json"), "w"))
            os.makedirs(os.path.join(base, "trusted", "policies"))
            open(os.path.join(base, "trusted", "policies", "unreadable"), "w").close()
            os.makedirs(os.path.join(base, "xrds"))
            e = literal_env("clearance-gate", "ingest")
            e.update(PHASE="decide", PIPELINE_RUN="run-1", APP_NAME=golden["app"], NAMESPACE=golden["namespace"], RENDER_ONLY="false")
            rc, log, _ = run_script(script("clearance-gate", "ingest"), base, e)
            self.assertEqual(rc, 0, log)
            change = json.load(open(os.path.join(base, "work", "eval", "hangar-prod__prod", "change.json")))
        self.assertEqual(change["treeDigest"], golden["treeDigest"])
        got = {"/".join([o["apiVersion"], o["kind"], o["namespace"], o["name"]]): {"digest": o["digest"], "op": o["op"]} for o in change["objects"]}
        self.assertEqual(got, golden["objects"])
        self.assertGreaterEqual(len(got), 15, "the comparison is not vacuous: hooks, CRDs, a subchart, a List, YAML corners")
        self.assertRegex(change["treeDigest"], r"^[0-9a-f]{64}$")


# ---------------------------------------------------------------------------------------------------------------- what is shown
class WhatIsShown(unittest.TestCase):
    """policy-validation's findings are rendered into a PR comment. A rendered object's name and a policy message that quotes it are
    attacker-controlled data: policy_eval_env writes only a bounded, escaped form."""

    def test_a_finding_about_a_hostile_object_is_neutralised(self):
        lib = open(os.path.join(CATALOG, "files", "lib", "policy-eval.sh")).read()
        with tempfile.TemporaryDirectory() as base:
            os.makedirs(os.path.join(base, "work", "eval", "c__e"))
            os.makedirs(os.path.join(base, "work", "policies"))
            open(os.path.join(base, "work", "policies", "c.bundle.yaml"), "w").write("x: 1\n")
            json.dump({"p": {"source": "ValidatingPolicy/p", "mode": "deny"}}, open(os.path.join(base, "work", "policies", "c.index.json"), "w"))
            os.makedirs(os.path.join(base, "bin"))
            report = {"results": [{"policy": "p", "rule": "r", "result": "fail",
                                   "resources": [{"kind": "ConfigMap", "name": "ev`il$(x) @pr-lens <b>x</b>\nline2|"}],
                                   "message": "ConfigMap ev`il @pr-lens [click](http://x) <img src=x> " + "z" * 600 + "\nline2 | col"}], "summary": {"fail": 1}}
            open(os.path.join(base, "bin", "kyverno"), "w").write("#!/bin/bash\ncat <<'EOF'\n%s\nEOF\nexit 1\n" % json.dumps(report))
            os.chmod(os.path.join(base, "bin", "kyverno"), 0o755)
            body = localize(lib, base) + '\npolicy_eval_env c e %s/findings.jsonl\n' % base
            open(os.path.join(base, "t.sh"), "w").write(body)
            p = subprocess.run(["bash", os.path.join(base, "t.sh")], cwd=base, env=dict(os.environ, PATH=os.path.join(base, "bin") + ":" + os.environ["PATH"]), capture_output=True, text=True)
            findings = [json.loads(l) for l in open(os.path.join(base, "findings.jsonl"))]
        self.assertEqual(len(findings), 1, p.stdout + p.stderr)
        f = findings[0]
        self.assertEqual(f["level"], "deny")
        for field in ("resource", "message"):
            self.assertRegex(f[field], r"^[^\x00-\x1f\x7f`<>|@\[\]\\]*$", field)
        self.assertLessEqual(len(f["message"]), 300)
        self.assertLessEqual(len(f["resource"]), 200)
        self.assertTrue(f["resource"].startswith("ConfigMap/ev"))


if __name__ == "__main__":
    unittest.main()
