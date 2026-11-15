#!/usr/bin/env python3
"""Pipeline trust T3, verified ref protection (docs/admin/security.md, "Ref protection").

Renders the catalog and asserts, from the manifests alone:
  - every privileged Pipeline (release, deploy, test, gitops-image-bump) and the build starts with the
    verify-ref-protection task, which has no when, no retries, no onError and no parameter but its kind, is not in
    finally, and has every other task of the Pipeline after it (directly or transitively): nothing runs before it.
  - the Pipelines are what the admission policy forces a .tekton file to reference (cluster resolver, no inline spec), so
    the file cannot drop the task: checked against the admission policy's own messages.
  - refProtection.mode is required by default, off only when set, anything else fails the render.
Then runs the task's script against a fake kubectl (the PipelineRun as Pipelines-as-Code recorded it) and a fake curl
(glidepath-api): a protected verdict passes; an unprotected, unavailable or unverifiable one, an API error and an
unreadable run all refuse (fail closed); off asks nothing and records "ref protection: Off here"; the ref and sha come
from the run's annotations; the release record gets the summary.

Run:  python3 charts/glidepath-catalog/tests/ref_protection_pipelines_test.py     (needs helm, pyyaml, jq, bash)
"""
import json
import os
import stat
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
CP = os.path.join(ROOT, "charts", "glidepath-control-plane")
PRIVILEGED = ["release", "deploy", "test", "gitops-image-bump"]
GUARD = "verify-ref-protection"
SHA = "3" * 40


def put(path, text):
    with open(path, "w") as f:
        f.write(text)


def get(path):
    if not os.path.exists(path):
        return ""
    with open(path) as f:
        return f.read()


def render(chart, *args):
    cmd = ["helm", "template", "t", chart, "-f", os.path.join(chart, "tests", "values.example.yaml"), *args]
    p = subprocess.run(cmd, capture_output=True, text=True)
    return p


def docs(chart, *args):
    p = render(chart, *args)
    assert p.returncode == 0, p.stderr
    return [d for d in yaml.safe_load_all(p.stdout) if d]


CAT = docs(CATALOG)
PIPELINES = {d["metadata"]["name"]: d for d in CAT if d["kind"] == "Pipeline"}
TASKS = {d["metadata"]["name"]: d for d in CAT if d["kind"] == "Task"}


def after(pipeline):
    """task name -> every task it runs after (transitively), by runAfter and by result references."""
    spec = pipeline["spec"]["tasks"]
    direct = {}
    for t in spec:
        deps = set(t.get("runAfter") or [])
        text = json.dumps(t.get("params") or []) + json.dumps(t.get("when") or [])
        for other in spec:
            if other["name"] != t["name"] and f"$(tasks.{other['name']}." in text:
                deps.add(other["name"])
        direct[t["name"]] = deps
    out = {}

    def walk(n, seen):
        for d in direct[n]:
            if d not in seen:
                seen.add(d)
                walk(d, seen)
        return seen

    for n in direct:
        out[n] = walk(n, set())
    return out


class Order(unittest.TestCase):
    def test_every_privileged_pipeline_and_the_build_starts_with_the_guard(self):
        for name in PRIVILEGED + ["build"]:
            p = PIPELINES[name]
            tasks = p["spec"]["tasks"]
            self.assertEqual(tasks[0]["name"], GUARD, name)
            g = tasks[0]
            for forbidden in ("when", "retries", "onError", "runAfter", "timeout"):
                self.assertNotIn(forbidden, g, f"{name}: the guard may not be skippable, retried or ordered after anything")
            self.assertEqual(g["taskRef"]["resolver"], "cluster", name)
            self.assertEqual({x["name"]: x["value"] for x in g["params"]}, {"pipeline-kind": "build" if name == "build" else "privileged"}, name)
            self.assertNotIn(GUARD, [t["name"] for t in p["spec"].get("finally", [])], name)
            # Nothing but the guard may start without it.
            deps = after(p)
            for t in tasks[1:]:
                self.assertIn(GUARD, deps[t["name"]], f"{name}: {t['name']} can run before verify-ref-protection (runs after {sorted(deps[t['name']])})")

    def test_the_guard_takes_nothing_a_file_can_set(self):
        t = TASKS[GUARD]
        self.assertEqual([p["name"] for p in t["spec"]["params"]], ["pipeline-kind"])
        step = t["spec"]["steps"][0]
        self.assertNotIn("workspaces", t["spec"])
        script = step["script"]
        # ref, sha, repo and event are the run's own annotations; the app is the namespace
        for a in ("event-type", "source-branch", "sha", "repo-url"):
            self.assertIn(f"ann {a}", script)
        self.assertNotIn("$(params.git", script)
        self.assertNotIn("$(params.revision", script)

    def test_the_release_record_gets_the_summary(self):
        rel = {t["name"]: t for t in PIPELINES["release"]["spec"]["tasks"]}["open-release-pr"]
        v = {p["name"]: p["value"] for p in rel["params"]}["ref-protection"]
        self.assertEqual(v, f"$(tasks.{GUARD}.results.ref-protection-summary)")
        text = json.dumps(TASKS["open-release-pr"])
        self.assertIn("refProtection=${REF_PROTECTION}", text)

    def test_the_pipelines_a_file_may_start_are_the_catalogs(self):
        # cicd-run-shape: cluster resolver, no inline pipelineSpec (the admission test starts runs that break it and expects Deny).
        policy = [d for d in docs(CP) if d["kind"] == "ValidatingAdmissionPolicy" and d["metadata"]["name"] == "cicd-run-shape"][0]
        msgs = " ".join(v["message"] for v in policy["spec"]["validations"])
        self.assertIn("may not carry an inline pipelineSpec", msgs)
        self.assertIn("cluster resolver", msgs)
        for n in PRIVILEGED + ["build"]:
            self.assertIn(n, yaml.safe_load(
                [v for v in policy["spec"]["variables"] if v["name"] == "catalog"][0]["expression"].strip("'")))


