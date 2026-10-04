#!/usr/bin/env python3
"""validate-cicd-config must hand every Task a config-json whose environment fields agree (ADR-0019).

The Task fills in schema defaults, which adds lowerEnvironments: [dev], upperEnvironments: [] and
promotionOrder: [] even for a file that declares deploy.environments. The normalization step
that follows has to make the list and the three older fields consistent, whichever shape the
developer wrote. This runs the Task's own jq programs (extracted from the rendered Task), not a copy.

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
        ["helm", "template", "t", CATALOG, "-s", "templates/tasks/validate-cicd-config.yaml"],
        capture_output=True, text=True, check=True,
    ).stdout
    return yaml.safe_load(out)["spec"]["steps"][0]["script"]


def heredoc(script, tag):
    m = re.search(r"<<'%s'\n(.*?)\n%s\n" % (tag, tag), script, re.S)
    assert m, f"heredoc {tag} not found in the Task"
    return m.group(1)


SCRIPT = task_script()
APPLY = heredoc(SCRIPT, "JQ_PROGRAM")
NORMALIZE = heredoc(SCRIPT, "JQ_NORMALIZE")


def config_json(user_doc):
    """What validate-cicd-config writes to its config-json result for this cicd.yaml (as JSON)."""
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


class ConfigJsonEnvironments(unittest.TestCase):
    def test_new_shape_carries_consistent_old_fields(self):
        d = config_json({**BASE, "deploy": {"environments": [
            {"name": "dev", "tier": "ground"},
            {"name": "test", "tier": "ground"},
            {"name": "staging", "tier": "flight", "cluster": "kind-prod"},
            {"name": "prod", "tier": "flight"},
        ]}})
        self.assertEqual(d["lowerEnvironments"], ["dev", "test"])  # not the schema default ["dev"]
        self.assertEqual(d["upperEnvironments"], [{"name": "staging", "cluster": "kind-prod"}, "prod"])
        self.assertEqual(d["promotionOrder"], ["dev", "test", "staging", "prod"])
        self.assertEqual([e["name"] for e in d["environments"]], ["dev", "test", "staging", "prod"])

    def test_old_shape_gains_the_equivalent_list(self):
        d = config_json({**BASE, "deploy": {
            "lowerEnvironments": ["dev", "test"],
            "upperEnvironments": [{"name": "staging", "cluster": "kind-prod"}, "prod"],
            "promotionOrder": ["dev", "test", "staging", "prod"],
        }})
        self.assertEqual(d["environments"], [
            {"name": "dev", "tier": "ground"},
            {"name": "test", "tier": "ground"},
            {"name": "staging", "tier": "flight", "cluster": "kind-prod"},
            {"name": "prod", "tier": "flight"},
        ])
        self.assertEqual(d["lowerEnvironments"], ["dev", "test"])  # untouched
        self.assertEqual(d["promotionOrder"], ["dev", "test", "staging", "prod"])

    def test_both_shapes_of_the_same_environments_agree_on_everything(self):
        old = config_json({**BASE, "deploy": {
            "lowerEnvironments": ["dev"], "upperEnvironments": [{"name": "staging", "cluster": "kind-prod"}],
            "promotionOrder": ["dev", "staging"]}})
        new = config_json({**BASE, "deploy": {"environments": [
            {"name": "dev", "tier": "ground"}, {"name": "staging", "tier": "flight", "cluster": "kind-prod"}]}})
        for key in ("environments", "lowerEnvironments", "upperEnvironments", "promotionOrder"):
            self.assertEqual(old[key], new[key], key)

    def test_nothing_declared_means_the_default_ground_dev(self):
        d = config_json(BASE)  # no deploy block at all
        self.assertEqual(d["environments"], [{"name": "dev", "tier": "ground"}])
        self.assertEqual(d["lowerEnvironments"], ["dev"])

    def test_a_cloud_app_keeps_its_target(self):
        d = config_json({**BASE, "deploy": {"target": "aws-lambda", "lambda": {"functionName": "f"},
                                            "environments": [{"name": "dev", "tier": "ground"}]}})
        self.assertEqual(d["target"], "aws-lambda")
        self.assertEqual(d["lambda"]["functionName"], "f")


if __name__ == "__main__":
    unittest.main(verbosity=2)
