#!/usr/bin/env python3
"""Where Pipelines-as-Code reads a gitops repository's .tekton files from (docs/admin/security.md, "A pull request's .tekton/ file is
source too"): pipelinesAsCode.gitopsRepoProvenance sets spec.settings.pipelinerun_provenance on the gitops-<app> Repository CR only.
Unset renders the CRs exactly as before (no settings block), the app repository and extra gitops repositories are never changed,
a value Pipelines-as-Code's CRD does not know fails the render, and the control plane forwards the value (and nothing when unset).

Run:  python3 charts/glidepath-app/tests/pac_provenance_test.py     (needs helm, pyyaml)
"""
import os
import subprocess
import unittest

import yaml

from pac_concurrency_test import EXTRA, render, repos

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CP = os.path.join(ROOT, "charts", "glidepath-control-plane")
CP_VALUES = os.path.join(CP, "tests", "values.example.yaml")
SET = "pipelinesAsCode.gitopsRepoProvenance=default_branch"


def forwarded(*extra):
    """The pipelinesAsCode block the control plane's tenant-onboarding ApplicationSet hands every app's glidepath-app."""
    p = subprocess.run(["helm", "template", "t", CP, "-f", CP_VALUES, *extra], capture_output=True, text=True)
    assert p.returncode == 0, p.stderr
    sets = [d for d in yaml.safe_load_all(p.stdout) if d and d["kind"] == "ApplicationSet"]
    assert sets, "no ApplicationSet rendered: this test would pass vacuously"
    text = yaml.safe_dump(sets)
    return text, sets


class PacGitopsProvenance(unittest.TestCase):
    def test_unset_renders_no_settings_and_nothing_else_changes(self):
        got = repos()
        self.assertEqual(sorted(got), ["example-app", "gitops-example-app"], got)  # non-vacuous: both Repository CRs rendered
        for name, spec in got.items():
            self.assertEqual(list(spec), ["url"], name)

    def test_default_branch_reaches_the_gitops_repository_only(self):
        got = repos("--set", SET)
        self.assertEqual(got["gitops-example-app"]["settings"], {"pipelinerun_provenance": "default_branch"})
        self.assertNotIn("settings", got["example-app"])

    def test_extra_gitops_repositories_are_not_changed(self):
        got = repos("--set", SET, "--set", "platformIdentity.extraGitopsRepos={%s}" % EXTRA)
        self.assertEqual(len(got), 3, got)
        self.assertNotIn("settings", got["gitops-cluster-shared"])

    def test_it_composes_with_the_concurrency_limit(self):
        got = repos("--set", SET, "--set", "pipelinesAsCode.concurrencyLimit=3")
        self.assertEqual(got["gitops-example-app"]["concurrency_limit"], 3)
        self.assertEqual(got["gitops-example-app"]["settings"]["pipelinerun_provenance"], "default_branch")

    def test_a_value_the_crd_does_not_know_fails(self):
        for bad in ("main", "Default_Branch"):
            p = render("--set-string", "pipelinesAsCode.gitopsRepoProvenance=" + bad)
            self.assertNotEqual(p.returncode, 0, bad)
            self.assertIn("default_branch, source, or unset", p.stderr, bad)

    def test_the_control_plane_forwards_it_and_forwards_nothing_when_unset(self):
        text, _ = forwarded("--set", SET)
        self.assertIn("gitopsRepoProvenance: default_branch", text)
        text, _ = forwarded()
        self.assertNotIn("gitopsRepoProvenance", text)


if __name__ == "__main__":
    unittest.main()
