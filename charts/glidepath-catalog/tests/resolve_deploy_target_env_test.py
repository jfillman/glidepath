#!/usr/bin/env python3
"""Per-environment cloud config (ADR-0019, phase 4): resolve-deploy-target merges the deploy step's
environment's override block over the app-level one, so each environment deploys to its own
function / service / Container App. Runs the Task's own jq program, extracted from the rendered Task.

Run:  python3 charts/glidepath-catalog/tests/resolve_deploy_target_env_test.py   (needs helm, jq, pyyaml)
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


def program():
    out = subprocess.run(
        ["helm", "template", "t", CATALOG, "-s", "templates/tasks/resolve-deploy-target.yaml"],
        capture_output=True, text=True, check=True,
    ).stdout
    script = yaml.safe_load(out)["spec"]["steps"][0]["script"]
    m = re.search(r"<<'JQ_ENV_OVERRIDES'\n(.*?)\nJQ_ENV_OVERRIDES\n", script, re.S)
    assert m, "override program not found in the Task"
    return m.group(1)


PROGRAM = program()


def resolve(config, env):
    with tempfile.TemporaryDirectory() as d:
        pf = os.path.join(d, "p.jq")
        open(pf, "w").write(PROGRAM)
        r = subprocess.run(
            ["jq", "-c", "--arg", "env", env, "-f", pf],
            input=json.dumps(config), capture_output=True, text=True, check=True,
        )
    return json.loads(r.stdout)


LAMBDA = {
    "deploy": {
        "target": "aws-lambda",
        "lambda": {"functionName": "app-fn", "region": "us-east-1"},
        "environments": [
            {"name": "dev", "tier": "ground"},
            {"name": "test", "tier": "ground", "lambda": {"functionName": "app-fn-test"}},
            {"name": "eu", "tier": "ground", "lambda": {"functionName": "app-fn-eu", "region": "eu-west-1"}},
        ],
    }
}


class EnvOverrides(unittest.TestCase):
    def test_an_environment_with_an_override_gets_its_own_function(self):
        self.assertEqual(resolve(LAMBDA, "test")["deploy"]["lambda"], {"functionName": "app-fn-test", "region": "us-east-1"})

    def test_fields_the_override_does_not_set_come_from_the_app_level_block(self):
        # region was not overridden for "test", so it is the app-level one
        self.assertEqual(resolve(LAMBDA, "test")["deploy"]["lambda"]["region"], "us-east-1")

    def test_an_override_can_change_more_than_one_field(self):
        self.assertEqual(resolve(LAMBDA, "eu")["deploy"]["lambda"], {"functionName": "app-fn-eu", "region": "eu-west-1"})

    def test_an_environment_without_an_override_uses_the_app_level_block(self):
        self.assertEqual(resolve(LAMBDA, "dev"), LAMBDA)

    def test_an_unknown_or_empty_environment_changes_nothing(self):
        self.assertEqual(resolve(LAMBDA, "nope"), LAMBDA)
        self.assertEqual(resolve(LAMBDA, ""), LAMBDA)

    def test_a_file_without_deploy_environments_is_unchanged(self):
        old = {"deploy": {"target": "aws-lambda", "lambda": {"functionName": "f"}, "lowerEnvironments": ["dev"]}}
        self.assertEqual(resolve(old, "dev"), old)
        self.assertEqual(resolve({}, "dev"), {})

    def test_ecs_overrides_merge_the_same_way(self):
        cfg = {"deploy": {"target": "aws-ecs", "ecs": {"cluster": "c", "service": "s", "region": "us-east-1"},
                          "environments": [{"name": "prod", "tier": "ground", "ecs": {"service": "s-prod"}}]}}
        self.assertEqual(resolve(cfg, "prod")["deploy"]["ecs"], {"cluster": "c", "service": "s-prod", "region": "us-east-1"})

    def test_azure_overrides_merge_the_same_way(self):
        cfg = {"deploy": {"target": "azure-container-apps", "azureContainerApps": {"resourceGroup": "rg", "appName": "a"},
                          "environments": [{"name": "test", "tier": "ground", "azureContainerApps": {"appName": "a-test"}}]}}
        self.assertEqual(resolve(cfg, "test")["deploy"]["azureContainerApps"], {"resourceGroup": "rg", "appName": "a-test"})

    def test_an_override_with_no_app_level_block_still_works(self):
        cfg = {"deploy": {"target": "aws-lambda", "environments": [{"name": "dev", "tier": "ground", "lambda": {"functionName": "only-here"}}]}}
        self.assertEqual(resolve(cfg, "dev")["deploy"]["lambda"], {"functionName": "only-here"})

    def test_the_other_environments_blocks_are_never_applied(self):
        # "dev" has no override: it must not pick up "test"'s function
        self.assertEqual(resolve(LAMBDA, "dev")["deploy"]["lambda"]["functionName"], "app-fn")


if __name__ == "__main__":
    unittest.main(verbosity=2)
