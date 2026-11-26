#!/usr/bin/env python3
"""No catalog Task renders an env entry with value: "". The API server drops an empty env value when it stores a Task, so a
rendered `value: ""` never matches the live object and Argo CD reports the Task OutOfSync forever (hangar-dev's
clearance-gate with an empty clearanceGate.pipelineLogins, 2026-11-21). An empty value is written as the name alone, which
the container sees as the same empty variable.

Needs helm and pyyaml.
Run:  python3 charts/glidepath-catalog/tests/empty_env_value_test.py
"""
import os
import subprocess
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
EXAMPLE = os.path.join(CATALOG, "tests", "values.example.yaml")


def tasks(*args):
    r = subprocess.run(["helm", "template", "c", CATALOG, "-f", EXAMPLE, *args], capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    return {d["metadata"]["name"]: d for d in yaml.safe_load_all(r.stdout) if d and d.get("kind") == "Task"}


def envs(task):
    spec = task["spec"]
    for c in spec.get("steps", []) + spec.get("sidecars", []) + ([spec["stepTemplate"]] if spec.get("stepTemplate") else []):
        for e in c.get("env") or []:
            yield c.get("name", "stepTemplate"), e


class EmptyEnvValue(unittest.TestCase):
    def assert_no_empty_values(self, ts):
        self.assertGreater(len(ts), 0)
        empty = [(n, s, e["name"]) for n, t in ts.items() for s, e in envs(t) if "value" in e and e["value"] == ""]
        self.assertEqual(empty, [], "Task env entries rendered with value: \"\" (write the name alone)")

    def test_example_values(self):
        self.assert_no_empty_values(tasks())

    def test_clearance_and_logins_unset(self):
        self.assert_no_empty_values(tasks("--set", "agentGates.clearanceUrl=", "--set", "clearanceGate.pipelineLogins=null"))

    def pipeline_logins(self, ts):
        return [e for _, e in envs(ts["clearance-gate-fetch"]) if e["name"] == "PIPELINE_LOGINS"]  # the fetch step's (its own Task)

    def test_pipeline_logins_empty_is_name_only(self):
        found = self.pipeline_logins(tasks())
        self.assertGreater(len(found), 0)
        self.assertTrue(all(e == {"name": "PIPELINE_LOGINS"} for e in found), found)

    def test_pipeline_logins_set_is_joined(self):
        found = self.pipeline_logins(tasks("--set", "clearanceGate.pipelineLogins={ci-bot,release-bot}"))
        self.assertGreater(len(found), 0)
        self.assertTrue(all(e.get("value") == "ci-bot,release-bot" for e in found), found)

    def test_clearance_url_set_and_unset(self):
        def url(ts):
            return [e for _, e in envs(ts["agent-gates"]) if e["name"] == "CLEARANCE_URL"]
        unset = url(tasks("--set", "agentGates.clearanceUrl="))
        self.assertTrue(unset and all(e == {"name": "CLEARANCE_URL"} for e in unset), unset)
        on = url(tasks("--set", "agentGates.clearanceUrl=http://clearance-core.clearance.svc.cluster.local:8080"))
        self.assertTrue(on and all(e.get("value") == "http://clearance-core.clearance.svc.cluster.local:8080" for e in on), on)


if __name__ == "__main__":
    unittest.main()
