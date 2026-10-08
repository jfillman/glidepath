#!/usr/bin/env python3
"""Tests for the environments model (ADR-0019, phase 2).

Two shapes of cicd.yaml describe the same environments:
  old: deploy.lowerEnvironments / upperEnvironments / promotionOrder (removed 2026-10-07; the chart and the
       schema now refuse them)
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
    def test_every_shape_renders_and_the_removed_keys_are_refused(self):
        # The pre-ADR-0019 keys were removed 2026-10-07 (every app migrated, each proven render-identical first).
        # Ignoring them would silently change an app's environments, so the chart refuses them instead.
        for name, steps, old, envs in TWINS:
            with self.subTest(name):
                new = {k: v for k, v in old.items() if k not in ("lowerEnvironments", "upperEnvironments", "promotionOrder")}
                new["environments"] = envs
                rc_new, out_new, err_new = render(cicd(steps, new))
                self.assertEqual(rc_new, 0, err_new[:400])
                self.assertGreater(len(docs(out_new)), 5)
                if any(k in old for k in ("lowerEnvironments", "upperEnvironments", "promotionOrder")):
                    rc_old, _, err_old = render(cicd(steps, old))
                    self.assertNotEqual(rc_old, 0)
                    self.assertIn("was replaced by deploy.environments", err_old)

    def test_cluster_mapped_flight_still_renders_the_registry_rbac(self):
        # The new shape must keep driving the same switches, not just render the same bytes by accident.
        _, steps, _, envs = TWINS[1]
        new = cicd(steps, {"environments": envs})
        rc, out, err = render(new)
        self.assertEqual(rc, 0, err[:400])
        names = {(d["kind"], d["metadata"]["name"]) for d in docs(out)}
        # The Ground env's deploy RBAC now comes from its own Application (ADR-0023 slice 3): the Ground
        # ApplicationSet carries the glidepath-env source, and this chart renders no Role for it.
        self.assertNotIn(("Role", "allow-pipeline-runner-deploy"), names)
        self.assertIn("charts/glidepath-env", out)
        flat = out
        self.assertIn("kind-prod", flat)

    def test_cloud_target_gets_no_deploy_rbac(self):
        _, steps, old, envs = TWINS[5]
        for config in (cicd(steps, {"target": "aws-lambda", "lambda": old["lambda"], "environments": envs}),):
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


class DeployChart(unittest.TestCase):
    """ADR-0023 slice 2: deploy.chart and environments[].chart."""

    PIN = {"targetRevision": "v0.3.200"}
    OWN = {"repoURL": "https://github.com/o/charts", "path": "charts/web", "targetRevision": "v1.0.0"}

    def ok(self, deploy):
        doc = cicd(BUILD_DEPLOY, deploy)
        jsonschema.validate(doc, SCHEMA)
        rc, _, err = render(doc)
        self.assertEqual(rc, 0, err[:400])

    def refused(self, deploy, message):
        doc = cicd(BUILD_DEPLOY, deploy)
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(doc, SCHEMA)
        rc, _, err = render(doc)
        self.assertNotEqual(rc, 0)
        self.assertIn(message, err)

    def test_app_wide_and_per_environment_charts_are_accepted(self):
        self.ok({"chart": self.OWN, "environments": [{"name": "dev", "tier": "ground", "chart": self.PIN}]})
        self.ok({"chart": self.PIN, "environments": [{"name": "dev", "tier": "ground"}]})
        self.ok({"chart": {"repoURL": "oci://ghcr.io/o/charts", "chart": "web", "targetRevision": "1.2.3"}})

    def test_a_cloud_app_has_no_chart(self):
        lam = {"target": "aws-lambda", "lambda": {"functionName": "fn"}}
        rc, _, err = render(cicd(BUILD_DEPLOY, {**lam, "chart": self.PIN}))
        self.assertNotEqual(rc, 0)
        self.assertIn("deploy.chart is set, but deploy.target is aws-lambda", err)
        rc, _, err = render(cicd(BUILD_DEPLOY, {**lam, "environments": [{"name": "dev", "tier": "ground", "chart": self.PIN}]}))
        self.assertNotEqual(rc, 0)
        self.assertIn("environment 'dev' chart is set, but deploy.target is aws-lambda", err)

    def test_path_and_chart_together_are_refused(self):
        self.refused({"chart": {**self.OWN, "chart": "web"}}, "sets both path and chart")

    def test_a_new_source_must_name_its_chart_and_version(self):
        self.refused({"chart": {"repoURL": "https://github.com/o/charts", "targetRevision": "v1"}},
                     "sets repoURL without path or chart")
        self.refused({"chart": {"repoURL": "https://github.com/o/charts", "path": "charts/web"}},
                     "sets repoURL without targetRevision")

    def test_the_schema_refuses_unknown_keys_and_an_empty_block(self):
        for bad in ({"version": "1"}, {}):
            with self.subTest(bad):
                with self.assertRaises(jsonschema.ValidationError):
                    jsonschema.validate(cicd(BUILD_DEPLOY, {"chart": bad}), SCHEMA)


def lower_envs(config):
    rc, out, err = render(config)
    assert rc == 0, err[:400]
    sets = [d for d in yaml.safe_load_all(out) if d and d.get("kind") == "ApplicationSet" and d["metadata"]["name"].endswith("-lower-envs")]
    return sets[0] if sets else None


class LowerEnvsChart(unittest.TestCase):
    """ADR-0023 slice 3: the Ground ApplicationSet renders the chart resolved from cicd.yaml."""

    def chart_of(self, appset):
        return appset["spec"]["template"]["spec"]["sources"][0]

    def test_unset_renders_the_cluster_default_with_no_patch(self):
        a = lower_envs(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "ground"}]}))
        src = self.chart_of(a)
        self.assertEqual((src["repoURL"], src["path"]), ("https://github.com/jfillman/airframe.git", "charts/airframe-application"))
        self.assertNotIn("templatePatch", a["spec"])
        self.assertTrue(a["spec"]["syncPolicy"]["preserveResourcesOnDeletion"])

    def test_deploy_chart_version_pin_keeps_the_default_source(self):
        a = lower_envs(cicd(BUILD_DEPLOY, {"chart": {"targetRevision": "v9.9.9"}, "environments": [{"name": "dev", "tier": "ground"}]}))
        src = self.chart_of(a)
        self.assertEqual((src["targetRevision"], src["path"]), ("v9.9.9", "charts/airframe-application"))

    def test_a_registry_chart_replaces_path(self):
        own = {"repoURL": "oci://ghcr.io/o/charts", "chart": "web", "targetRevision": "1.2.3"}
        src = self.chart_of(lower_envs(cicd(BUILD_DEPLOY, {"chart": own, "environments": [{"name": "dev", "tier": "ground"}]})))
        self.assertEqual(src["chart"], "web")
        self.assertNotIn("path", src)

    def test_an_environment_override_patches_only_that_environment(self):
        a = lower_envs(cicd(BUILD_DEPLOY, {"chart": {"targetRevision": "v2"}, "environments": [
            {"name": "dev", "tier": "ground"},
            {"name": "test", "tier": "ground", "chart": {"targetRevision": "v3-canary"}},
            {"name": "staging", "tier": "flight", "cluster": "kind-prod", "chart": {"targetRevision": "v4"}},
        ]}))
        self.assertEqual(self.chart_of(a)["targetRevision"], "v2")
        patch = a["spec"]["templatePatch"]
        self.assertIn('if eq .envName "test"', patch)
        self.assertIn("targetRevision: v3-canary", patch)
        self.assertNotIn("staging", patch)  # Flight environments are not this ApplicationSet's
        self.assertIn("{}", patch)

    def test_a_cloud_app_has_no_ground_applicationset(self):
        self.assertIsNone(lower_envs(cicd(BUILD_DEPLOY, {"target": "aws-lambda", "lambda": {"functionName": "f"}})))


class DeployRbacMove(unittest.TestCase):
    """ADR-0023 slice 3b: Ground deploy RBAC rides with the environment's Application."""

    def test_same_cluster_flight_keeps_its_rbac_here(self):
        rc, out, err = render(cicd(WITH_RELEASE, {"environments": [{"name": "dev", "tier": "ground"}, {"name": "staging", "tier": "flight"}]}))
        self.assertEqual(rc, 0, err[:400])
        roles = [d for d in docs(out) if d["kind"] == "Role" and d["metadata"]["name"] == "allow-pipeline-runner-deploy"]
        self.assertEqual([r["metadata"]["namespace"] for r in roles], ["app-sample-staging"])

    def test_no_deploy_stage_means_no_rbac_source(self):
        a = lower_envs(cicd([{"stage": "build"}], {"environments": [{"name": "dev", "tier": "ground"}]}))
        self.assertNotIn("charts/glidepath-env", yaml.safe_dump(a))

    def test_rbac_source_names_the_cicd_namespace(self):
        a = lower_envs(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "ground"}]}))
        rbac = [s for s in a["spec"]["template"]["spec"]["sources"] if s.get("path") == "charts/glidepath-env"]
        self.assertEqual(rbac[0]["helm"]["valuesObject"]["cicdNamespace"], "app-sample-cicd")


