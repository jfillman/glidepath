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


def cm(name, data, labels=None, rv="100"):
    lab = {"hangar.io/app": "shop", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"} if labels is None else labels
    return {"metadata": {"name": name, "resourceVersion": rv, "labels": lab}, "data": data}


class Reissue(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        a = SCRIPT.index('reissue_of=""')
        b = SCRIPT.index("# The dev-side release record (ADR-0021)")
        put(os.path.join(cls.tmp, "read.sh"), 'set -euo pipefail\nrun_json="$(cat "$FX/run.json")"\n' + SCRIPT[a:b] + '\nprintf "%s" "${reissue_of}"\n')
        # the reissue functions, and close_stale_proposals, as one script that runs the function named by $FN
        c = SCRIPT.index("close_stale_proposals() {")
        d = SCRIPT.index('if [[ -z "${CLUSTER}" ]]')
        put(os.path.join(cls.tmp, "fns.sh"), "set -euo pipefail\n" + SCRIPT[c:d] + '\n$FN\n')
        bin_ = os.path.join(cls.tmp, "bin")
        os.makedirs(bin_)
        put(os.path.join(bin_, "kubectl"), '''#!/usr/bin/env bash
echo "kubectl $*" >> "$FX/calls"
args="$*"
case "$1" in
  get)
    if [[ "$args" == *" -l "* ]]; then cat "$FX/list.json"; exit 0; fi
    f="$FX/cm-$3.json"; [[ -f "$FX/switched" && -f "$FX/cm-$3.after.json" ]] && f="$FX/cm-$3.after.json"
    [[ -f "$f" ]] || exit 1
    if [[ "$args" == *jsonpath* ]]; then k="${args##*.data.}"; k="${k%\}*}"; jq -r --arg k "$k" '.data[$k] // empty' "$f"; else cat "$f"; fi ;;
  patch)
    if [[ -f "$FX/conflict" ]]; then rm "$FX/conflict"; touch "$FX/switched"; exit 1; fi
    echo "$3 $args" >> "$FX/patched" ;;
esac
''')
        put(os.path.join(bin_, "curl"), '''#!/usr/bin/env bash
u="${@: -1}"; echo "curl $u" >> "$FX/calls"
case "$u" in
  *"pulls?state=open"*) cat "$FX/pulls.json" ;;
  */pulls/1) echo '{"state":"closed","merged":false}' ;;
  *) exit 22 ;;
esac
''')
        for n in ("kubectl", "curl"):
            os.chmod(os.path.join(bin_, n), 0o755)

    def env(self, fx, **extra):
        return dict(os.environ, FX=fx, PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], NAMESPACE="app-shop-cicd",
                    APP_NAME="shop", ENV="prod", CLUSTER="hangar-prod", CHAIN_ID=NEW, owner="o", gitops_repo="gitops-shop", token="t", **extra)

    def fixture(self, files):
        fx = tempfile.mkdtemp(dir=self.tmp)
        for k, v in files.items():
            put(os.path.join(fx, k), v if isinstance(v, str) else json.dumps(v))
        return fx

    def run_fn(self, fx, fn, reissue_of=OLD):
        p = subprocess.run(["bash", os.path.join(self.tmp, "fns.sh")], capture_output=True, text=True, env=self.env(fx, FN=fn, reissue_of=reissue_of))
        return p

    def patched(self, fx):
        path = os.path.join(fx, "patched")
        if not os.path.exists(path):
            return ""
        with open(path) as f:
            return f.read()

    def read(self, annotations):
        fx = self.fixture({"run.json": {"metadata": {"annotations": annotations}}})
        p = subprocess.run(["bash", os.path.join(self.tmp, "read.sh")], capture_output=True, text=True, env=self.env(fx))
        self.assertEqual(p.returncode, 0, p.stderr)
        return p.stdout.splitlines()[-1] if p.stdout else ""

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

    # ---- link_reissued: a resourceVersion precondition, re-read on conflict, never a record the relay owns ----
    def link(self, data, labels=None, after=None, conflict=False, reissue_of=OLD):
        files = {f"cm-release-tracking-{OLD}.json": cm(f"release-tracking-{OLD}", data, labels)}
        if after is not None:
            files[f"cm-release-tracking-{OLD}.after.json"] = cm(f"release-tracking-{OLD}", after, labels, rv="101")
        if conflict:
            files["conflict"] = ""
        fx = self.fixture(files)
        p = self.run_fn(fx, "link_reissued", reissue_of)
        self.assertEqual(p.returncode, 0, p.stderr)
        return self.patched(fx)

    def ops(self, patched):
        return json.loads(patched.split("-p ", 1)[1].strip())

    def test_a_proposed_record_ends_closed_with_a_resourceVersion_precondition(self):
        ops = self.ops(self.link({"state": "proposed"}))
        self.assertEqual(ops[0], {"op": "test", "path": "/metadata/resourceVersion", "value": "100"})
        by_path = {o["path"]: o["value"] for o in ops[1:]}
        self.assertEqual(by_path["/data/reissuedBy"], NEW)
        self.assertEqual(by_path["/data/state"], "closed")
        self.assertIn("/data/stateAt", by_path)

    def test_a_closed_record_only_gains_the_link(self):
        ops = self.ops(self.link({"state": "closed"}))
        self.assertEqual([o["path"] for o in ops[1:]], ["/data/reissuedBy"])

    def test_a_conflict_re_reads_and_never_moves_a_record_the_relay_took_to_merged(self):
        # the first patch fails (the record changed); the re-read says merged: nothing is patched
        self.assertEqual(self.link({"state": "proposed"}, after={"state": "merged"}, conflict=True), "")
        for st in ("healthy", "progressing", "superseded"):
            self.assertEqual(self.link({"state": "proposed"}, after={"state": st}, conflict=True), "", st)

    def test_a_conflict_that_left_it_proposed_is_retried_with_the_new_resourceVersion(self):
        ops = self.ops(self.link({"state": "proposed"}, after={"state": "proposed"}, conflict=True))
        self.assertEqual(ops[0]["value"], "101")

    def test_a_record_that_ran_or_is_already_linked_or_is_another_places_is_left_alone(self):
        for st in ("merged", "healthy", "superseded", "rolled-back", "progressing"):
            self.assertEqual(self.link({"state": st}), "", st)
        self.assertEqual(self.link({"state": "closed", "reissuedBy": "someone-else"}), "")
        self.assertEqual(self.link({"state": "proposed"}, labels={"hangar.io/app": "shop", "hangar.io/env": "staging", "hangar.io/cluster": "hangar-prod"}), "")
        self.assertEqual(self.link({"state": "proposed"}, labels={"hangar.io/app": "other", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"}), "")
        self.assertEqual(self.link({"state": "proposed"}, reissue_of=""), "")

    # ---- reissue_guard: re-checked at PR time ----
    def guard(self, others, pulls=(), old_created="2026-11-10T10:00:00Z"):
        items = [cm(f"release-tracking-{OLD}", {"prCreatedAt": old_created, "releaseId": f"{OLD}:hangar-prod/prod"}), cm(f"release-tracking-{NEW}", {"prCreatedAt": "2026-11-16T00:00:00Z"})]
        items += [cm(f"release-tracking-{n}", d) for n, d in others]
        fx = self.fixture({f"cm-release-tracking-{OLD}.json": items[0], "list.json": {"items": items}, "pulls.json": list(pulls)})
        p = self.run_fn(fx, "reissue_guard")
        return p, fx

    def test_the_guard_passes_when_the_reissued_release_is_still_the_newest_and_no_PR_is_open(self):
        p, _ = self.guard([("aaaa", {"prCreatedAt": "2026-11-01T00:00:00Z", "releaseId": "older"})], pulls=[{"head": {"ref": "release-staging-abc12345"}, "html_url": "u"}])
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_the_guard_refuses_when_a_newer_release_opened_since_and_changes_nothing(self):
        p, fx = self.guard([("bbbb", {"prCreatedAt": "2026-11-12T00:00:00Z", "releaseId": "newer:hangar-prod/prod", "state": "proposed"})])
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("a newer release", p.stderr)
        self.assertEqual(self.patched(fx), "")

    def test_the_guard_refuses_when_another_release_PR_for_the_environment_is_open(self):
        for ref in ("release-prod-abc12345", "release-rollback-prod-abc12345"):
            p, fx = self.guard([], pulls=[{"head": {"ref": ref}, "html_url": "https://github.com/o/gitops-shop/pull/9"}])
            self.assertNotEqual(p.returncode, 0, ref)
            self.assertIn("pull/9", p.stderr)
            self.assertEqual(self.patched(fx), "")
        # another environment's PR, or one whose env merely starts the same, is not this environment's
        p, _ = self.guard([], pulls=[{"head": {"ref": "release-prod-eu-abc12345"}, "html_url": "u"}, {"head": {"ref": "feature-x"}, "html_url": "u"}])
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_the_guard_is_wired_before_a_change_request_or_a_branch_exists(self):
        self.assertLess(SCRIPT.index("reissue_guard || exit 1"), SCRIPT.index('workdir="$(mktemp -d)"'))
        self.assertLess(SCRIPT.index("reissue_guard || exit 1"), SCRIPT.index("git push --quiet --force"))

    # ---- close_stale_proposals never closes a release newer than the reissued one ----
    def test_close_stale_skips_a_release_newer_than_the_one_reissued(self):
        names = [f"release-tracking-{n}" for n in ("old2", "newer")]
        files = {"list.json": {"items": []}, f"cm-release-tracking-{OLD}.json": cm(f"release-tracking-{OLD}", {"prCreatedAt": "2026-11-10T10:00:00Z"}),
                 "cm-release-tracking-old2.json": cm("release-tracking-old2", {"state": "proposed", "prCreatedAt": "2026-11-05T00:00:00Z", "prUrl": "https://github.com/o/gitops-shop/pull/1"}),
                 "cm-release-tracking-newer.json": cm("release-tracking-newer", {"state": "proposed", "prCreatedAt": "2026-11-12T00:00:00Z", "prUrl": "https://github.com/o/gitops-shop/pull/2"})}
        fx = self.fixture(files)
        wrapper = os.path.join(self.tmp, "kc.sh")
        # `kubectl get configmap -l ... -o jsonpath=<names>` lists the proposed records
        put(os.path.join(fx, "list.json"), "\n".join(names))
        p = self.run_fn(fx, "close_stale_proposals")
        self.assertEqual(p.returncode, 0, p.stderr)
        patched = self.patched(fx)
        self.assertIn("release-tracking-old2", patched)
        self.assertNotIn("release-tracking-newer", patched)


if __name__ == "__main__":
    unittest.main()
