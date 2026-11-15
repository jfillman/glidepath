#!/usr/bin/env python3
"""No two platform CronJobs fire in the same minute of the hour.

A single-node lab fell over on 2026-11-15 because every CronJob fired on :00/:30 together. This renders
glidepath-control-plane and fails if two of its CronJobs share a minute-of-hour slot, if one is on :00 or :30 (where
everything else fires), or if one lacks concurrencyPolicy Forbid or a startingDeadlineSeconds. It also renders
glidepath-app's per-app pr-token-refresher for several app names: the minute is derived from the name (so apps differ),
the period stays 20 minutes, and minuteOffset overrides it.

Run:  python3 charts/glidepath-control-plane/tests/cronjob_schedule_test.py      (needs helm, pyyaml)
"""
import os
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CP = os.path.join(ROOT, "charts", "glidepath-control-plane")
APP = os.path.join(ROOT, "charts", "glidepath-app")
VALUES = os.path.join(CP, "tests", "values.example.yaml")


def helm(chart, *args):
    out = subprocess.run(["helm", "template", "t", chart, *args], capture_output=True, text=True)
    assert out.returncode == 0, out.stderr
    return [d for d in yaml.safe_load_all(out.stdout) if d]


def minutes(expr):
    """Minutes of the hour a five-field cron expression fires in (minute field: N, N-59/S, */S, a,b)."""
    field = expr.split()[0]
    out = set()
    for part in field.split(","):
        rng, _, step = part.partition("/")
        lo, _, hi = rng.partition("-")
        if rng == "*":
            lo, hi = "0", "59"
        hi = hi or (("59" if step else lo))
        out |= set(range(int(lo), int(hi) + 1, int(step or 1)))
    return out


def cronjobs(docs):
    return [d for d in docs if d["kind"] == "CronJob"]


class ControlPlane(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.jobs = cronjobs(helm(CP, "-f", VALUES))

    def test_has_the_four_cronjobs(self):
        names = sorted(j["metadata"]["name"] for j in self.jobs)
        self.assertEqual(names, ["pipelinerun-pruner", "pr-namespace-ttl-sweep", "release-record-sweeper",
                                 "stalled-pipeline-detector"])

    def test_no_two_share_a_minute(self):
        seen = {}
        for j in self.jobs:
            for m in minutes(j["spec"]["schedule"]):
                self.assertNotIn(m, seen, f"{j['metadata']['name']} and {seen.get(m)} both fire at minute {m}")
                seen[m] = j["metadata"]["name"]

    def test_none_on_the_hour_or_half_hour(self):
        for j in self.jobs:
            self.assertFalse({0, 30} & minutes(j["spec"]["schedule"]), j["metadata"]["name"])

    def test_forbid_and_a_deadline(self):
        for j in self.jobs:
            self.assertEqual(j["spec"]["concurrencyPolicy"], "Forbid", j["metadata"]["name"])
            self.assertGreater(j["spec"].get("startingDeadlineSeconds", 0), 0, j["metadata"]["name"])

    def test_schedule_is_a_value(self):
        docs = cronjobs(helm(CP, "-f", VALUES, "--set", "cronSchedules.pipelinerunPruner=11 * * * *"))
        pruner = [j for j in docs if j["metadata"]["name"] == "pipelinerun-pruner"][0]
        self.assertEqual(pruner["spec"]["schedule"], "11 * * * *")


class PrTokenRefresher(unittest.TestCase):
    def render(self, app, offset=None):
        with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
            vals = {
                "platformIdentity": {"appName": app, "type": "app", "gitopsRepoUrl": "https://github.com/o/g",
                                     "appRepoUrl": "https://github.com/o/a", "githubOwner": "o",
                                     "catalogNamespace": "platform-catalog"},
                "platformCicdRepoUrl": "https://github.com/o/p.git", "devClusterName": "example-dev",
                "defaultChart": {"repoURL": "https://github.com/o/f.git", "path": "c", "targetRevision": "v0"},
                "ephemeralEnvironments": {"pullRequest": {"enabled": True}},
            }
            if offset is not None:
                vals["prTokenRefresher"] = {"minuteOffset": offset}
            yaml.safe_dump(vals, f)
            f.flush()
            jobs = cronjobs(helm(APP, "-f", f.name))
        return [j for j in jobs if j["metadata"]["name"].endswith("pr-token-refresher")][0]

    def test_every_20_minutes_forbid_deadline(self):
        j = self.render("gate-api")
        self.assertEqual(len(minutes(j["spec"]["schedule"])), 3)
        self.assertEqual(j["spec"]["concurrencyPolicy"], "Forbid")
        self.assertGreater(j["spec"]["startingDeadlineSeconds"], 0)

    def test_minute_derives_from_the_app_name(self):
        apps = ["boarding-api", "flight-api", "gate-api"]
        slots = [self.render(a)["spec"]["schedule"] for a in apps]
        self.assertEqual(len(set(slots)), 3, slots)
        self.assertEqual(slots, [self.render(a)["spec"]["schedule"] for a in apps], "not deterministic")

    def test_the_apps_clear_the_control_plane_slots(self):
        taken = set()
        for j in ControlPlane.jobs if hasattr(ControlPlane, "jobs") else cronjobs(helm(CP, "-f", VALUES)):
            taken |= minutes(j["spec"]["schedule"])
        for a in ["boarding-api", "flight-api", "gate-api"]:
            self.assertFalse(taken & minutes(self.render(a)["spec"]["schedule"]), a)

    def test_offset_zero_is_honoured(self):
        self.assertEqual(self.render("gate-api", 0)["spec"]["schedule"], "0-59/20 * * * *")


if __name__ == "__main__":
    unittest.main()
