#!/usr/bin/env python3
"""Pipelines-as-Code's per-repository queue (docs/admin/pipeline-performance.md, "Queueing gate runs"): pipelinesAsCode.concurrencyLimit
sets spec.concurrency_limit on this app's Repository CRs (the app repo and gitops-<app>). Unset renders the CRs exactly as before
(no field), a set value reaches both, the per-repository overrides win, extra gitops repos are never limited, and a value that is
not a whole number of at least 1 fails the render.

Run:  python3 charts/glidepath-app/tests/pac_concurrency_test.py     (needs helm, pyyaml)
"""
import os
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
APP = os.path.join(ROOT, "charts", "glidepath-app")
CICD = {"apiVersion": "platform/v1", "kind": "PipelineConfig", "build": {"agent": "nodejs-20"},
        "deploy": {"environments": [{"name": "dev", "tier": "ground"}]}}
EXTRA = "https://github.com/example-org/gitops-cluster-shared"


def render(*extra, values=None):
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(CICD, f)
        path = f.name
    try:
        args = ["helm", "template", "x", APP, "--namespace", "app-x-cicd", "-f", path, "-f",
                os.path.join(APP, "tests", "fixtures", "control-plane-values.yaml"), *extra]
        p = subprocess.run(args, capture_output=True, text=True)
        return p
    finally:
        os.unlink(path)


def repos(*extra):
    p = render(*extra)
    assert p.returncode == 0, p.stderr
    return {d["metadata"]["name"]: d["spec"] for d in yaml.safe_load_all(p.stdout) if d and d["kind"] == "Repository"}


class PacConcurrency(unittest.TestCase):
    def test_unset_renders_no_limit_and_nothing_else_changes(self):
        got = repos()
        self.assertEqual(len(got), 2, got)  # the app repo and gitops-<app>
        for name, spec in got.items():
            self.assertEqual(list(spec), ["url"], name)

    def test_one_value_limits_both_repositories(self):
        got = repos("--set", "pipelinesAsCode.concurrencyLimit=3")
        self.assertEqual(sorted(s["concurrency_limit"] for s in got.values()), [3, 3])
        for spec in got.values():
            self.assertIsInstance(spec["concurrency_limit"], int)

    def test_a_per_repository_value_wins_for_that_repository_only(self):
        got = repos("--set", "pipelinesAsCode.concurrencyLimit=3", "--set", "pipelinesAsCode.gitopsRepoConcurrencyLimit=5")
        gitops = [s for n, s in got.items() if n.startswith("gitops-")]
        app = [s for n, s in got.items() if not n.startswith("gitops-")]
        self.assertEqual([s["concurrency_limit"] for s in gitops], [5])
        self.assertEqual([s["concurrency_limit"] for s in app], [3])

    def test_only_one_repository_can_be_limited(self):
        got = repos("--set", "pipelinesAsCode.appRepoConcurrencyLimit=2")
        self.assertEqual({n.startswith("gitops-"): "concurrency_limit" in s for n, s in got.items()}, {False: True, True: False})

    def test_extra_gitops_repositories_are_not_limited(self):
        got = repos("--set", "pipelinesAsCode.concurrencyLimit=3", "--set", "platformIdentity.extraGitopsRepos={%s}" % EXTRA)
        self.assertEqual(len(got), 3, got)
        self.assertNotIn("concurrency_limit", got["gitops-cluster-shared"])

    def test_a_value_that_is_not_a_whole_number_of_at_least_one_fails(self):
        for bad in ("0", "-1", "2.5", "three"):
            p = render("--set-string" if bad == "three" else "--set", "pipelinesAsCode.concurrencyLimit=" + bad)
            self.assertNotEqual(p.returncode, 0, bad)
            self.assertIn("whole number of at least 1", p.stderr, bad)


if __name__ == "__main__":
    unittest.main()
