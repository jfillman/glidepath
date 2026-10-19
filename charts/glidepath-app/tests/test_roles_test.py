#!/usr/bin/env python3
"""Test-step roles (docs/admin/testing.md): the shapes validateFlows must refuse, and what the
trigger carries for the ones it accepts. Renders glidepath-app the way examples_test.py does.
Needs helm and pyyaml."""
import os
import subprocess
import sys
import tempfile
import unittest

import yaml

CHART = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FIXTURE = os.path.join(CHART, "tests", "fixtures", "control-plane-values.yaml")

BASE = {
    "apiVersion": "platform/v1", "kind": "PipelineConfig",
    "build": {"agent": "nodejs-20"},
    "test": {"name": "integration"},
    "deploy": {"environments": [{"name": "dev", "tier": "ground"}]},
}


def render(steps):
    cfg = dict(BASE, pipelines={"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": steps}})
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(cfg, f)
        path = f.name
    try:
        p = subprocess.run(["helm", "template", "x", CHART, "--namespace", "app-x-cicd", "-f", path, "-f", FIXTURE],
                           capture_output=True, text=True)
    finally:
        os.unlink(path)
    return p.returncode, p.stdout, p.stderr


class Refused(unittest.TestCase):
    def refuses(self, steps, message):
        code, _, err = render(steps)
        self.assertNotEqual(code, 0, "expected the chart to refuse this flow")
        self.assertIn(message, err)

    def test_check_ride_has_no_test_name(self):
        self.refuses([{"stage": "build"}, {"stage": "test", "env": "dev", "role": "check-ride", "testName": "x"}],
                     "a check-ride step takes no testName")

    def test_one_check_ride_per_flow(self):
        self.refuses([{"stage": "build"}, {"stage": "test", "env": "dev", "role": "check-ride"},
                      {"stage": "test", "env": "dev", "role": "check-ride"}],
                     "at most one check-ride step")

    def test_check_ride_is_always_required(self):
        self.refuses([{"stage": "build"}, {"stage": "test", "env": "dev", "role": "check-ride", "required": False}],
                     "a check-ride step is always required")

    def test_role_fields_only_on_test_steps(self):
        self.refuses([{"stage": "build"}, {"stage": "deploy", "env": "dev", "required": False}],
                     "`required` is only valid on a test step")

    def test_timeout_shape(self):
        self.refuses([{"stage": "build"}, {"stage": "test", "env": "dev", "timeout": "20minutes"}],
                     "must be a number followed by s, m or h")

    def test_unknown_role(self):
        self.refuses([{"stage": "build"}, {"stage": "test", "env": "dev", "role": "smoke"}],
                     "is not one of execute, test, check-ride")


class Accepted(unittest.TestCase):
    def test_trigger_carries_role_required_timeout(self):
        code, out, err = render([
            {"stage": "build"},
            {"stage": "test", "env": "dev", "role": "execute", "required": False, "timeout": "2m"},
            {"stage": "test", "env": "dev", "role": "check-ride", "timeout": "20m"},
        ])
        self.assertEqual(code, 0, err)
        bindings = [d for d in yaml.safe_load_all(out) if d and d.get("kind") == "TriggerBinding"]
        got = []
        for b in bindings:
            params = {p["name"]: p["value"] for p in b["spec"]["params"]}
            if params.get("stage-name") == "test":
                got.append((params["test-role"], params["test-required"], params["test-timeout"]))
        self.assertEqual(sorted(got), [("check-ride", "true", "20m"), ("execute", "false", "2m")])
        # The default: a plain test step is role test, required, 10m.
        code, out, _ = render([{"stage": "build"}, {"stage": "test", "env": "dev"}])
        self.assertEqual(code, 0)
        b = [d for d in yaml.safe_load_all(out) if d and d.get("kind") == "TriggerBinding"
             and any(p["name"] == "stage-name" and p["value"] == "test" for p in d["spec"]["params"])][0]
        params = {p["name"]: p["value"] for p in b["spec"]["params"]}
        self.assertEqual((params["test-role"], params["test-required"], params["test-timeout"]), ("test", "true", "10m"))


if __name__ == "__main__":
    unittest.main(argv=[sys.argv[0]])
