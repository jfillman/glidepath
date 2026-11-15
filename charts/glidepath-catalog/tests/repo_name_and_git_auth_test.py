#!/usr/bin/env python3
"""Two small leftovers of earlier reviews (pipeline trust T3b).

1. A repository name is parsed whole: `acme.web` is a repository name, only a trailing `.git` is not. Three scripts cut it at the
   first dot (`[^/.]+`): verify-commit-signature (app_repo_name), comment-pr-check-result (repo) and the onboarding-resync
   bootstrap hook. Each now takes the validated full name and fails on one that is not plain. The test renders each script, runs
   its own parse lines against real URLs, and checks no `[^/.]+` truncation is left in those three.
2. A credential header that git needs only for a fetch does not stay exported for the rest of the step: validate-values and
   clearance-gate call git_auth_clear once the fetch is done (the token variables GIT_CONFIG_*; the step goes on to run the
   repository's validators and policy evaluation).

Run:  python3 charts/glidepath-catalog/tests/repo_name_and_git_auth_test.py     (needs helm, pyyaml, bash)
"""
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
APP = os.path.join(ROOT, "charts", "glidepath-app")
TRUNCATING = "([^/.]+)"


def helm(chart, *args):
    p = subprocess.run(["helm", "template", "x", chart, *args], capture_output=True, text=True)
    assert p.returncode == 0, p.stderr
    return [d for d in yaml.safe_load_all(p.stdout) if d]


def task_script(task, pick):
    """The one step script of the rendered catalog Task that `pick` selects."""
    d = [x for x in helm(CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")) if x["kind"] == "Task" and x["metadata"]["name"] == task][0]
    steps = [s["script"] for s in d["spec"]["steps"] if "script" in s and pick(s["script"])]
    assert len(steps) == 1, (task, len(steps))
    return steps[0]


def hook_script():
    cicd = {"apiVersion": "platform/v1", "kind": "PipelineConfig", "build": {"agent": "nodejs-20"},
            "deploy": {"environments": [{"name": "dev", "tier": "ground"}]},
            "pipelines": {"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": [{"stage": "build"}]}}}
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(cicd, f)
    try:
        docs = helm(APP, "--namespace", "app-x-cicd", "-f", f.name, "-f", os.path.join(APP, "tests", "fixtures", "control-plane-values.yaml"))
    finally:
        os.unlink(f.name)
    job = [d for d in docs if d["kind"] == "Job" and d["metadata"]["name"] == "onboarding-resync-bootstrap"][0]
    return job["spec"]["template"]["spec"]["containers"][0]["args"][0]


def parse_block(script, var):
    """The script's own parse lines for `var`: the sed that reads the name, the .git strip and the validation."""
    lines = script.splitlines()
    start = [i for i, l in enumerate(lines) if re.match(rf"\s*{var}=\"\$\(sed -E 's#\^https\?://github", l)]
    assert len(start) == 1, f"{var}: {len(start)} parse lines"
    block = lines[start[0]:start[0] + 2]
    assert "exit 1" in block[1], block
    return "\n".join(block)


class RepoNames(unittest.TestCase):
    CASES = {
        "verify-commit-signature": ("app_repo_name", "app_repo"),
        "comment-pr-check-result": ("repo", "REPO_URL"),
        "onboarding-resync-bootstrap": ("repo", "APP_REPO_URL"),
    }

    @classmethod
    def setUpClass(cls):
        cls.scripts = {
            "verify-commit-signature": task_script("verify-commit-signature", lambda s: "app_repo_name=" in s),
            "comment-pr-check-result": task_script("comment-pr-check-result", lambda s: 'repo="$(sed -E' in s),
            "onboarding-resync-bootstrap": hook_script(),
        }

    def parse(self, name, url):
        var, src = self.CASES[name]
        block = parse_block(self.scripts[name], var)
        p = subprocess.run(["bash", "-c", f'{src}="$1"\n{block}\nprintf "%s" "${{{var}}}"', "x", url], capture_output=True, text=True)
        return p.returncode, p.stdout, p.stderr

    def test_a_name_with_a_dot_is_kept_whole(self):
        for name in self.CASES:
            for url, want in {
                "https://github.com/o/app": "app",
                "https://github.com/o/acme.web": "acme.web",
                "https://github.com/o/acme.web.git": "acme.web",
                "https://github.com/o/acme.web/": "acme.web",
                "http://github.com/o/a.b.c": "a.b.c",
                "https://github.com/o/gitops-shop.api.git": "gitops-shop.api",
            }.items():
                with self.subTest(task=name, url=url):
                    rc, out, err = self.parse(name, url)
                    self.assertEqual((rc, out), (0, want), err)

    def test_a_name_that_is_not_plain_is_refused(self):
        for name in self.CASES:
            for url in ["https://github.com/o/..", "https://github.com/o/.", "https://github.com/o/r/pull/3", "https://evil.example/o/r",
                        "https://github.com/o/", "https://github.com/o/r r", "https://github.com/o/" + "a" * 101, "not a url", ""]:
                with self.subTest(task=name, url=url):
                    rc, out, err = self.parse(name, url)
                    self.assertEqual(rc, 1, (out, err))
                    self.assertIn("plain repository name", err)

    def test_these_three_no_longer_truncate_at_the_first_dot(self):
        # only the parse of the variable each task names: other scripts (and verify-commit-signature's gitops_repo) still do, and are owed
        for name, (var, src) in self.CASES.items():
            with self.subTest(task=name):
                for line in self.scripts[name].splitlines():
                    if re.match(rf"\s*{var}=", line):
                        self.assertNotIn(TRUNCATING, line)
                        self.assertIn("[A-Za-z0-9._-]+", line)


class TokenDoesNotOutliveItsFetch(unittest.TestCase):
    def assert_cleared_after_every_fetch(self, script, fetch_re):
        lines = script.splitlines()
        fetches = [i for i, l in enumerate(lines) if re.search(fetch_re, l) and not l.lstrip().startswith("#")]
        clears = [i for i, l in enumerate(lines) if re.match(r"\s*git_auth_clear\b", l)]
        self.assertTrue(fetches, "the script fetches with the credential header")
        for f in fetches:
            nxt = [c for c in clears if c > f]
            self.assertTrue(nxt, f"no git_auth_clear after the fetch at line {f + 1}: {lines[f].strip()}")
            between = lines[f + 1:nxt[0]]
            # only the fetch's own follow-up (a checkout, removing the copied config) may sit between: nothing else runs with the token set
            for l in between:
                self.assertRegex(l.strip(), r"^(git .*checkout .*|rm -f .*|#.*|\}|)$", f"{l.strip()!r} runs after the fetch with the token still exported")

    def test_validate_values(self):
        script = task_script("validate-values", lambda s: "git_auth " in s)
        self.assert_cleared_after_every_fetch(script, r"with_retry 3 -- git fetch")

    def test_clearance_gate(self):
        script = task_script("clearance-gate", lambda s: "fetch_repo()" in s)
        self.assert_cleared_after_every_fetch(script, r"with_retry 3 -- git -C .* fetch")
        # the helper runs the fetch and clears in the same function: no caller has to remember to
        body = script[script.index("fetch_repo()"):]
        body = body[:body.index("\n}\n") + 1]
        self.assertIn("git_auth_clear", body[body.index(" fetch "):])


if __name__ == "__main__":
    unittest.main()