class Schema(unittest.TestCase):
    def valid(self, doc):
        jsonschema.validate(doc, SCHEMA)

    def invalid(self, doc):
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(doc, SCHEMA)

    def test_new_shape_is_accepted(self):
        self.valid(cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "ground"},
                                                         {"name": "staging", "tier": "flight", "cluster": "kind-prod"}]}))

    def test_old_shape_is_refused(self):
        self.invalid(cicd(WITH_RELEASE, {"lowerEnvironments": ["dev"],
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



TAXONOMY = {"clusterTaxonomy": {"self": "kind-dev", "zones": {"kind-dev": "lower", "kind-prod": "upper", "edge-1": "lower"}}}


def with_taxonomy(identity=IDENTITY):
    return {**identity, **TAXONOMY}


class ClusterTaxonomy(unittest.TestCase):
    """ADR-0024: production environments, registered clusters."""

    def envs(self, *flight):
        return cicd(WITH_RELEASE, {"environments": [{"name": "dev", "tier": "ground"}, *flight]})

    def test_production_on_an_upper_cluster_renders(self):
        code, _, err = render(self.envs({"name": "staging", "tier": "flight", "cluster": "kind-prod", "production": True}), with_taxonomy())
        self.assertEqual(code, 0, err)

    def test_production_on_the_control_plane_cluster_is_refused(self):
        code, _, err = render(self.envs({"name": "staging", "tier": "flight", "production": True}), with_taxonomy())
        self.assertNotEqual(code, 0)
        self.assertIn("must run on an upper cluster", err)

    def test_production_on_a_lower_registered_cluster_is_refused(self):
        code, _, err = render(self.envs({"name": "staging", "tier": "flight", "cluster": "edge-1", "production": True}), with_taxonomy())
        self.assertNotEqual(code, 0)
        self.assertIn("'edge-1', which is zone lower", err)

    def test_production_ground_is_refused(self):
        cfg = cicd(BUILD_DEPLOY, {"environments": [{"name": "dev", "tier": "ground", "production": True}]})
        code, _, err = render(cfg, with_taxonomy())
        self.assertNotEqual(code, 0)
        self.assertIn("is production but tier ground", err)

    def test_unregistered_cluster_is_refused(self):
        code, _, err = render(self.envs({"name": "staging", "tier": "flight", "cluster": "nowhere"}), with_taxonomy())
        self.assertNotEqual(code, 0)
        self.assertIn("not in the cluster registry (known: edge-1, kind-dev, kind-prod)", err)

    def test_without_taxonomy_only_the_tier_rule_applies(self):
        code, _, err = render(self.envs({"name": "staging", "tier": "flight", "cluster": "nowhere", "production": True}))
        self.assertEqual(code, 0, err)

    def test_taxonomy_does_not_change_a_valid_render(self):
        cfg = self.envs({"name": "staging", "tier": "flight", "cluster": "kind-prod"})
        _, plain, _ = render(cfg)
        _, taxed, _ = render(cfg, with_taxonomy())
        self.assertEqual(docs(plain), docs(taxed))

    def test_schema_accepts_production_and_refuses_a_non_boolean(self):
        cfg = self.envs({"name": "staging", "tier": "flight", "cluster": "kind-prod", "production": True})
        jsonschema.validate(cfg, SCHEMA)
        cfg["deploy"]["environments"][1]["production"] = "yes"
        with self.assertRaises(jsonschema.ValidationError):
            jsonschema.validate(cfg, SCHEMA)


if __name__ == "__main__":
    unittest.main(verbosity=2)
