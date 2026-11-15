#!/usr/bin/env python3
"""build-artifact's publish step pushes the path the build step names. The build step runs the repository's code and can write
that result, so publish must refuse anything that is not a regular file inside <workspace>/.hangar-artifact-out: a `..`, a
symlink (the file or any directory on the way), a directory, a file elsewhere (the registry credential, say).

Runs the rendered step's own script with a fake `oras` and a scratch workspace. Needs helm, pyyaml, bash.
Run:  python3 charts/glidepath-catalog/tests/build_artifact_publish_test.py
"""
import os
import stat
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")


def publish_script():
    r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")],
                       capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    task = [d for d in yaml.safe_load_all(r.stdout) if d and d["kind"] == "Task" and d["metadata"]["name"] == "build-artifact"][0]
    return [s for s in task["spec"]["steps"] if s["name"] == "publish"][0]["script"]


class Publish(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.script = publish_script()

    def run_publish(self, setup):
        """setup(ws, secret_dir) -> the ARTIFACT_FILE value. Returns (exit code, oras argv or None, stderr)."""
        with tempfile.TemporaryDirectory() as d:
            ws = os.path.join(d, "ws")
            secret = os.path.join(d, "creds")
            bin_ = os.path.join(d, "bin")
            for p in (ws, secret, bin_, os.path.join(ws, ".hangar-artifact-out")):
                os.makedirs(p, exist_ok=True)
            open(os.path.join(secret, "config.json"), "w").write('{"auths": {"registry": {"auth": "SECRET"}}}')
            log = os.path.join(d, "oras.log")
            fake = os.path.join(bin_, "oras")
            open(fake, "w").write('#!/bin/sh\necho "$@" > "%s"\n' % log)
            os.chmod(fake, os.stat(fake).st_mode | stat.S_IXUSR)
            artifact = setup(ws, secret)
            script = self.script.replace("$(workspaces.source.path)", ws).replace("$(results.artifact-ref.path)", os.path.join(d, "ref"))
            env = dict(os.environ, PATH=bin_ + ":" + os.environ["PATH"], ARTIFACT_FILE=artifact, ARTIFACT_TYPE="go-binary",
                       IMAGE_REPO="registry.example/o/app", VERSION="1.0.0", DOCKER_CONFIG=secret)
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True, env=env, cwd=d)
            pushed = open(log).read() if os.path.exists(log) else None
            return p.returncode, pushed, p.stderr

    def good(self, ws, secret):
        f = os.path.join(ws, ".hangar-artifact-out", "app-linux-amd64")
        open(f, "w").write("bin")
        return f

    def test_a_regular_file_in_the_output_directory_is_pushed(self):
        code, pushed, err = self.run_publish(self.good)
        self.assertEqual(code, 0, err)
        self.assertIn("app-linux-amd64", pushed)

    def refused(self, setup, why):
        code, pushed, err = self.run_publish(setup)
        self.assertNotEqual(code, 0, "expected a refusal: " + why)
        self.assertIsNone(pushed, "oras ran for: " + why)
        self.assertNotIn("SECRET", err)

    def test_the_registry_credential_is_refused(self):
        self.refused(lambda ws, secret: os.path.join(secret, "config.json"), "credential file by absolute path")

    def test_dotdot_is_refused(self):
        self.refused(lambda ws, secret: os.path.join(ws, ".hangar-artifact-out", "..", "..", "creds", "config.json"), "traversal")
        self.refused(lambda ws, secret: os.path.join(ws, ".hangar-artifact-out", "..", "x"), "traversal to the workspace")

    def test_a_symlinked_file_is_refused(self):
        def setup(ws, secret):
            f = os.path.join(ws, ".hangar-artifact-out", "link")
            os.symlink(os.path.join(secret, "config.json"), f)
            return f
        self.refused(setup, "symlink to the credential")

    def test_a_symlinked_directory_on_the_way_is_refused(self):
        def setup(ws, secret):
            os.symlink(secret, os.path.join(ws, ".hangar-artifact-out", "dir"))
            return os.path.join(ws, ".hangar-artifact-out", "dir", "config.json")
        self.refused(setup, "symlinked directory")

    def test_the_output_directory_itself_a_symlink_is_refused(self):
        def setup(ws, secret):
            os.rmdir(os.path.join(ws, ".hangar-artifact-out"))
            os.symlink(secret, os.path.join(ws, ".hangar-artifact-out"))
            return os.path.join(ws, ".hangar-artifact-out", "config.json")
        self.refused(setup, "symlinked output directory")

    def test_a_directory_a_missing_file_and_an_empty_name_are_refused(self):
        def adir(ws, secret):
            p = os.path.join(ws, ".hangar-artifact-out", "sub")
            os.makedirs(p)
            return p
        self.refused(adir, "directory")
        self.refused(lambda ws, secret: os.path.join(ws, ".hangar-artifact-out", "nope"), "missing")
        self.refused(lambda ws, secret: "", "empty")
        self.refused(lambda ws, secret: "relative/path", "relative path")


if __name__ == "__main__":
    unittest.main()
