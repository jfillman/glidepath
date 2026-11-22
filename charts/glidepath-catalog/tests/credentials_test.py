#!/usr/bin/env python3
"""No credential in a URL, an argument or a log line the platform posts.

git's failing command, with its arguments, used to be echoed by with_retry, and the arguments carried `https://x-access-token:<token>@github.com/...`;
comment-pr-check-result posts a log excerpt into the pull request, and a repository's own configuration can make a fetch fail the same way every
time. So: a token goes in the environment as an HTTP header scoped to the git host (git_auth), never in a URL or an argument; with_retry prints a label,
not its arguments; and every place a log excerpt leaves the cluster passes it through redact_secrets first.

Needs helm, pyyaml, bash, git.   Run:  python3 charts/glidepath-catalog/tests/credentials_test.py
"""
import glob
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")], capture_output=True, text=True)
assert r.returncode == 0, r.stderr
RENDERED = r.stdout
DOCS = [d for d in yaml.safe_load_all(RENDERED) if d]
TASKS = {d["metadata"]["name"]: d for d in DOCS if d["kind"] in ("Task", "StepAction")}  # a name that is both (notify-slack) keeps the last: the log test walks DOCS
FAKE = "ghs_FAKEtokenFAKEtokenFAKEtoken0123456789"
FAKE_JWT = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlLXNpZ25hdHVyZS1zaWduYXR1cmU"


def block(name):
    """A helper block of the catalog, as the tasks splice it in."""
    for t in TASKS.values():
        scripts = [s.get("script", "") for s in t["spec"].get("steps", [])] + [t["spec"].get("script", "")]
        for s in scripts:
            if "# BEGIN %s" % name in s:
                return s[s.index("# BEGIN %s" % name):s.index("# END %s" % name)]
    raise AssertionError(name)


def bash(script, **env):
    return subprocess.run(["bash", "-c", script], capture_output=True, text=True, env=dict(os.environ, **env))


class NoCredentialInAUrl(unittest.TestCase):
    URL_WITH_CREDENTIALS = re.compile(r"https?://[^/\s\"'$]*:[^/\s@\"']+@|https?://\$\{?[A-Za-z_]*(TOKEN|token|Token)\}?@|x-access-token:\$\{")

    def sources(self):
        for t in TASKS.values():
            for s in t["spec"].get("steps", []):
                yield t["metadata"]["name"], s.get("script", "")
            yield t["metadata"]["name"], t["spec"].get("script", "")
        for path in glob.glob(os.path.join(ROOT, "catalog", "lib", "*")) + glob.glob(os.path.join(CATALOG, "files", "lib", "*")):
            yield path, open(path).read()
        for path in glob.glob(os.path.join(ROOT, "charts", "glidepath-app", "templates", "**", "*.yaml"), recursive=True):
            yield path, open(path).read()

    def test_no_script_builds_a_url_with_a_credential_in_it(self):
        n, bad = 0, []
        for name, text in self.sources():
            n += 1
            for line in text.split("\n"):
                if line.strip().startswith("#"):
                    continue
                if self.URL_WITH_CREDENTIALS.search(line):
                    bad.append((name, line.strip()[:120]))
        self.assertGreater(n, 60, "the scan found too few scripts to mean anything")
        self.assertEqual(bad, [])

    def test_the_scan_sees_the_old_pattern(self):
        self.assertTrue(self.URL_WITH_CREDENTIALS.search('clone_url="https://x-access-token:${token}@github.com/${owner}/${repo}.git"'))
        self.assertTrue(self.URL_WITH_CREDENTIALS.search("git fetch https://user:pw@example.org/x"))
        self.assertFalse(self.URL_WITH_CREDENTIALS.search('clone_url="https://${GIT_HOST}/${owner}/${repo}.git"'))

    def test_every_git_remote_call_that_needs_a_token_sets_it_through_git_auth(self):
        for name, t in TASKS.items():
            for s in t["spec"].get("steps", []):
                text = s.get("script", "")
                if re.search(r"(?m)^\s*(with_retry 3 -- )?git (clone|push|fetch|-C \S+ fetch)[^\n]*\$\{GIT_HOST\}|GIT_HOST", text) and "git_auth " in text:
                    self.assertIn("# BEGIN git-auth", text, name)
                    self.assertLess(text.index("# BEGIN git-auth"), text.index("git_auth \""), name)


