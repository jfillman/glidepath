#!/usr/bin/env python3
"""Tests for the environments model (ADR-0019, phase 2).

Two shapes of cicd.yaml describe the same environments:
  old: deploy.lowerEnvironments / upperEnvironments / promotionOrder
  new: deploy.environments: [{name, tier, cluster?}]
The chart must render *identically* for a config and its twin, and refuse the combinations the
model does not support. The schema must accept the new shape and refuse mixing the two.

Run:  python3 charts/glidepath-app/tests/envs_model_test.py      (needs helm, pyyaml, jsonschema)
"""
import copy
import json
import os
import subprocess
import sys
import tempfile
import unittest

import jsonschema
import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CHART = os.path.join(ROOT, "charts", "glidepath-app")
SCHEMA = json.load(open(os.path.join(ROOT, "schemas", "cicd.schema.json")))

IDENTITY = {
    "platformIdentity": {
        "appName": "sample",
        "type": "app",
        "gitopsRepoUrl": "https://github.com/o/gitops-sample",
        "appRepoUrl": "https://github.com/o/sample",
        "githubOwner": "o",
        "catalogNamespace": "platform-catalog",
    }
}

BUILD_DEPLOY = [{"stage": "build"}, {"stage": "deploy", "env": "dev"}]
WITH_RELEASE = BUILD_DEPLOY + [{"stage": "release", "env": "staging"}]


def cicd(steps, deploy):
    return {
        "apiVersion": "platform/v1",
        "kind": "PipelineConfig",
        "build": {"agent": "nodejs-22"},
        "pipelines": {"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": steps}},
        "deploy": deploy,
    }


def render(config, identity=IDENTITY):
    with tempfile.TemporaryDirectory() as d:
        paths = []
        for name, doc in (("cicd.yaml", config), ("identity.yaml", identity)):
            p = os.path.join(d, name)
            yaml.safe_dump(doc, open(p, "w"))
            paths += ["-f", p]
        r = subprocess.run(
            ["helm", "template", "sample", CHART, "--namespace", "app-sample-cicd"] + paths,
            capture_output=True, text=True,
        )
    return r.returncode, r.stdout, r.stderr


def docs(out):
    return [d for d in yaml.safe_load_all(out) if d]


# (case name, steps, old deploy block, new environments list)
TWINS = [
    (
        "kubernetes, one Ground env",
        BUILD_DEPLOY,
        {"lowerEnvironments": ["dev"], "promotionOrder": ["dev"]},
        [{"name": "dev", "tier": "ground"}],
    ),
    (
        "kubernetes, Ground + cluster-mapped Flight",
        WITH_RELEASE,
        {"lowerEnvironments": ["dev"], "upperEnvironments": [{"name": "staging", "cluster": "kind-prod"}],
         "promotionOrder": ["dev", "staging"]},
        [{"name": "dev", "tier": "ground"}, {"name": "staging", "tier": "flight", "cluster": "kind-prod"}],
    ),
    (
        "kubernetes, Flight on the app's own cluster (plain-name form)",
        WITH_RELEASE,
        {"lowerEnvironments": ["dev"], "upperEnvironments": ["staging"], "promotionOrder": ["dev", "staging"]},
        [{"name": "dev", "tier": "ground"}, {"name": "staging", "tier": "flight"}],
    ),
    (
        "kubernetes, two Ground envs and two Flight envs",
        WITH_RELEASE,
        {"lowerEnvironments": ["dev", "test"],
         "upperEnvironments": [{"name": "staging", "cluster": "kind-prod"}, {"name": "prod", "cluster": "kind-prod"}],
         "promotionOrder": ["dev", "test", "staging", "prod"]},
        [{"name": "dev", "tier": "ground"}, {"name": "test", "tier": "ground"},
         {"name": "staging", "tier": "flight", "cluster": "kind-prod"}, {"name": "prod", "tier": "flight", "cluster": "kind-prod"}],
    ),
    (
        "kubernetes, no environments declared (the default Ground dev)",
        BUILD_DEPLOY,
        {},
        [{"name": "dev", "tier": "ground"}],
    ),
    (
        "aws-lambda, one Ground env",
        BUILD_DEPLOY,
        {"target": "aws-lambda", "lambda": {"functionName": "fn", "region": "us-east-1"},
         "lowerEnvironments": ["dev"], "promotionOrder": ["dev"]},
        [{"name": "dev", "tier": "ground"}],
    ),
]


