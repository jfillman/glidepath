#!/usr/bin/env python3
"""Pipeline trust T3 and T3b, review evidence (docs/admin/security.md, "Ref protection").

Renders the catalog and asserts, from the manifests alone:
  - every privileged Pipeline (release, deploy, test, gitops-image-bump) and the build starts with the
    verify-ref-protection task, which has no when, no retries, no onError and no parameter but its kind, is not in
    finally, and has every other task of the Pipeline after it (directly or transitively): nothing runs before it.
  - the Pipelines are what the admission policy forces a .tekton file to reference (cluster resolver, no inline spec), so
    the file cannot drop the task: checked against the admission policy's own messages.
  - refProtection.mode is required by default, off only when set, anything else fails the render.
Then runs the task's script against a fake kubectl (the PipelineRun as Pipelines-as-Code recorded it) and a fake curl
(glidepath-api): a protected verdict passes and records "review evidence: ..."; an unreviewed or unverifiable one, an API
error and an unreadable run all refuse (fail closed), listing the commits a developer must fix in at most 20 lines;
off asks nothing and records "ref protection: Off here"; the ref and sha come from the run's annotations; the release
record gets the summary. The elevated broker permission (Administration) is gone: nothing in the control plane renders it.

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
                 no_run=False, curl_fail=False, namespace="app-shop-cicd", params=None):
        fx = tempfile.mkdtemp(dir=self.tmp)
        a = {"pipelinesascode.tekton.dev/sha": SHA, "pipelinesascode.tekton.dev/repo-url": "https://github.com/jfillman/shop",
             "pipelinesascode.tekton.dev/source-branch": ref}
        if event:
            a["pipelinesascode.tekton.dev/event-type"] = event
        a.update(anns or {})
        if not no_run:
            put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": a}, "spec": {"params": params or []}}))
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

    SUMMARY = "review evidence: 7 commits in 3 pull requests, all approved by someone other than their authors on the final head (main, range 1111111..2222222)"
    PROTECTED = {"verdict": "protected", "mainLike": True, "reasons": [SUMMARY],
                 "configuration": {"state": "unavailable", "reasons": ["o/shop: the git host offers no rulesets or branch protection for this repository on its plan"]}}

    def test_protected_passes_with_the_runs_own_ref_and_sha(self):
        p, res, summ, calls = self.run_task(api=self.PROTECTED)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(res, "verified")
        self.assertEqual(summ.split(" [")[0], "ref protection: " + self.SUMMARY)
        self.assertIn("[ruleset note: unavailable: o/shop: the git host offers no rulesets", summ)
        self.assertTrue(summ.startswith("ref protection: review evidence: 7 commits in 3 pull requests"), summ)
        self.assertIn("http://api.example/v1/apps/shop/ref-protection", calls)
        self.assertIn("repo=jfillman/shop", calls)
        self.assertIn("ref=refs/heads/main", calls)
        self.assertIn(f"sha={SHA}", calls)
        self.assertIn("Authorization: Bearer sa-token", calls)

    def test_the_target_environment_and_cluster_go_with_the_question(self):
        params = [{"name": "env", "value": "prod"}, {"name": "cluster", "value": "hangar-prod"}]
        p, res, _, calls = self.run_task(api=self.PROTECTED, params=params)
        self.assertEqual((p.returncode, res), (0, "verified"), p.stderr)
        self.assertIn("env=prod", calls)
        self.assertIn("cluster=hangar-prod", calls)
        # a run with no target parameter sends neither (the API then reads the strictest history)
        p, res, _, calls = self.run_task(api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "verified"))
        self.assertNotIn("env=", calls)
        self.assertNotIn("cluster=", calls)

    def test_a_tag_run_asks_about_the_tag_ref(self):
        p, res, _, calls = self.run_task(ref="refs/tags/v1.2.3", api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "verified"))
        self.assertIn("ref=refs/tags/v1.2.3", calls)

    def test_infra_app_namespace(self):
        _, _, _, calls = self.run_task(api=self.PROTECTED, namespace="infra-net-cicd")
        self.assertIn("/v1/apps/net/ref-protection", calls)

    def test_anything_but_protected_refuses_with_a_clear_message(self):
        for verdict in ("unreviewed", "unverifiable", "unprotected", "unavailable", "", "PROTECTED"):
            with self.subTest(verdict=verdict):
                api = {"verdict": verdict, "mainLike": True, "reasons": ["review evidence is incomplete: 2 of 5 commits have no approved pull request (main, range 1111111..2222222)"],
                       "missing": ["aaaaaaa: no pull request: pushed to main directly (a direct push, a force push or an administrator's bypass)",
                                   "bbbbbbb: in #7, which has no usable approval: approved only on an older head (bob on ccccccc) but the final head is ddddddd: it was pushed to after the approval"],
                       "configuration": {"state": "protected", "reasons": ["branch main requires a reviewed pull request"]}}
                p, res, summ, _ = self.run_task(api=api)
                self.assertEqual(p.returncode, 1)
                self.assertEqual(res, "refused")
                self.assertIn("refused", summ)
                self.assertIn("has NO review evidence for", p.stderr)
                self.assertIn("2 of 5 commits have no approved pull request", p.stderr)
                self.assertIn("    aaaaaaa: no pull request: pushed to main directly", p.stderr)
                self.assertIn("    bbbbbbb: in #7, which has no usable approval: approved only on an older head", p.stderr)
                self.assertIn("(ruleset note: protected: branch main requires a reviewed pull request)", p.stderr)
                self.assertIn("refProtection.mode: off", p.stderr)
                if verdict == "unreviewed":
                    self.assertIn("approves on its final head", p.stderr)
                    self.assertIn("single maintainer cannot pass", p.stderr)
                else:
                    self.assertIn("the evidence could not be established", p.stderr)

    def test_a_refusal_prints_a_bounded_number_of_lines(self):
        api = {"verdict": "unreviewed", "mainLike": True, "reasons": ["r " * 1000],
               "missing": [f"{i:07d}: " + "x" * 1000 for i in range(60)]}
        p, res, summ, _ = self.run_task(api=api)
        self.assertEqual((p.returncode, res), (1, "refused"))
        listed = [l for l in p.stderr.splitlines() if l.startswith("    0")]
        self.assertEqual(len(listed), 20, "no more than 20 commit lines")
        self.assertTrue(all(len(l) <= 304 for l in listed), "each line is cut")
        self.assertLessEqual(len(p.stderr.splitlines()), 30)
        self.assertLessEqual(len(summ), 800, "the one-line summary stays inside a Tekton result")

    def test_the_summary_is_what_the_next_range_recognises(self):
        # glidepath-api treats a release record whose refProtection contains "review evidence:" as a verified release
        for kw in (dict(), dict(ref="refs/tags/v1.2.3")):
            p, res, summ, _ = self.run_task(api=self.PROTECTED, **kw)
            self.assertIn("review evidence:", summ)
        p, res, summ, _ = self.chained(api=self.PROTECTED)
        self.assertIn("review evidence:", summ)
        p, res, summ, _ = self.run_task(mode="off", api=self.PROTECTED)
        self.assertNotIn("review evidence:", summ, "an Off here release is never a verified baseline")

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
        # The build on a side branch: no main image, so nothing to verify; on main or a tag it must be protected.
        side = {"verdict": "unreviewed", "mainLike": False, "reasons": ["no"]}
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

    # ---- chained runs: the guard verifies the origin the Trigger stamped, never a free pass.
    CHAIN_PARAMS = [{"name": "git-url", "value": "https://github.com/jfillman/shop"}, {"name": "git-revision", "value": SHA}]

    def chained(self, **kw):
        anns = {"glidepath.io/origin-ref": "refs/heads/main", "glidepath.io/origin-sha": SHA}
        anns.update(kw.pop("anns", {}))
        return self.run_task(event="", anns=anns, params=kw.pop("params", self.CHAIN_PARAMS), **kw)

    def test_a_chained_run_verifies_its_origin(self):
        p, res, summ, calls = self.chained(api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "origin-verified"), p.stderr)
        self.assertIn("(origin refs/heads/main)", summ)
        self.assertIn("ref=refs/heads/main", calls)
        self.assertIn(f"sha={SHA}", calls)
        self.assertIn("repo=jfillman/shop", calls)

    def test_a_chained_run_from_an_unprotected_origin_is_refused(self):
        p, res, _, calls = self.chained(ref="refs/heads/feature/x", anns={"glidepath.io/origin-ref": "refs/heads/feature/x"},
                                        api={"verdict": "unreviewed", "mainLike": False, "reasons": ["no"]})
        self.assertEqual((p.returncode, res), (1, "refused"))
        self.assertIn("ref=refs/heads/feature/x", calls)  # asked, and refused: a side ref never takes the build's exit

    def test_a_chained_run_with_no_verifiable_origin_is_refused(self):
        for name, kw in {
            "no origin at all": dict(anns={"glidepath.io/origin-ref": "", "glidepath.io/origin-sha": ""}),
            "no origin sha": dict(anns={"glidepath.io/origin-sha": ""}),
            "origin is not the revision": dict(anns={"glidepath.io/origin-sha": "4" * 40}),
            "no revision param": dict(params=[self.CHAIN_PARAMS[0]]),
            "no git-url param": dict(params=[self.CHAIN_PARAMS[1]]),
        }.items():
            with self.subTest(name):
                p, res, _, _ = self.chained(api=self.PROTECTED, **kw)
                self.assertEqual((p.returncode, res), (1, "refused"), p.stdout)
        # a run with neither a Pipelines-as-Code event nor an origin stamp
        p, res, _, _ = self.run_task(event="", api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (1, "refused"))
        # a build with neither an event nor an origin is refused too
        p, res, _, _ = self.run_task(kind="build", event="", api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (1, "refused"))

    # ---- runs the API starts: reruns, promote and rollback carry a stamped origin, verified like any other.
    def test_a_rerun_of_a_main_build_verifies_its_origin(self):
        p, res, _, calls = self.run_task(kind="build", event="", anns={"glidepath.io/origin-ref": "refs/heads/main", "glidepath.io/origin-sha": SHA},
                                         params=self.CHAIN_PARAMS, api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "origin-verified"), p.stderr)
        self.assertIn("ref=refs/heads/main", calls)
        # unprotected origin of a main-like ref: refused; a side ref passes the build's side-ref exit as before
        p, res, _, _ = self.run_task(kind="build", event="", anns={"glidepath.io/origin-ref": "refs/heads/main", "glidepath.io/origin-sha": SHA},
                                     params=self.CHAIN_PARAMS, api={"verdict": "unreviewed", "mainLike": True, "reasons": ["no"]})
        self.assertEqual((p.returncode, res), (1, "refused"))
        p, res, _, _ = self.run_task(kind="build", event="", anns={"glidepath.io/origin-ref": "refs/heads/feature/x", "glidepath.io/origin-sha": SHA},
                                     params=self.CHAIN_PARAMS, api={"verdict": "unreviewed", "mainLike": False, "reasons": ["no"]})
        self.assertEqual((p.returncode, res), (0, "side-ref"))

    def test_a_rerun_of_a_pull_request_build_is_not_a_push(self):
        p, res, _, calls = self.run_task(kind="build", event="", anns={"glidepath.io/origin-event": "pull_request"}, params=self.CHAIN_PARAMS, api=self.PROTECTED)
        self.assertEqual((p.returncode, res, calls), (0, "not-push", ""))
        # a privileged Pipeline never takes that exit
        p, res, _, _ = self.run_task(kind="privileged", event="", anns={"glidepath.io/origin-event": "pull_request"}, params=self.CHAIN_PARAMS, api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (1, "refused"))

    def test_a_promote_or_rollback_run_verifies_the_stamped_commit(self):
        # glidepath-api stamps the configured protected branch and the release's commit; the guard asks the host about exactly that
        # (a rollback's older sha is an ancestor of the branch head, which the API's reachability check accepts by design)
        older = "5" * 40
        params = [{"name": "git-url", "value": "https://github.com/jfillman/shop"}, {"name": "git-revision", "value": older}]
        p, res, _, calls = self.run_task(event="", anns={"glidepath.io/origin-ref": "refs/heads/main", "glidepath.io/origin-sha": older},
                                         params=params, api=self.PROTECTED)
        self.assertEqual((p.returncode, res), (0, "origin-verified"), p.stderr)
        self.assertIn(f"sha={older}", calls)
        # not on the protected branch: the API says so and the run is refused
        p, res, _, _ = self.run_task(event="", anns={"glidepath.io/origin-ref": "refs/heads/main", "glidepath.io/origin-sha": older}, params=params,
                                     api={"verdict": "unreviewed", "mainLike": True, "reasons": ["commit is not on branch main"]})
        self.assertEqual((p.returncode, res), (1, "refused"))

    def test_the_failure_says_what_a_misordered_upgrade_needs(self):
        p, _, summ, _ = self.run_task(api={"error": "no route"}, code="404")
        self.assertIn("no ref-protection endpoint", p.stderr)
        self.assertIn("glidepath-api, then glidepath-app", p.stderr)
        self.assertIn("refProtection.mode: off", p.stderr)
        p, _, _, _ = self.run_task(api={"error": "forbidden"}, code="403")
        self.assertIn("may not read ref protection", p.stderr)
        self.assertIn("refprotection grant", p.stderr)
        self.assertIn("glidepath-api, then glidepath-app, then the catalog", p.stderr)
        p, _, _, _ = self.run_task(curl_fail=True)
        self.assertIn("could not be reached", p.stderr)

    def test_off_asks_nothing_and_records_the_gap(self):
        for kind in ("privileged", "build"):
            p, res, summ, calls = self.run_task(mode="off", kind=kind, api={"verdict": "unreviewed"})
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


class BrokerSetting(unittest.TestCase):
    """The broker mints only the read-only token: the elevated ref-protection permission and its callers are gone."""

    def deployment(self, name, *args):
        return [x for x in docs(CP, *args) if x["kind"] == "Deployment" and x["metadata"]["name"] == name][0]

    def env(self, name, *args):
        d = self.deployment(name, *args)
        return {e["name"]: e.get("value") for c in d["spec"]["template"]["spec"]["containers"] for e in c.get("env", [])}

    def test_the_broker_has_no_elevation_setting(self):
        env = self.env("cdevents-broker-auth")
        self.assertEqual(env["GITHUB_READ_CALLERS"], "system:serviceaccount:platform-system:glidepath-api")
        self.assertNotIn("GITHUB_READ_REF_PROTECTION_PERMISSIONS", env)
        self.assertNotIn("GITHUB_READ_REF_PROTECTION_CALLERS", env)
        # a values file from before T3b that still sets them changes nothing: the chart no longer reads them
        e = self.env("cdevents-broker-auth", "--set", "githubRead.refProtectionPermissions.administration=write",
                     "--set", "githubRead.refProtectionCallers[0]=system:serviceaccount:x:y")
        self.assertNotIn("GITHUB_READ_REF_PROTECTION_PERMISSIONS", e)
        self.assertNotIn("administration", json.dumps(docs(CP, "--set", "githubRead.refProtectionPermissions.administration=write")))

    def test_no_chart_value_or_template_mentions_the_elevated_path(self):
        import pathlib
        root = pathlib.Path(CP)
        for f in list(root.rglob("*.yaml")) + list(root.rglob("*.tpl")):
            if "tests" in f.parts:
                continue
            text = f.read_text()
            self.assertNotIn("refProtectionPermissions", text, f)
            self.assertNotIn("refProtectionCallers", text, f)
            self.assertNotIn("REF_PROTECTION_PERMISSIONS", text, f)

    def test_the_api_gets_the_admission_values_and_the_pipeline_logins(self):
        env = self.env("glidepath-api", "--set", "apiImage=x:1")
        self.assertEqual(env["PROTECTED_BRANCHES"], "main")
        self.assertIn("refs/tags/v", env["TAG_REF_PATTERN"])
        self.assertEqual(env["PIPELINE_LOGINS"], "")
        env = self.env("glidepath-api", "--set", "apiImage=x:1", "--set", "api.pipelineLogins={glidepath-app[bot],other}")
        self.assertEqual(env["PIPELINE_LOGINS"], "glidepath-app[bot],other")


class Debrief(unittest.TestCase):
    """stage-debrief puts the originating ref and sha into the domain event, from the run's own annotations."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        task = TASKS["stage-debrief"]
        step = [x for x in task["spec"]["steps"] if x["name"] == "resolve"][0]
        script = step["script"]
        for r in ("status", "image-ref", "config-json-b64", "subject-content-json-b64"):
            script = script.replace(f"$(step.results.{r}.path)", os.path.join(cls.tmp, r))
        put(os.path.join(cls.tmp, "resolve.sh"), script)
        bin_ = os.path.join(cls.tmp, "bin")
        os.makedirs(bin_)
        put(os.path.join(bin_, "kubectl"), '#!/usr/bin/env bash\ncase "$*" in *"-o json"*) cat "$FX/run.json" ;; *) exit 0 ;; esac\n')
        put(os.path.join(bin_, "base64"), '#!/usr/bin/env bash\nexec /usr/bin/base64 | tr -d "\\n"\n')
        for n in ("kubectl", "base64"):
            os.chmod(os.path.join(bin_, n), 0o755)

    def content(self, annotations):
        fx = tempfile.mkdtemp(dir=self.tmp)
        put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": annotations}}))
        env = dict(os.environ, FX=fx, PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], PIPELINE_STATUS="Succeeded",
                   APP_NAMESPACE="app-shop-cicd", PIPELINE_RUN_NAME="ci-0-build-x", IMAGE_REF_PARAM="r/i:1", IMAGE_REF_FROM_TASK="",
                   CONFIG_JSON_PARAM="{}", CONFIG_JSON_FROM_TASK="", CONTENT_TEMPLATE='{"revision":"abc","image":"${IMAGE_REF}"}')
        p = subprocess.run(["bash", os.path.join(self.tmp, "resolve.sh")], capture_output=True, text=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        import base64
        return json.loads(base64.b64decode(get(os.path.join(self.tmp, "subject-content-json-b64"))))

    def test_a_push_run_carries_its_own_ref_and_sha(self):
        c = self.content({"pipelinesascode.tekton.dev/event-type": "push", "pipelinesascode.tekton.dev/source-branch": "refs/heads/main",
                          "pipelinesascode.tekton.dev/sha": SHA, "glidepath.io/origin-ref": "refs/heads/forged"})
        self.assertEqual((c["origin-ref"], c["origin-sha"], c["revision"], c["image"]), ("refs/heads/main", SHA, "abc", "r/i:1"))

    def test_a_chained_run_passes_its_stamp_on(self):
        c = self.content({"glidepath.io/origin-ref": "refs/tags/v1.2.3", "glidepath.io/origin-sha": SHA})
        self.assertEqual((c["origin-ref"], c["origin-sha"]), ("refs/tags/v1.2.3", SHA))

    def test_a_pull_request_has_no_origin_to_pass_on(self):
        c = self.content({"pipelinesascode.tekton.dev/event-type": "pull_request", "pipelinesascode.tekton.dev/source-branch": "feature",
                          "pipelinesascode.tekton.dev/sha": SHA, "glidepath.io/origin-ref": "refs/heads/main"})
        self.assertEqual((c["origin-ref"], c["origin-sha"]), ("", ""))


if __name__ == "__main__":
    unittest.main()
