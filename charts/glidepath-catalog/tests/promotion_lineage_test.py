#!/usr/bin/env python3
"""Promotion lineage (docs/admin/api.md#promotion-lineage, docs/admin/tracing.md "Promotions join the build's trace").

A promotion keeps its OWN chain; what links it to the build is annotations glidepath-api stamps on the run (admission
refuses every glidepath.io/ annotation on a run a .tekton file creates). Two trusted tasks read them from the run itself:

  - stage-preflight joins the build's trace (a sibling stage span under the build's flow root) when the build finished
    within the catalog's promotionLineage.traceJoinWindow, and otherwise starts its own trace; it also accepts an incoming
    trace context WITHOUT an incoming chain id (new chain, same trace). A run that joined never closes the other flow's
    root span. A run it cannot read, a malformed context, an unreadable time or a stale build is never a join.
  - open-release-pr writes originChainId, originChainSlug and originTraceId on the release record, from the run's
    annotations and never from a parameter, checked for shape.

Runs the two tasks' own bash against a fake kubectl.   Run:  python3 charts/glidepath-catalog/tests/promotion_lineage_test.py
(needs helm, pyyaml, jq, bash, uuidgen, openssl)
"""
import datetime
import json
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
LIB = os.path.join(ROOT, "catalog", "lib")
TRACE = "98f0963d6bc08cb69e6051852fc528de"
TP = f"00-{TRACE}-a71828a66b3e8c7d-01"
BUILD_CHAIN = "94e8228a-5cdc-4350-914a-d73802e5b296"
BUILD_START = "2026-11-15T20:53:01.775206426Z"


def put(path, text):
    with open(path, "w") as f:
        f.write(text)


def get(path):
    if not os.path.exists(path):
        return ""
    with open(path) as f:
        return f.read()


def docs(*args):
    cmd = ["helm", "template", "t", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), *args]
    p = subprocess.run(cmd, capture_output=True, text=True)
    assert p.returncode == 0, p.stderr
    return [d for d in yaml.safe_load_all(p.stdout) if d]


CAT = docs()
TASKS = {d["metadata"]["name"]: d for d in CAT if d["kind"] == "Task"}
PIPELINES = {d["metadata"]["name"]: d for d in CAT if d["kind"] == "Pipeline"}


def ago(seconds):
    t = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(seconds=seconds)
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


class Wiring(unittest.TestCase):
    def preflight(self, p):
        return {x["name"]: x["value"] for x in next(t for t in p["spec"]["tasks"] if t["name"] == "preflight")["params"]}

    def test_the_release_pipeline_hands_the_window_to_preflight_and_closes_the_root_by_its_result(self):
        rel = PIPELINES["release"]
        self.assertEqual(self.preflight(rel)["origin-trace-window"], "12h")
        self.assertEqual(self.preflight(rel)["is-flow-terminal"], "$(params.is-flow-terminal)")
        debrief = {x["name"]: x["value"] for x in rel["spec"]["finally"][0]["params"]}
        self.assertEqual(debrief["is-flow-terminal"], "$(tasks.preflight.results.flow-terminal)")

    def test_the_window_is_a_catalog_value(self):
        p = {d["metadata"]["name"]: d for d in docs("--set", "promotionLineage.traceJoinWindow=6h") if d["kind"] == "Pipeline"}["release"]
        self.assertEqual(self.preflight(p)["origin-trace-window"], "6h")

    def test_no_other_stage_joins_a_trace(self):
        for name, p in PIPELINES.items():
            if name == "release":
                continue
            for t in p["spec"]["tasks"]:
                if t["name"] == "preflight":
                    self.assertNotIn("origin-trace-window", {x["name"] for x in t["params"]}, name)

    def test_the_tasks_read_the_annotations_not_a_parameter(self):
        for name, key in (("stage-preflight", "origin-trace-window"), ("open-release-pr", "PIPELINE_RUN_NAME")):
            text = json.dumps(TASKS[name])
            self.assertIn(key, text)
        for name in ("stage-preflight", "open-release-pr"):
            for step in TASKS[name]["spec"]["steps"]:
                script = step.get("script", "")
                self.assertNotIn("param origin", script)
                self.assertNotRegex(script, r"\$\(params\.origin-(chain|trace)")
        for name in ("open-release-pr", "stage-preflight"):
            env = {e["name"]: e for e in TASKS[name]["spec"]["steps"][0]["env"]}
            self.assertEqual(env["PIPELINE_RUN_NAME"]["valueFrom"]["fieldRef"]["fieldPath"], "metadata.labels['tekton.dev/pipelineRun']", name)