class RenderEquivalence(unittest.TestCase):
    def test_old_and_new_shape_render_identically(self):
        for name, steps, old, envs in TWINS:
            with self.subTest(name):
                new = {k: v for k, v in old.items() if k not in ("lowerEnvironments", "upperEnvironments", "promotionOrder")}
                new["environments"] = envs
                rc_old, out_old, err_old = render(cicd(steps, old))
                rc_new, out_new, err_new = render(cicd(steps, new))
                self.assertEqual(rc_old, 0, err_old[:400])
                self.assertEqual(rc_new, 0, err_new[:400])
                self.assertGreater(len(docs(out_old)), 5)  # it rendered a real chart, not nothing
                self.assertEqual(out_old, out_new)

    def test_cluster_mapped_flight_still_renders_the_registry_rbac(self):
        # The new shape must keep driving the same switches, not just render the same bytes by accident.
        _, steps, _, envs = TWINS[1]
        new = cicd(steps, {"environments": envs})
        rc, out, err = render(new)
        self.assertEqual(rc, 0, err[:400])
        names = {(d["kind"], d["metadata"]["name"]) for d in docs(out)}
        self.assertIn(("Role", "allow-pipeline-runner-deploy"), names)  # the Ground env still gets its RBAC
        flat = out
        self.assertIn("kind-prod", flat)

    def test_cloud_target_gets_no_deploy_rbac_in_either_shape(self):
        _, steps, old, envs = TWINS[5]
        for config in (cicd(steps, old), cicd(steps, {"target": "aws-lambda", "lambda": old["lambda"], "environments": envs})):
            rc, out, err = render(config)
            self.assertEqual(rc, 0, err[:400])
            self.assertFalse([d for d in docs(out) if d["metadata"].get("name") == "allow-pipeline-runner-deploy"])


class Validation(unittest.TestCase):
    def fails(self, environments, steps=BUILD_DEPLOY, extra=None):
        deploy = {"environments": environments}
        deploy.update(extra or {})
        rc, _, err = render(cicd(steps, deploy))
        self.assertNotEqual(rc, 0, "expected the chart to refuse this")
        return err

    def test_duplicate_names(self):
        self.assertIn("listed twice", self.fails([{"name": "dev", "tier": "ground"}, {"name": "dev", "tier": "ground"}]))

    def test_bad_name(self):
        self.assertIn("not a valid environment name", self.fails([{"name": "Dev_1", "tier": "ground"}]))

    def test_bad_tier(self):
        self.assertIn("expected ground or flight", self.fails([{"name": "dev", "tier": "lower"}]))

    def test_ground_with_cluster_is_not_supported_yet(self):
        self.assertIn("not supported yet", self.fails([{"name": "dev", "tier": "ground", "cluster": "kind-other"}]))

    def test_cloud_target_allows_flight_without_kubernetes_artifacts(self):
        # ADR-0020: a cloud Flight environment is approved by a release pin PR on the source repo.
        envs = [{"name": "dev", "tier": "ground"}, {"name": "prod", "tier": "flight"}]
        rc, out, err = render(cicd(BUILD_DEPLOY, {"environments": envs, "target": "aws-lambda", "lambda": {"functionName": "f"}}))
        self.assertEqual(rc, 0, err)
        docs = [d for d in yaml.safe_load_all(out) if d]
        self.assertFalse([d for d in docs if d["kind"] == "Application"], "no Argo CD Application for a cloud environment")
        self.assertFalse([d for d in docs if d["kind"] in ("Role", "RoleBinding") and d["metadata"].get("namespace", "").endswith("-prod")])

    def test_cloud_flight_refuses_a_cluster(self):
        err = self.fails(
            [{"name": "dev", "tier": "ground"}, {"name": "prod", "tier": "flight", "cluster": "kind-prod"}],
            extra={"target": "aws-lambda", "lambda": {"functionName": "f"}},
        )
        self.assertIn("has no cluster", err)

    def test_a_deploy_step_for_an_undeclared_env_still_fails_on_kubernetes(self):
        rc, _, err = render(cicd(BUILD_DEPLOY, {"environments": [{"name": "qa", "tier": "ground"}]}))
        self.assertNotEqual(rc, 0)
        self.assertIn("is not listed under", err)