class GitAuth(unittest.TestCase):
    def test_the_token_is_in_a_header_scoped_to_the_git_host_and_nowhere_else(self):
        with tempfile.TemporaryDirectory() as tmp:
            script = block("git-auth") + '\ngit_auth "$T"\ngit config --get-urlmatch http.extraheader "$1"; echo "follow=$(git config --get-urlmatch http.followRedirects "$1")"\n'
            ok = bash(script + "", T=FAKE, HOME=tmp, GIT_CONFIG_NOSYSTEM="1")
            self.assertEqual(ok.returncode, 0, ok.stderr)
            # on the configured host: the header (a Basic credential, in the environment of git); another host: none; and no redirect is followed
            for url, want in (("https://github.com/o/r.git", True), ("https://evil.example/o/r.git", False), ("https://github.com.evil.example/o/r.git", False),
                              ("http://github.com/o/r.git", False)):
                p = subprocess.run(["bash", "-c", block("git-auth") + '\ngit_auth "$T"\ngit config --get-urlmatch http.extraheader "$U"; echo "follow=$(git config --get-urlmatch http.followRedirects "$U")"'],
                                   capture_output=True, text=True, env=dict(os.environ, T=FAKE, U=url, HOME=tmp, GIT_CONFIG_NOSYSTEM="1"))
                self.assertEqual("Authorization: Basic" in p.stdout, want, (url, p.stdout, p.stderr))
                if want:
                    self.assertIn("follow=false", p.stdout)
                    self.assertNotIn(FAKE, p.stdout, "the header carries the credential encoded, never the raw token string")

    def test_a_failing_git_command_shows_neither_the_token_nor_its_arguments(self):
        with tempfile.TemporaryDirectory() as tmp:
            retry = open(os.path.join(ROOT, "catalog", "lib", "retry.sh")).read()
            script = retry + "\n" + block("git-auth") + '\ngit_auth "$T"\nsleep() { :; }\nwith_retry 2 -- git -C "$D" fetch --quiet --depth 1 "https://${GIT_HOST}/o/r.git" "refs/heads/main-secret-rev"\n'
            p = bash(script, T=FAKE, D=tmp, HOME=tmp, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0")
            out = p.stdout + p.stderr
            self.assertNotEqual(p.returncode, 0)
            self.assertIn("with_retry: 'git' failed", out)
            self.assertNotIn(FAKE, out)
            self.assertNotIn("main-secret-rev", out, "with_retry prints a label, not its arguments")
            self.assertNotIn("https://", out.split("with_retry")[1] if "with_retry" in out else "")

    def test_a_caller_supplied_label_is_what_is_printed(self):
        retry = open(os.path.join(ROOT, "catalog", "lib", "retry.sh")).read()
        p = bash(retry + "\nsleep() { :; }\nWITH_RETRY_LABEL='fetch the PR head' with_retry 2 -- false secretarg\n")
        self.assertIn("'fetch the PR head' failed", p.stderr)
        self.assertNotIn("secretarg", p.stderr)

    def test_the_host_comes_from_the_value(self):
        out = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), "--set", "gitHost=git.example.org",
                              "--show-only", "templates/tasks/deploy-manifests.yaml"], capture_output=True, text=True)
        self.assertIn('GIT_HOST="git.example.org"', out.stdout)


class Redaction(unittest.TestCase):
    def redact(self, text):
        return bash(block("redact-secrets") + '\nprintf "%s" "$T" | redact_secrets\n', T=text).stdout

    def test_every_token_shape_is_redacted(self):
        shapes = [FAKE, "ghp_" + "A" * 36, "gho_" + "b" * 30, "ghu_" + "c" * 30, "github_pat_" + "D" * 30 + "_" + "e" * 30, FAKE_JWT,
                  "Bearer abcdefghijklmnop1234", "Authorization: Bearer abcdefghijklmnop1234", "https://x-access-token:%s@github.com/o/r.git" % FAKE,
                  "https://user:hunter2hunter2@example.org/x", "x-access-token:%s" % FAKE, "Authorization: Basic eC1hY2Nlc3MtdG9rZW46Z2hzX3Rva2Vu"]
        for s in shapes:
            out = self.redact("fatal: unable to access '%s' (exit 128)" % s)
            for secret in (FAKE, FAKE_JWT, "hunter2hunter2", "abcdefghijklmnop1234", "eC1hY2Nlc3MtdG9rZW46", "A" * 36, "D" * 30):
                self.assertNotIn(secret, out, s)
            self.assertIn("redacted", out, s)

    def test_ordinary_text_is_left_alone(self):
        text = "FAIL: an environment did not render (the render task's log says why)\nhangar-prod/prod: 3 objects"
        self.assertEqual(self.redact(text), text)

    def test_every_place_a_log_excerpt_leaves_the_cluster_redacts_it_first(self):
        n = 0
        for t in DOCS:
            if t["kind"] not in ("Task", "StepAction"):
                continue
            name = t["metadata"]["name"]
            for s in t["spec"].get("steps", []) + [t["spec"]]:
                text = s.get("script", "")
                # The whole pipeline statement (backslash continuations joined): redaction must come first, and nothing
                # before it may cut bytes (a cut could split a secret so redact_secrets no longer matches it).
                for m in re.finditer(r"kubectl logs[^\n]*(?:\\\n[^\n]*)*", text):
                    n += 1
                    stmt = m.group(0).replace("\\\n", " ")
                    self.assertIn("redact_secrets", stmt, (name, stmt[:100]))
                    before, after = stmt.split("redact_secrets", 1)
                    self.assertNotRegex(before, r"tail -c|head -c|cut |substr|log_tail", (name, "truncated before redaction", stmt[:100]))
                    self.assertIn("# BEGIN redact-secrets", text, name)
                # log_tail (comment-pr-check-result) only ever receives a pod name and reads the log itself, redacted;
                # nothing may feed it raw text, and no other function may cut a log by bytes.
                self.assertNotRegex(text, r"\|\s*log_tail", (name, "log_tail fed by a pipe"))
                for m in re.finditer(r"log_tail\(\) \{.*?\n        \}", text, re.S):
                    self.assertEqual(len(re.findall(r"kubectl logs", m.group(0))), 1, name)
        self.assertGreaterEqual(n, 3)


if __name__ == "__main__":
    unittest.main()