class Setting(unittest.TestCase):
    def mode(self, *args):
        d = [x for x in docs(CATALOG, *args) if x["kind"] == "Task" and x["metadata"]["name"] == GUARD][0]
        return {e["name"]: e["value"] for e in d["spec"]["steps"][0]["env"]}["REF_PROTECTION_MODE"]

    def test_required_by_default_off_only_when_set(self):
        self.assertEqual(self.mode(), "required")
        self.assertEqual(self.mode("--set", "refProtection.mode=off"), "off")
        p = render(CATALOG, "--set", "refProtection.mode=sometimes")
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("must be required or off", p.stderr)


class Script(unittest.TestCase):
    """The task's own bash, against a fake kubectl and a fake curl."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        for mode in ("required", "off"):
            task = [d for d in docs(CATALOG, "--set", f"refProtection.mode={mode}") if d["kind"] == "Task" and d["metadata"]["name"] == GUARD][0]
            script = task["spec"]["steps"][0]["script"]
            script = script.replace("$(results.ref-protection.path)", os.path.join(cls.tmp, "res-" + mode)) \
                           .replace("$(results.ref-protection-summary.path)", os.path.join(cls.tmp, "sum-" + mode))
            put(os.path.join(cls.tmp, f"{mode}.sh"), script)
            cls.env_of = {e["name"]: e["value"] for e in task["spec"]["steps"][0]["env"]}
        bin_ = os.path.join(cls.tmp, "bin")
        os.makedirs(bin_)
        kubectl = '#!/usr/bin/env bash\n[[ -f "$FX/run.json" ]] || { echo "not found" >&2; exit 1; }\ncat "$FX/run.json"\n'
        curl = r'''#!/usr/bin/env bash
echo "$@" >> "$FX/curl-calls"
out=""; while (( $# )); do case "$1" in -o) out="$2"; shift ;; esac; shift; done
[[ -f "$FX/curl-fail" ]] && exit 7
cp "$FX/api-body.json" "$out"; printf '%s' "$(cat "$FX/api-code")"
'''
        for n, body in (("kubectl", kubectl), ("curl", curl)):
            path = os.path.join(bin_, n)
            put(path, body)
            os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC)
        put(os.path.join(cls.tmp, "token"), "sa-token")

    def run_task(self, *, mode="required", kind="privileged", event="push", ref="refs/heads/main", anns=None, api=None, code="200",
                 no_run=False, curl_fail=False, namespace="app-shop-cicd"):
        fx = tempfile.mkdtemp(dir=self.tmp)
        a = {"pipelinesascode.tekton.dev/sha": SHA, "pipelinesascode.tekton.dev/repo-url": "https://github.com/jfillman/shop",
             "pipelinesascode.tekton.dev/source-branch": ref}
        if event:
            a["pipelinesascode.tekton.dev/event-type"] = event
        a.update(anns or {})
        if not no_run:
            put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": a}}))
        put(os.path.join(fx, "api-body.json"), json.dumps(api if api is not None else {}))
        put(os.path.join(fx, "api-code"), code)
        if curl_fail:
            put(os.path.join(fx, "curl-fail"), "1")
        env = dict(os.environ, FX=fx, PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"],
                   REF_PROTECTION_MODE=mode, GLIDEPATH_API_URL="http://api.example", PIPELINE_RUN_NAME="release-abc", NAMESPACE=namespace,
                   PIPELINE_KIND=kind, TOKEN_PATH=os.path.join(self.tmp, "token"))
        p = subprocess.run(["bash", os.path.join(self.tmp, f"{mode}.sh")], capture_output=True, text=True, env=env)
        res = get(os.path.join(self.tmp, f"res-{mode}"))
        summ = get(os.path.join(self.tmp, f"sum-{mode}"))
        for f in (f"res-{mode}", f"sum-{mode}"):
            if os.path.exists(os.path.join(self.tmp, f)):
                os.remove(os.path.join(self.tmp, f))
        calls = get(os.path.join(fx, "curl-calls"))
        return p, res, summ, calls

    PROTECTED = {"verdict": "protected", "mainLike": True, "reasons": ["branch main requires a reviewed pull request"],
                 "bypass": [{"actorType": "RepositoryRole", "actorId": 5, "moot": False}]}

    def test_protected_passes_with_the_runs_own_ref_and_sha(self):
        p, res, summ, calls = self.run_task(api=self.PROTECTED)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(res, "verified")
        self.assertIn("verified for refs/heads/main", summ)
        self.assertIn("http://api.example/v1/apps/shop/ref-protection", calls)
        self.assertIn("repo=jfillman/shop", calls)
        self.assertIn("ref=refs/heads/main", calls)
        self.assertIn(f"sha={SHA}", calls)
        self.assertIn("Authorization: Bearer sa-token", calls)

    def test_a_tag_run_asks_about_the_tag_ref(self):
        p, res, _, calls = self.run_task(ref="refs/tags/v1.2.3", api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "verified"))
        self.assertIn("ref=refs/tags/v1.2.3", calls)

    def test_infra_app_namespace(self):
        _, _, _, calls = self.run_task(api=self.PROTECTED, namespace="infra-net-cicd")
        self.assertIn("/v1/apps/net/ref-protection", calls)

    def test_anything_but_protected_refuses_with_a_clear_message(self):
        for verdict in ("unprotected", "unavailable", "unverifiable", "", "PROTECTED"):
            with self.subTest(verdict=verdict):
                api = {"verdict": verdict, "mainLike": True, "reasons": ["branch main is not protected as required: missing blocking deletion"],
                       "missing": ["blocking deletion"], "bypass": [{"actorType": "Team", "moot": True}]}
                p, res, summ, _ = self.run_task(api=api)
                self.assertEqual(p.returncode, 1)
                self.assertEqual(res, "refused")
                self.assertIn("refused", summ)
                self.assertIn("NOT verified as protected", p.stderr)
                self.assertIn("missing blocking deletion", p.stderr)
                self.assertIn("missing: blocking deletion", p.stderr)
                self.assertIn("Team (makes it moot)", p.stderr)
                self.assertIn("refProtection.mode: off", p.stderr)

    def test_fail_closed(self):
        for name, kw in {
            "API 502": dict(api={"error": "GitHub failed"}, code="502"),
            "API 403": dict(api={"error": "forbidden"}, code="403"),
            "API 200 but no verdict": dict(api={}),
            "API unreachable": dict(curl_fail=True),
            "run unreadable": dict(no_run=True),
            "no sha recorded": dict(anns={"pipelinesascode.tekton.dev/sha": ""}, api=self.PROTECTED),
            "no ref recorded": dict(ref="", api=self.PROTECTED),
            "not an app namespace": dict(namespace="default", api=self.PROTECTED),
        }.items():
            with self.subTest(name):
                p, res, summ, _ = self.run_task(**kw)
                self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
                self.assertEqual(res, "refused")

    def test_pull_request_and_chained_and_side_refs(self):
        # A privileged Pipeline is push-only: a pull request event is refused.
        p, res, _, calls = self.run_task(event="pull_request", api=self.PROTECTED)
        self.assertEqual((p.returncode, res, calls), (1, "refused", ""))
        # The build passes a pull request (it writes no main image) without asking.
        p, res, _, calls = self.run_task(kind="build", event="pull_request", api=self.PROTECTED)
        self.assertEqual((p.returncode, res, calls), (0, "not-push", ""))
        # A run the platform chained (no Pipelines-as-Code annotation).
        p, res, summ, calls = self.run_task(event="", api={})
        self.assertEqual((p.returncode, res, calls), (0, "chained", ""))
        self.assertIn("chained", summ)
        # The build on a side branch: no main image, so nothing to verify; on main or a tag it must be protected.
        side = {"verdict": "unprotected", "mainLike": False, "reasons": ["no"]}
        p, res, _, _ = self.run_task(kind="build", ref="refs/heads/feature/x", api=side)
        self.assertEqual((p.returncode, res), (0, "side-ref"))
        main = dict(side, mainLike=True)
        p, res, _, _ = self.run_task(kind="build", api=main)
        self.assertEqual((p.returncode, res), (1, "refused"))
        p, res, _, _ = self.run_task(kind="build", api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "verified"))
        # A privileged Pipeline never takes the side-ref exit.
        p, res, _, _ = self.run_task(kind="privileged", ref="refs/heads/feature/x", api=side)
        self.assertEqual((p.returncode, res), (1, "refused"))

    def test_off_asks_nothing_and_records_the_gap(self):
        for kind in ("privileged", "build"):
            p, res, summ, calls = self.run_task(mode="off", kind=kind, api={"verdict": "unprotected"})
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertEqual(res, "off")
            self.assertIn("ref protection: Off here", summ)
            self.assertIn("refProtection.mode is off", summ)
            self.assertEqual(calls, "", "off asks the API nothing")

    def test_unknown_kind_or_mode_refuses(self):
        p, res, _, _ = self.run_task(kind="whatever", api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (1, "refused"))
        p, res, _, _ = self.run_task(mode="required", api=self.PROTECTED)  # sanity: required still passes
        self.assertEqual(p.returncode, 0)


if __name__ == "__main__":
    unittest.main()