class Preflight(unittest.TestCase):
    """stage-preflight's chain and trace block, against a fake kubectl."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        script = TASKS["stage-preflight"]["spec"]["steps"][0]["script"]
        start = script.index("# Promotion lineage: the build's trace context")
        end = script.index("# This stage's own span")
        seg = script[start:end]
        for r in ("traceparent", "start-time", "chain-id", "chain-slug", "trace-joined", "flow-terminal"):
            seg = seg.replace(f"$(results.{r}.path)", os.path.join(cls.tmp, "r-" + r))
        put(os.path.join(cls.tmp, "seg.sh"), "set -euo pipefail\nsource \"${PLATFORM_LIB}/otel.sh\"\nsource \"${PLATFORM_LIB}/chain-slug.sh\"\n" + seg)
        bin_ = os.path.join(cls.tmp, "bin")
        os.makedirs(bin_)
        put(os.path.join(bin_, "kubectl"), '#!/usr/bin/env bash\n[[ -f "$FX/run.json" ]] || { echo "forbidden" >&2; exit 1; }\ncat "$FX/run.json"\n')
        os.chmod(os.path.join(bin_, "kubectl"), 0o755)

    def run_seg(self, annotations=None, window="12h", chain="", tp="", terminal="true", unreadable=False):
        fx = tempfile.mkdtemp(dir=self.tmp)
        if not unreadable:
            put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": annotations or {}}}))
        for r in ("traceparent", "start-time", "chain-id", "chain-slug", "trace-joined", "flow-terminal"):
            if os.path.exists(os.path.join(self.tmp, "r-" + r)):
                os.remove(os.path.join(self.tmp, "r-" + r))
        env = dict(os.environ, FX=fx, PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], PLATFORM_LIB=LIB,
                   ORIGIN_TRACE_WINDOW=window, INCOMING_CHAIN_ID=chain, INCOMING_TRACEPARENT=tp, INCOMING_FLOW_START_TIME="2026-11-15T20:00:00Z" if chain else "",
                   IS_FLOW_TERMINAL=terminal, PIPELINE_RUN_NAME="promote-staging-e65cefae-7ef9ab", NAMESPACE="app-gate-api-cicd", STAGE_NAME="release")
        p = subprocess.run(["bash", os.path.join(self.tmp, "seg.sh")], capture_output=True, text=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        out = {r: get(os.path.join(self.tmp, "r-" + r)) for r in ("traceparent", "start-time", "chain-id", "chain-slug", "trace-joined", "flow-terminal")}
        out["stderr"], out["stdout"] = p.stderr, p.stdout
        return out

    def origin(self, finished_ago=600, tp=TP, extra=None):
        a = {"glidepath.io/origin-chain-id": BUILD_CHAIN, "glidepath.io/origin-traceparent": tp, "glidepath.io/origin-finished-at": ago(finished_ago),
             "glidepath.io/origin-flow-start-time": BUILD_START}
        a.update(extra or {})
        return a

    def test_a_promotion_within_the_window_joins_the_builds_trace_with_its_own_chain(self):
        r = self.run_seg(self.origin(finished_ago=3 * 3600))
        self.assertEqual(r["traceparent"], TP)
        self.assertEqual(r["trace-joined"], "true")
        self.assertRegex(r["chain-id"], r"^[0-9a-fA-F-]{36}$")
        self.assertNotEqual(r["chain-id"].lower(), BUILD_CHAIN, "the promotion keeps its OWN chain id")
        self.assertNotEqual(r["start-time"], BUILD_START, "the flow start is the promotion's own, not the build's")
        self.assertEqual(r["flow-terminal"], "false", "a joined run never closes the build's root span")
        self.assertIn(TRACE, r["stdout"])

    def test_a_build_older_than_the_window_is_not_joined(self):
        r = self.run_seg(self.origin(finished_ago=13 * 3600))
        self.assertEqual((r["trace-joined"], r["flow-terminal"]), ("false", "true"))
        self.assertNotIn(TRACE, r["traceparent"])
        self.assertIn("more than 12h ago", r["stderr"])

    def test_the_window_is_the_catalogs(self):
        a = self.origin(finished_ago=2 * 3600)
        self.assertEqual(self.run_seg(a, window="1h")["trace-joined"], "false")
        self.assertEqual(self.run_seg(a, window="3h")["trace-joined"], "true")
        self.assertEqual(self.run_seg(a, window="1d")["trace-joined"], "true")

    def test_other_stages_pass_no_window_and_never_join(self):
        self.assertEqual(self.run_seg(self.origin(), window="")["trace-joined"], "false")

    def test_a_run_with_no_origin_starts_its_own_trace(self):
        r = self.run_seg({})
        self.assertEqual((r["trace-joined"], r["flow-terminal"]), ("false", "true"))
        self.assertRegex(r["traceparent"], r"^00-[0-9a-f]{32}-[0-9a-f]{16}-01$")

    def test_a_failed_read_is_never_a_join(self):
        r = self.run_seg(unreadable=True)
        self.assertEqual(r["trace-joined"], "false")
        self.assertIn("could not read this run's annotations", r["stderr"])

    def test_a_malformed_context_or_time_is_never_a_join(self):
        self.assertEqual(self.run_seg(self.origin(tp="00-zz-yy-01"))["trace-joined"], "false")
        bad = self.origin(extra={"glidepath.io/origin-finished-at": "yesterday"})
        r = self.run_seg(bad)
        self.assertEqual(r["trace-joined"], "false")
        self.assertIn("unreadable", r["stderr"])
        gone = self.origin()
        del gone["glidepath.io/origin-finished-at"]
        self.assertEqual(self.run_seg(gone)["trace-joined"], "false")
        self.assertEqual(self.run_seg(self.origin(), window="12 hours")["trace-joined"], "false")
        # a window big enough to overflow the shell's arithmetic is "own trace", not a failed release
        self.assertEqual(self.run_seg(self.origin(), window="99999999999999999999h")["trace-joined"], "false")
        self.assertEqual(self.run_seg(self.origin(extra={"glidepath.io/origin-finished-at": "2026-13-45T99:99:99Z"}))["trace-joined"], "false")

    def test_an_incoming_trace_without_a_chain_is_a_new_chain_in_the_same_trace(self):
        r = self.run_seg({}, window="", tp=TP)
        self.assertEqual((r["traceparent"], r["trace-joined"], r["flow-terminal"]), (TP, "true", "false"))
        self.assertRegex(r["chain-id"], r"^[0-9a-fA-F-]{36}$")

    def test_an_event_chained_stage_is_unchanged(self):
        other = "00-" + "ab" * 16 + "-" + "cd" * 8 + "-01"
        r = self.run_seg(self.origin(), chain="11111111-1111-4111-8111-111111111111", tp=other, terminal="true")
        self.assertEqual((r["chain-id"], r["traceparent"], r["start-time"], r["trace-joined"], r["flow-terminal"]),
                         ("11111111-1111-4111-8111-111111111111", other, "2026-11-15T20:00:00Z", "false", "true"))


class Record(unittest.TestCase):
    """open-release-pr's origin block and the record keys it writes."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        script = TASKS["open-release-pr"]["spec"]["steps"][0]["script"]
        cls.script = script
        start = script.index('origin_chain_id=""; origin_chain_slug=""')
        end = script.index("# The dev-side release record (ADR-0021)")
        put(os.path.join(cls.tmp, "seg.sh"), "set -euo pipefail\n" + script[start:end] + '\nprintf "%s|%s|%s" "${origin_chain_id}" "${origin_chain_slug}" "${origin_trace_id}"\n')
        bin_ = os.path.join(cls.tmp, "bin")
        os.makedirs(bin_)
        put(os.path.join(bin_, "kubectl"), '#!/usr/bin/env bash\n[[ -f "$FX/run.json" ]] || exit 1\ncat "$FX/run.json"\n')
        os.chmod(os.path.join(bin_, "kubectl"), 0o755)

    def read(self, annotations, unreadable=False):
        fx = tempfile.mkdtemp(dir=self.tmp)
        if not unreadable:
            put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": annotations}}))
        env = dict(os.environ, FX=fx, PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], PIPELINE_RUN_NAME="promote-x", NAMESPACE="app-gate-api-cicd")
        p = subprocess.run(["bash", os.path.join(self.tmp, "seg.sh")], capture_output=True, text=True, env=env)
        self.assertEqual(p.returncode, 0, p.stderr)
        return p.stdout.splitlines()[-1], p.stderr

    def test_the_record_carries_the_origin_the_run_was_stamped_with(self):
        out, _ = self.read({"glidepath.io/origin-chain-id": BUILD_CHAIN, "glidepath.io/origin-chain-slug": "ornate-ram", "glidepath.io/origin-trace-id": TRACE})
        self.assertEqual(out, f"{BUILD_CHAIN}|ornate-ram|{TRACE}")

    def test_no_origin_is_empty_fields_not_an_error(self):
        self.assertEqual(self.read({})[0], "||")
        out, err = self.read({}, unreadable=True)
        self.assertEqual(out, "||")
        self.assertIn("could not read this run's annotations", err)

    def test_a_malformed_value_is_dropped_and_a_slug_or_trace_never_stands_alone(self):
        self.assertEqual(self.read({"glidepath.io/origin-chain-id": "not-a-uuid", "glidepath.io/origin-chain-slug": "ornate-ram", "glidepath.io/origin-trace-id": TRACE})[0], "||")
        out, _ = self.read({"glidepath.io/origin-chain-id": BUILD_CHAIN, "glidepath.io/origin-chain-slug": "Bad Slug", "glidepath.io/origin-trace-id": "xyz"})
        self.assertEqual(out, f"{BUILD_CHAIN}||")

    def test_the_record_gets_all_three_keys(self):
        for k, v in (("originChainId", "origin_chain_id"), ("originChainSlug", "origin_chain_slug"), ("originTraceId", "origin_trace_id")):
            self.assertIn(f'--from-literal="{k}=${{{v}}}"', self.script)


if __name__ == "__main__":
    unittest.main()
