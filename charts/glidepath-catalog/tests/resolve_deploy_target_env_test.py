#!/usr/bin/env python3
"""Per-environment cloud config (ADR-0019, phase 4): resolve-deploy-target merges the deploy step's
environment's override block over the app-level one, so each environment deploys to its own
function / service / Container App. Runs the Task's own jq program, extracted from the rendered Task.
Also runs the whole step (event-chained path) to check PROM-10: with promotion on, a cloud target in a Flight
environment is refused (ADR-0029 phase 6); with promotion off, or on a cluster, it is not.

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


def step_script(*sets):
    args = ["helm", "template", "t", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), "-s", "templates/tasks/resolve-deploy-target.yaml"]
    for kv in sets:
        args += ["--set", kv]
    out = subprocess.run(args, capture_output=True, text=True, check=True).stdout
    return yaml.safe_load(out)["spec"]["steps"][0]["script"]


PROMOTION_ON = ("promotion.enabled=true", "promotion.pinDigest=true", "registries.flight.namespace=flight",
                "promotion.promoter.repo=flight/flight-promoter")


def program():
    script = step_script()
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


def run_step(script, config, env):
    """The whole step, event-chained (config-json is real, so it never reaches the broker). Returns (rc, stderr, target)."""
    with tempfile.TemporaryDirectory() as d:
        script = script.replace("$(results.target.path)", os.path.join(d, "target")).replace("$(results.config-json.path)", os.path.join(d, "config"))
        r = subprocess.run(["bash", "-c", script], env={**os.environ, "CONFIG_JSON": json.dumps(config), "ENV_NAME": env},
                           capture_output=True, text=True)
        target = open(os.path.join(d, "target")).read() if os.path.exists(os.path.join(d, "target")) else ""
        return r.returncode, r.stderr, target


def cloud(target, tier):
    return {"deploy": {"target": target, "environments": [{"name": "dev", "tier": "ground"}, {"name": "prod", "tier": tier}]}}


class CloudFlightRefusal(unittest.TestCase):
    ON = step_script(*PROMOTION_ON)
    OFF = step_script()

    def test_every_cloud_target_in_a_flight_environment_is_refused_with_promotion_on(self):
        for target in ("aws-ecs", "aws-lambda", "azure-container-apps"):
            rc, err, _ = run_step(self.ON, cloud(target, "flight"), "prod")
            self.assertEqual(rc, 1, target)
            self.assertIn("PROM-10", err)
            self.assertIn(target, err)

    def test_a_ground_environment_of_the_same_app_still_deploys(self):
        rc, err, target = run_step(self.ON, cloud("aws-lambda", "flight"), "dev")
        self.assertEqual((rc, target), (0, "aws-lambda"), err)

    def test_a_cluster_target_in_a_flight_environment_is_untouched(self):
        rc, err, target = run_step(self.ON, cloud("k8s-rollout", "flight"), "prod")
        self.assertEqual((rc, target), (0, "k8s-rollout"), err)

    def test_promotion_off_changes_nothing(self):
        self.assertNotIn("refused (PROM-10)", self.OFF)
        rc, err, target = run_step(self.OFF, cloud("aws-ecs", "flight"), "prod")
        self.assertEqual((rc, target), (0, "aws-ecs"), err)

    def test_an_environment_not_in_the_file_is_not_refused(self):
        rc, err, _ = run_step(self.ON, cloud("aws-ecs", "flight"), "staging")
        self.assertEqual(rc, 0, err)


if __name__ == "__main__":
    unittest.main(verbosity=2)
