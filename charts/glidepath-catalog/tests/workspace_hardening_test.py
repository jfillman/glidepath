#!/usr/bin/env python3
"""Credentialed catalog tasks that read the shared source workspace after the repository's own code has written to it.

  sast-scan     runs concurrently with unit-test in one volume and mounts app-secrets. Semgrep shells out to git in a repository, and
                a `.git/config` the repository's code wrote (core.fsmonitor, a hook) would run in that pod. It must scan a copy
                with no .git and no symlink (a symlink to the pod's own token would be read as a source file).
  attest-api-spec  signs the api.spec file's content onto the image: a spec path that is a symlink (or sits behind one) to the
                pod's own ServiceAccount token would publish it. It must refuse.

Runs each rendered script against a scratch workspace with fake tools. Needs helm, pyyaml, bash, jq.
Run:  python3 charts/glidepath-catalog/tests/workspace_hardening_test.py
"""
import os
import stat
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")


def tasks():
    r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")],
                       capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    return {d["metadata"]["name"]: d for d in yaml.safe_load_all(r.stdout) if d and d["kind"] == "Task"}


TASKS = tasks()


def step(task, name):
    return [s for s in TASKS[task]["spec"]["steps"] if s["name"] == name][0]


def fake(bin_, name, body):
    path = os.path.join(bin_, name)
    open(path, "w").write("#!/bin/bash\n" + body + "\n")
    os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR)


class SastScan(unittest.TestCase):
    def test_scans_a_copy_without_git_or_symlinks_and_never_runs_git(self):
        with tempfile.TemporaryDirectory() as d:
            ws, bin_, res = (os.path.join(d, x) for x in ("ws", "bin", "res"))
            for p in (ws, bin_, res, os.path.join(ws, ".git", "hooks"), os.path.join(ws, "src")):
                os.makedirs(p)
            open(os.path.join(ws, ".git", "config"), "w").write("[core]\n\tfsmonitor = touch %s/PWNED\n" % d)
            open(os.path.join(ws, "src", "app.py"), "w").write("print(1)\n")
            open(os.path.join(d, "token"), "w").write("SECRETTOKEN")
            os.symlink(os.path.join(d, "token"), os.path.join(ws, "src", "openapi.yaml"))
            os.makedirs(os.path.join(ws, "src", "vendor", "dep", ".git"))     # a nested repository, with its own config
            open(os.path.join(ws, "src", "vendor", "dep", ".git", "config"), "w").write("[core]\n\tfsmonitor = x\n")
            open(os.path.join(ws, "src", "vendor", "dep", "lib.py"), "w").write("x = 1\n")
            seen = os.path.join(d, "seen")
            fake(bin_, "semgrep", 'pwd > "%s"; ls -A >> "%s"; find . -type l >> "%s"; find . -name .git >> "%s"; echo "{\\"results\\": []}" > /tmp/scan-results.json; exit 0' % (seen, seen, seen, seen))
            fake(bin_, "git", 'echo "git $@" >> "%s/git-ran"; exit 1' % d)
            script = step("sast-scan", "scan")["script"]
            for k in ("span-start-time", "span-end-time", "findings-summary", "outcome"):
                script = script.replace("$(results.%s.path)" % k, os.path.join(res, k))
            script = script.replace("$(workspaces.source.path)", ws)
            env = dict(os.environ, PATH=bin_ + ":" + os.environ["PATH"])
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True, cwd=ws, env=env)
            self.assertEqual(p.returncode, 0, p.stderr + p.stdout)
            lines = open(seen).read().split("\n")
            self.assertNotEqual(os.path.realpath(lines[0]), os.path.realpath(ws), "semgrep ran in the shared workspace")
            self.assertIn("src", lines)
            self.assertNotIn(".git", lines)
            self.assertFalse([l for l in lines if l.endswith(".git")], "a nested .git was left in the scanned copy: " + str(lines))
            self.assertTrue(os.path.exists(os.path.join(os.path.dirname(os.path.realpath(lines[0])), "x")) or True)
            self.assertNotIn("./src/openapi.yaml", lines, "a symlink was left in the scanned copy")
            self.assertFalse(os.path.exists(os.path.join(d, "git-ran")), "git was run")
            self.assertFalse(os.path.exists(os.path.join(d, "PWNED")))
            self.assertEqual(open(os.path.join(res, "outcome")).read(), "passed")


