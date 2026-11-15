#!/usr/bin/env python3
"""policy-validation-gate and clearance-gate run `helm template` on a chart and value files the repository can choose, in a pod that
holds a ServiceAccount token. Helm follows symlinks (`.Files.Get`, `{{ fail ... }}`), and the steps used to `cat` helm's error into the
log, which a developer reads. So: a chart or value file with a symlink out of its directory is refused before helm runs, and helm's
error text is never printed from that pod.

Runs the rendered helper (chart_safe) against scratch charts. Needs helm, pyyaml, bash.
Run:  python3 charts/glidepath-catalog/tests/chart_safety_test.py
"""
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
TASKS = {d["metadata"]["name"]: d for d in yaml.safe_load_all(r.stdout) if d and d["kind"] == "Task"}


def scripts(task):
    return "\n".join(s.get("script", "") for s in TASKS[task]["spec"]["steps"])


class ChartSafety(unittest.TestCase):
    def test_both_gates_check_before_helm_and_print_no_helm_error(self):
        for task in ("policy-validation-gate", "clearance-gate"):
            text = scripts(task)
            self.assertIn("# BEGIN chart-safety", text, task)
            # helm's stderr goes to a file that is never printed; the files that are printed hold only chart_safe's own message
            self.assertNotRegex(text, r'cat "\$\{out\}[^"]*(helm-err|render\.err)"', task)
            self.assertNotRegex(text, r'(cat|head|tail|sed|grep)[^\n]*(\.helm-err|render\.err)', task)
            if task == "clearance-gate":
                self.assertIn('2> "${out}.helm-err"', text)
            for m in re.finditer(r"helm template", text):
                before = text[: m.start()]
                self.assertIn("chart_safe", before, task)

    def helper(self, tmp):
        text = scripts("policy-validation-gate")
        a = text.index("# BEGIN chart-safety")
        b = text.index("# END chart-safety")
        path = os.path.join(tmp, "helper.sh")
        open(path, "w").write(text[a:b])
        return path

    def check(self, setup):
        with tempfile.TemporaryDirectory() as tmp:
            chart = os.path.join(tmp, "chart")
            os.makedirs(os.path.join(chart, "templates"))
            open(os.path.join(chart, "Chart.yaml"), "w").write("name: x\n")
            open(os.path.join(tmp, "secret"), "w").write("SECRETTOKEN")
            files = setup(chart, tmp) or []
            h = self.helper(tmp)
            p = subprocess.run(["bash", "-c", 'source "%s"; chart_safe "%s" %s' % (h, chart, " ".join('"%s"' % f for f in files))],
                               capture_output=True, text=True)
            return p.returncode, p.stdout + p.stderr

    def test_a_plain_chart_passes(self):
        code, out = self.check(lambda c, t: None)
        self.assertEqual(code, 0, out)

    def test_a_symlink_inside_the_chart_passes(self):
        def setup(c, t):
            open(os.path.join(c, "templates", "a.yaml"), "w").write("a: 1\n")
            os.symlink("a.yaml", os.path.join(c, "templates", "b.yaml"))
        self.assertEqual(self.check(setup)[0], 0)

    def test_a_symlink_out_of_the_chart_is_refused(self):
        code, out = self.check(lambda c, t: os.symlink(os.path.join(t, "secret"), os.path.join(c, "templates", "x.txt")))
        self.assertNotEqual(code, 0)
        self.assertNotIn("SECRETTOKEN", out)
        code, out = self.check(lambda c, t: os.symlink("../../secret", os.path.join(c, "files-link")))
        self.assertNotEqual(code, 0)
        code, out = self.check(lambda c, t: os.symlink(t, os.path.join(c, "templates", "dir")))
        self.assertNotEqual(code, 0)
        code, out = self.check(lambda c, t: os.symlink("/nonexistent/x", os.path.join(c, "dangling")))
        self.assertNotEqual(code, 0)

    def test_a_symlinked_values_file_is_refused(self):
        def setup(c, t):
            link = os.path.join(t, "values.yaml")
            os.symlink(os.path.join(t, "secret"), link)
            return ["-f", link]
        code, out = self.check(setup)
        self.assertNotEqual(code, 0)
        self.assertNotIn("SECRETTOKEN", out)


if __name__ == "__main__":
    unittest.main()