class PerEnvironmentCloudConfig(unittest.TestCase):
    """Phase 4: an environment may override fields of the app's cloud target block."""

    LAMBDA = {"target": "aws-lambda", "lambda": {"functionName": "fn", "region": "us-east-1"}}

    def test_an_override_for_the_apps_own_target_is_accepted(self):
        deploy = {**self.LAMBDA, "environments": [
            {"name": "dev", "tier": "ground"},
            {"name": "test", "tier": "ground", "lambda": {"functionName": "fn-test"}},
        ]}
        rc, _, err = render(cicd(BUILD_DEPLOY, deploy))
        self.assertEqual(rc, 0, err[:400])

    def test_an_override_for_another_target_is_refused(self):
        deploy = {**self.LAMBDA, "environments": [{"name": "dev", "tier": "ground", "ecs": {"service": "s"}}]}
        rc, _, err = render(cicd(BUILD_DEPLOY, deploy))
        self.assertNotEqual(rc, 0)
        self.assertIn("sets ecs, but deploy.target is aws-lambda", err)

    def test_a_kubernetes_app_has_no_cloud_override(self):
        deploy = {"environments": [{"name": "dev", "tier": "ground", "lambda": {"functionName": "f"}}]}
        rc, _, err = render(cicd(BUILD_DEPLOY, deploy))
        self.assertNotEqual(rc, 0)
        self.assertIn("deploy.target is k8s-rollout", err)

    def test_the_schema_accepts_partial_overrides_and_refuses_unknown_keys(self):
        ok = cicd(BUILD_DEPLOY, {**self.LAMBDA, "environments": [{"name": "t", "tier": "ground", "lambda": {"region": "eu-west-1"}}]})
        jsonschema.validate(ok, SCHEMA)
        bad = cicd(BUILD_DEPLOY, {**self.LAMBDA, "environments": [{"name": "t", "tier": "ground", "lambda": {"functionname": "x"}}]})
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(bad, SCHEMA)


class Schema(unittest.TestCase):
    def valid(self, doc):
        jsonschema.validate(doc, SCHEMA)

    def invalid(self, doc):
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(doc, SCHEMA)

    def test_new_shape_is_accepted(self):
        self.valid(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "ground"},
                                                         {"name": "staging", "tier": "flight", "cluster": "kind-prod"}]}))

    def test_old_shape_is_still_accepted(self):
        self.valid(cicd(WITH_RELEASE, {"lowerEnvironments": ["dev"],
                                       "upperEnvironments": [{"name": "staging", "cluster": "kind-prod"}],
                                       "promotionOrder": ["dev", "staging"]}))

    def test_mixing_the_shapes_is_refused(self):
        envs = [{"name": "dev", "tier": "ground"}]
        for old in ({"lowerEnvironments": ["dev"]}, {"upperEnvironments": ["staging"]}, {"promotionOrder": ["dev"]}):
            with self.subTest(old):
                self.invalid(cicd(BUILD_DEPLOY, {"environments": envs, **old}))

    def test_entry_needs_name_and_tier(self):
        self.invalid(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev"}]}))
        self.invalid(cicd(BUILD_DEPLOY, {"environments": [{"tier": "ground"}]}))

    def test_bad_tier_name_and_unknown_keys(self):
        self.invalid(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "middle"}]}))
        self.invalid(cicd(BUILD_DEPLOY, {"environments": [{"name": "Dev", "tier": "ground"}]}))
        self.invalid(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "ground", "approval": "pr"}]}))

    def test_empty_list_is_refused(self):
        self.invalid(cicd(BUILD_DEPLOY, {"environments": []}))


if __name__ == "__main__":
    unittest.main(verbosity=2)
