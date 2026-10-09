#!/usr/bin/env python3
"""stage-preflight's prepare step must hand every Task a config-json whose environment fields agree (ADR-0019).

deploy.environments is passed through as declared, or defaults to one Ground environment, dev. The
pre-ADR-0019 fields (lowerEnvironments, upperEnvironments, promotionOrder) were removed 2026-10-07 and
are never filled in. This runs the Task's own jq programs (extracted from the rendered Task), not a copy.

Run:  python3 charts/glidepath-catalog/tests/validate_config_envs_test.py   (needs helm, jq, pyyaml)
"""
import json
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
SCHEMA = open(os.path.join(ROOT, "schemas", "cicd.schema.json")).read()


def task_script():
    out = subprocess.run(
        ["helm", "template", "t", CATALOG, "-s", "templates/tasks/stage-preflight.yaml"],
        capture_output=True, text=True, check=True,
    ).stdout
    steps = yaml.safe_load(out)["spec"]["steps"]
    return [s for s in steps if s["name"] == "prepare"][0]["script"]


def heredoc(script, tag):
    m = re.search(r"<<'%s'\n(.*?)\n%s\n" % (tag, tag), script, re.S)
    assert m, f"heredoc {tag} not found in the Task"
    return m.group(1)


SCRIPT = task_script()
APPLY = heredoc(SCRIPT, "JQ_PROGRAM")
NORMALIZE = heredoc(SCRIPT, "JQ_NORMALIZE")


def config_json(user_doc):
    """What stage-preflight's prepare step writes to its config-json result for this cicd.yaml (as JSON)."""
    with tempfile.TemporaryDirectory() as d:
        paths = {}
        for name, text in (("apply.jq", APPLY), ("normalize.jq", NORMALIZE), ("orig.json", json.dumps(user_doc))):
            paths[name] = os.path.join(d, name)
            open(paths[name], "w").write(text)
        defaulted = subprocess.run(
            ["jq", "--argjson", "schema", SCHEMA, "-f", paths["apply.jq"], paths["orig.json"]],
            capture_output=True, text=True, check=True,
        ).stdout
        dpath = os.path.join(d, "defaulted.json")
        open(dpath, "w").write(defaulted)
        out = subprocess.run(
            ["jq", "--argjson", "orig", json.dumps(user_doc), "-f", paths["normalize.jq"], dpath],
            capture_output=True, text=True, check=True,
        ).stdout
    return json.loads(out)["deploy"]


BASE = {"apiVersion": "platform/v1", "kind": "PipelineConfig", "build": {"agent": "nodejs-22"}}
OLD_KEYS = ("lowerEnvironments", "upperEnvironments", "promotionOrder")


class ConfigJsonEnvironments(unittest.TestCase):
    def test_the_declared_list_is_passed_through(self):
        envs = [{"name": "dev", "tier": "ground"}, {"name": "staging", "tier": "flight", "cluster": "kind-prod"}]
        d = config_json({**BASE, "deploy": {"environments": envs}})
        self.assertEqual(d["environments"], envs)

    def test_nothing_declared_means_the_default_ground_dev(self):
        d = config_json(BASE)  # no deploy block at all
        self.assertEqual(d["environments"], [{"name": "dev", "tier": "ground"}])

    def test_the_removed_fields_are_not_filled_in(self):
        # The schema no longer declares them, so apply-defaults has nothing to add (removed 2026-10-07).
        d = config_json({**BASE, "deploy": {"environments": [{"name": "dev", "tier": "ground"}]}})
        for key in OLD_KEYS:
            self.assertNotIn(key, d)

    def test_a_cloud_app_keeps_its_target(self):
        d = config_json({**BASE, "deploy": {"target": "aws-lambda", "lambda": {"functionName": "f"},
                                            "environments": [{"name": "dev", "tier": "ground"}]}})
        self.assertEqual(d["target"], "aws-lambda")
        self.assertEqual(d["lambda"]["functionName"], "f")


if __name__ == "__main__":
    unittest.main(verbosity=2)
