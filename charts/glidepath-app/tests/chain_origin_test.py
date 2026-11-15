#!/usr/bin/env python3
"""Pipeline trust T3, chained stages: every Trigger that chains a stage stamps the originating ref and sha on the run it creates
(glidepath.io/origin-ref and origin-sha, from the verified event), so the chained Pipeline's verify-ref-protection verifies the
origin instead of trusting that "the first stage was checked". And nothing chains off a test run that did not succeed (a refused
test run's testcaserun.finished carries outcome Failed, and the from-test Trigger requires Succeeded).

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
            self.assertEqual(ann["glidepath.io/origin-sha"], "$(tt.params.git-revision)", name)
            self.assertIn("origin-ref", [p["name"] for p in templates[name]["spec"]["params"]], name)
            b = {p["name"]: p["value"] for p in bindings[name.replace("-template", "-binding")]["spec"]["params"]}
            self.assertEqual(b["origin-ref"], "$(body.subject.content.origin-ref)", name)
            self.assertEqual(b["git-revision"], "$(body.subject.content.revision)", name)

    def test_nothing_chains_off_a_test_run_that_did_not_succeed(self):
        triggers = BY("Trigger")
        after_test = [t for n, t in triggers.items() if "testcaserun.finished" in yaml.safe_dump(t)]
        self.assertTrue(after_test, "no trigger chains off testcaserun.finished")
        for t in after_test:
            self.assertIn("body.subject.content.outcome == 'Succeeded'", yaml.safe_dump(t))


if __name__ == "__main__":
    unittest.main()
