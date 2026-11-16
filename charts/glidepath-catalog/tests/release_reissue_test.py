#!/usr/bin/env python3
"""Reissue (docs/admin/release-state.md#reissue): open-release-pr's two reissue pieces.

glidepath-api stamps `glidepath.io/reissue-of` (the closed release's chain id) on the run it starts for POST
.../releases/{chainId}/reissue. open-release-pr reads it from the run itself, never from a parameter, checks its shape,
writes it on the NEW release's record as reissueOf, and marks the closed release's record reissuedBy (ending it `closed`
when it was still `proposed`). Runs the task's own bash against a fake kubectl.
Run: python3 charts/glidepath-catalog/tests/release_reissue_test.py   (needs helm, pyyaml, jq, bash)
"""
import json
import os
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
OLD = "11111111-2222-4333-8444-555555555555"
NEW = "66666666-7777-4888-8999-aaaaaaaaaaaa"


def put(path, text):
    with open(path, "w") as f:
        f.write(text)


def render():
    cmd = ["helm", "template", "t", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), "--show-only", "templates/tasks/open-release-pr.yaml"]
    p = subprocess.run(cmd, capture_output=True, text=True)
    assert p.returncode == 0, p.stderr
    return yaml.safe_load(p.stdout)


TASK = render()
SCRIPT = TASK["spec"]["steps"][0]["script"]


class Reissue(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        a = SCRIPT.index('reissue_of=""')
        b = SCRIPT.index("# The dev-side release record (ADR-0021)")
        put(os.path.join(cls.tmp, "read.sh"), 'set -euo pipefail\nrun_json="$(cat "$FX/run.json")"\n' + SCRIPT[a:b] + '\nprintf "%s" "${reissue_of}"\n')
        c = SCRIPT.index("link_reissued() {")
        d = SCRIPT.index('if [[ -z "${CLUSTER}" ]]')
        put(os.path.join(cls.tmp, "link.sh"), "set -euo pipefail\n" + SCRIPT[c:d] + "\nlink_reissued\n")
        bin_ = os.path.join(cls.tmp, "bin")
        os.makedirs(bin_)
        put(os.path.join(bin_, "kubectl"), '''#!/usr/bin/env bash
echo "kubectl $*" >> "$FX/calls"
case "$1" in
  get) [[ -f "$FX/cm.json" ]] || exit 1; cat "$FX/cm.json" ;;
  patch) echo "$*" >> "$FX/patched" ;;
esac
''')
        os.chmod(os.path.join(bin_, "kubectl"), 0o755)

    def env(self, fx, **extra):
        return dict(os.environ, FX=fx, PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], NAMESPACE="app-shop-cicd",
                    APP_NAME="shop", ENV="prod", CLUSTER="hangar-prod", CHAIN_ID=NEW, **extra)

    def read(self, annotations):
        fx = tempfile.mkdtemp(dir=self.tmp)
        put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": annotations}}))
        p = subprocess.run(["bash", os.path.join(self.tmp, "read.sh")], capture_output=True, text=True, env=self.env(fx))
        self.assertEqual(p.returncode, 0, p.stderr)
        return p.stdout.splitlines()[-1] if p.stdout else ""

    def link(self, data, labels=None, reissue_of=OLD):
        fx = tempfile.mkdtemp(dir=self.tmp)
        lab = {"hangar.io/app": "shop", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"} if labels is None else labels
        put(os.path.join(fx, "cm.json"), json.dumps({"metadata": {"labels": lab}, "data": data}))
        p = subprocess.run(["bash", os.path.join(self.tmp, "link.sh")], capture_output=True, text=True, env=self.env(fx, reissue_of=reissue_of))
        self.assertEqual(p.returncode, 0, p.stderr)
        path = os.path.join(fx, "patched")
        if not os.path.exists(path):
            return ""
        with open(path) as f:
            return f.read()

    def test_the_annotation_is_read_from_the_run(self):
        self.assertEqual(self.read({"glidepath.io/reissue-of": OLD}), OLD)

    def test_a_malformed_value_or_the_releases_own_chain_is_dropped(self):
        self.assertEqual(self.read({"glidepath.io/reissue-of": "not-a-uuid"}), "")
        self.assertEqual(self.read({"glidepath.io/reissue-of": NEW}), "")
        self.assertEqual(self.read({}), "")

    def test_it_is_never_a_parameter(self):
        self.assertNotIn("reissue-of", [p["name"] for p in TASK["spec"]["params"]])

    def test_the_record_carries_reissueOf(self):
        self.assertIn('--from-literal="reissueOf=${reissue_of}"', SCRIPT)

    def test_a_proposed_record_ends_closed_and_names_the_release_that_replaced_it(self):
        patched = self.link({"state": "proposed"})
        self.assertIn(f"release-tracking-{OLD}", patched)
        body = json.loads(patched.split("-p ", 1)[1].strip())
        self.assertEqual(body["data"]["reissuedBy"], NEW)
        self.assertEqual(body["data"]["state"], "closed")
        self.assertIn("stateAt", body["data"])

    def test_a_closed_record_only_gains_the_link(self):
        body = json.loads(self.link({"state": "closed"}).split("-p ", 1)[1].strip())
        self.assertEqual(body, {"data": {"reissuedBy": NEW}})

    def test_a_record_that_ran_or_is_already_linked_or_is_another_places_is_left_alone(self):
        for st in ("merged", "healthy", "superseded", "rolled-back", "progressing"):
            self.assertEqual(self.link({"state": st}), "", st)
        self.assertEqual(self.link({"state": "closed", "reissuedBy": "someone-else"}), "")
        self.assertEqual(self.link({"state": "proposed"}, labels={"hangar.io/app": "shop", "hangar.io/env": "staging", "hangar.io/cluster": "hangar-prod"}), "")
        self.assertEqual(self.link({"state": "proposed"}, labels={"hangar.io/app": "other", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"}), "")
        self.assertEqual(self.link({"state": "proposed"}, reissue_of=""), "")


if __name__ == "__main__":
    unittest.main()
