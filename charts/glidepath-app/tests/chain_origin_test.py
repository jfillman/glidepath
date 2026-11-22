#!/usr/bin/env python3
"""Pipeline trust T3, chained stages: every Trigger that chains a stage stamps the originating ref and sha on the run it creates
(glidepath.io/origin-ref and origin-sha, from the verified event), so the chained Pipeline's verify-ref-protection verifies the
origin instead of trusting that "the first stage was checked". And nothing chains off a test run that did not succeed (a refused
test run's testcaserun.finished carries outcome Failed, and the from-test Trigger requires Succeeded or Completed; Tekton reports Completed when a task was skipped by a when-expression).

Run:  python3 charts/glidepath-app/tests/chain_origin_test.py     (needs helm, pyyaml)
"""
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
APP = os.path.join(ROOT, "charts", "glidepath-app")
CICD = {
    "apiVersion": "platform/v1", "kind": "PipelineConfig", "build": {"agent": "nodejs-20"}, "test": {"name": "integration"},
    "deploy": {"environments": [{"name": "dev", "tier": "ground"}, {"name": "prod", "tier": "flight"}]},
    "pipelines": {"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": [
        {"stage": "build"}, {"stage": "test", "env": "dev", "testName": "integration"},
        {"stage": "deploy", "env": "dev"}, {"stage": "release", "env": "prod"}]}},
}


def render():
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(CICD, f)
        path = f.name
    try:
        p = subprocess.run(["helm", "template", "x", APP, "--namespace", "app-x-cicd", "-f", path, "-f",
                            os.path.join(APP, "tests", "fixtures", "control-plane-values.yaml")], capture_output=True, text=True)
        assert p.returncode == 0, p.stderr
        return [d for d in yaml.safe_load_all(p.stdout) if d]
    finally:
        os.unlink(path)


DOCS = render()
BY = lambda kind: {d["metadata"]["name"]: d for d in DOCS if d["kind"] == kind}  # noqa: E731


class ChainOrigin(unittest.TestCase):
    def test_every_chained_stage_stamps_its_origin(self):
        templates, bindings = BY("TriggerTemplate"), BY("TriggerBinding")
        flow = [n for n in templates if n.startswith("flow-")]
        self.assertEqual(len(flow), 3, flow)  # test, deploy, release are chained; build is git-rooted
        for name in flow:
            run = templates[name]["spec"]["resourcetemplates"][0]
            ann = run["metadata"]["annotations"]
            self.assertEqual(ann["glidepath.io/origin-ref"], "$(tt.params.origin-ref)", name)
            # the origin sha is the event's own origin-sha, not the revision the run is started for: the guard compares the two
            self.assertEqual(ann["glidepath.io/origin-sha"], "$(tt.params.origin-sha)", name)
            self.assertIn("origin-sha", [p["name"] for p in templates[name]["spec"]["params"]], name)
            self.assertIn("origin-ref", [p["name"] for p in templates[name]["spec"]["params"]], name)
            b = {p["name"]: p["value"] for p in bindings[name.replace("-template", "-binding")]["spec"]["params"]}
            self.assertEqual(b["origin-ref"], "$(body.subject.content.origin-ref)", name)
            self.assertEqual(b["origin-sha"], "$(body.subject.content.origin-sha)", name)
            self.assertEqual(b["git-revision"], "$(body.subject.content.revision)", name)

    def test_nothing_chains_off_a_test_run_that_did_not_succeed(self):
        triggers = BY("Trigger")
        after_test = [t for n, t in triggers.items() if "testcaserun.finished" in yaml.safe_dump(t)]
        self.assertTrue(after_test, "no trigger chains off testcaserun.finished")
        for t in after_test:
            # Succeeded AND Completed (Tekton's $(tasks.status) when a when-expression skipped a task) chain; Failed does not
            self.assertIn("body.subject.content.outcome in ['Succeeded', 'Completed']", yaml.safe_dump(t))

    def test_a_failed_test_outcome_still_does_not_chain(self):
        triggers = BY("Trigger")
        after_test = [t for t in triggers.values() if "testcaserun.finished" in yaml.safe_dump(t)]
        self.assertTrue(after_test, "no trigger chains off testcaserun.finished")
        for t in after_test:
            dump = yaml.safe_dump(t)
            # the only outcome the filter names are the two success values; nothing admits Failed or an unconditional match
            self.assertEqual(re.findall(r"outcome (?:==|in) (\[[^\]]*\]|'[^']*')", dump), ["['Succeeded', 'Completed']"])
            self.assertNotIn("Failed", dump.split("body.subject.content.outcome")[1].split("&&")[0])


if __name__ == "__main__":
    unittest.main()
