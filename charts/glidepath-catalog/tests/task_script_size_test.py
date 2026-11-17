#!/usr/bin/env python3
"""No catalog Task's step scripts may exceed the size Tekton can hand to a pod.

Tekton's place-scripts init container receives every step script of a Task as ONE base64-encoded argument, and Linux caps a single
argument at 131072 bytes (MAX_ARG_STRLEN). Past that every pod of the Task fails with "exec /bin/sh: argument list too long".
glidepath#247 inlined files/lib/release-source.sh into clearance-gate's fetch step and took the Task to 138044 bytes
(gitops-parachute#14). This renders every Task with the largest realistic values and fails when a Task's encoded total passes
LIMIT, which leaves headroom under the real cap. Library text a step needs belongs in the toolbox image ($PLATFORM_LIB) or in a
step env var, not in the script.

Run:  python3 charts/glidepath-catalog/tests/task_script_size_test.py   (needs helm, pyyaml)
"""
import base64
import os
import subprocess
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
VALUES = os.path.join(CATALOG, "tests", "values.example.yaml")
LIMIT = 120000
HARD_CAP = 131072
PER_STEP_OVERHEAD = 200  # place-scripts' own text around each script (measured: 6 steps, 138044 live vs 136944 of base64)

# The renders that add the most script text: the example values, and the same with every optional feature on (what a lab turns on).
RENDERS = {
    "example values": [],
    "promotion on": [
        "--set", "promotion.enabled=true", "--set", "promotion.pinDigest=true",
        "--set", "registries.flight.namespace=riley-roooo", "--set", "promotion.promoter.repo=riley-roooo/flight-promoter",
        "--set-json", 'releaseGuardrailOverridesByApp={"backstage":{"image-scan":{"required":false}}}',
    ],
}


def encoded_sizes(extra):
    out = subprocess.run(["helm", "template", "t", CATALOG, "-f", VALUES, "--namespace", "platform-catalog", *extra],
                         capture_output=True, text=True, check=True).stdout
    sizes = {}
    for d in yaml.safe_load_all(out):
        if d and d.get("kind") == "Task":
            steps = d["spec"].get("steps", [])
            sizes[d["metadata"]["name"]] = sum(len(base64.b64encode(s.get("script", "").encode())) + PER_STEP_OVERHEAD for s in steps)
    return sizes


class TaskScriptSize(unittest.TestCase):
    def test_every_task_fits(self):
        self.assertLess(LIMIT, HARD_CAP)
        for label, extra in RENDERS.items():
            sizes = encoded_sizes(extra)
            self.assertGreater(len(sizes), 10, "rendered too few Tasks: the guard is looking at nothing")
            self.assertIn("clearance-gate", sizes)
            over = {n: s for n, s in sizes.items() if s > LIMIT}
            self.assertFalse(over, f"{label}: Task step scripts over {LIMIT} encoded bytes (Linux caps one argument at {HARD_CAP}): {over}")



if __name__ == "__main__":
    unittest.main()