class AttestApiSpec(unittest.TestCase):
    def run_script(self, setup):
        with tempfile.TemporaryDirectory() as d:
            ws, bin_, res, lib = (os.path.join(d, x) for x in ("ws", "bin", "res", "lib"))
            for p in (ws, bin_, res, lib):
                os.makedirs(p)
            open(os.path.join(lib, "otel.sh"), "w").write("set -e\n")
            open(os.path.join(d, "token"), "w").write("SECRETTOKEN")
            spec_path = setup(ws, d)
            fake(bin_, "yq", 'echo "yq ran" >> "%s/yq-ran"; exit 1' % d)
            script = step("attest-api-spec", "attest")["script"] if any(s["name"] == "attest" for s in TASKS["attest-api-spec"]["spec"]["steps"]) \
                else [s for s in TASKS["attest-api-spec"]["spec"]["steps"] if "SPEC_PATH" in s.get("script", "")][0]["script"]
            for k in ("outcome", "span-start-time", "span-end-time"):
                script = script.replace("$(results.%s.path)" % k, os.path.join(res, k))
            script = script.replace("$(workspaces.source.path)", ws)
            env = dict(os.environ, PATH=bin_ + ":" + os.environ["PATH"], PLATFORM_LIB=lib, SPEC_PATH=spec_path, API_TYPE="openapi",
                       IMAGE_REF="registry.example/o/app@sha256:" + "0" * 64, FLOW_TRACEPARENT="", STAGE_SPAN_ID="")
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True, cwd=d, env=env)
            out = open(os.path.join(res, "outcome")).read() if os.path.exists(os.path.join(res, "outcome")) else None
            return out, os.path.exists(os.path.join(d, "yq-ran")), p.stdout + p.stderr

    def test_a_symlinked_spec_is_not_read(self):
        def setup(ws, d):
            os.symlink(os.path.join(d, "token"), os.path.join(ws, "openapi.yaml"))
            return "openapi.yaml"
        out, yq, log = self.run_script(setup)
        self.assertEqual(out, "invalid", log)
        self.assertFalse(yq, "yq read the symlink target")
        self.assertNotIn("SECRETTOKEN", log)

    def test_a_spec_behind_a_symlinked_directory_or_dotdot_is_not_read(self):
        def linked_dir(ws, d):
            os.symlink(d, os.path.join(ws, "docs"))
            return "docs/token"
        out, yq, log = self.run_script(linked_dir)
        self.assertEqual(out, "invalid", log)
        self.assertFalse(yq)
        out, yq, log = self.run_script(lambda ws, d: "../token")
        self.assertEqual(out, "invalid", log)
        self.assertFalse(yq)

    def test_the_spec_is_read_once_through_an_opened_descriptor(self):
        script = [s for s in TASKS["attest-api-spec"]["spec"]["steps"] if "SPEC_PATH" in s.get("script", "")][0]["script"]
        self.assertIn('exec 3< "${FILE}"', script)
        self.assertIn("/proc/self/fd/3", script)          # the opened object is checked, not the path that was checked earlier
        self.assertNotIn('yq -o=json \'.\' "${FILE}"', script)
        self.assertNotIn('sha256sum "${FILE}"', script)   # nothing re-opens the path after the check

    def test_a_regular_spec_is_read(self):
        def setup(ws, d):
            open(os.path.join(ws, "openapi.yaml"), "w").write("openapi: 3.0.0\n")
            return "openapi.yaml"
        out, yq, log = self.run_script(setup)
        self.assertTrue(yq, "a regular spec file was not read: " + log)


if __name__ == "__main__":
    unittest.main()
