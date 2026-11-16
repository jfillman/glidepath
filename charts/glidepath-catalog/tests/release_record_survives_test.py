#!/usr/bin/env python3
"""The release record survives the calls after the PR (docs/admin/release-state.md, "The record survives the calls after the PR").

2026-11-16, backstage 1.0.2 to prod: open-release-pr created the change request, pushed the branch and opened the PR, then the
change tool's link call failed during a cluster overload and the step died before it wrote the release record. The PR merged and
deployed with no record. These tests run the WHOLE rendered open-release-pr step (bash, real jq and yq) against fakes for kubectl,
curl (GitHub and the change tool), git and sleep, with a failure injected at each network call after the PR exists, and re-runs
from the exact state that release was left in.

Run: python3 charts/glidepath-catalog/tests/release_record_survives_test.py   (needs helm, pyyaml, jq, yq, bash, python3)
Set GLIDEPATH_CATALOG to another copy of the chart to run the same tests against it (how "fails before" was checked).
"""
import json
import os
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.environ.get("GLIDEPATH_CATALOG", os.path.join(ROOT, "charts", "glidepath-catalog"))
CHAIN = "66666666-7777-4888-8999-aaaaaaaaaaaa"
OLD = "11111111-2222-4333-8444-555555555555"
PR_URL = "https://github.com/o/gitops-shop/pull/312"
RECORD = f"release-tracking-{CHAIN}"


def put(path, text):
    with open(path, "w") as f:
        f.write(text)


def render_script():
    cmd = ["helm", "template", "t", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), "--show-only", "templates/tasks/open-release-pr.yaml"]
    p = subprocess.run(cmd, capture_output=True, text=True)
    assert p.returncode == 0, p.stderr
    return yaml.safe_load(p.stdout)["spec"]["steps"][0]["script"]


FAKE_KUBECTL = r'''#!/usr/bin/env python3
import json, os, sys
fx = os.environ["FX"]; a = sys.argv[1:]
def log(s):
    open(fx + "/calls", "a").write(s + "\n")
def inject(key):
    spec = json.load(open(fx + "/inject.json")).get(key)
    if not spec: return False
    n = len(open(fx + "/count-" + key.replace(":", "_")).read().split()) if os.path.exists(fx + "/count-" + key.replace(":", "_")) else 0
    open(fx + "/count-" + key.replace(":", "_"), "a").write("x ")
    return n < spec["fail"]
def path(name): return fx + "/cm/" + name + ".json"
def load(name): return json.load(open(path(name))) if os.path.exists(path(name)) else None
def save(name, o): os.makedirs(fx + "/cm", exist_ok=True); json.dump(o, open(path(name), "w"))
def opt(flag):
    return a[a.index(flag) + 1] if flag in a else None
verb = a[0]
if verb == "get" and a[1] == "pipelinerun":
    if os.path.exists(fx + "/run.json"): print(open(fx + "/run.json").read()); sys.exit(0)
    sys.exit(1)
if verb == "get" and a[1] == "configmap":
    if a[2] == "cluster-registry": print(json.dumps({"data": {"hangar-prod": "x"}})); sys.exit(0)
    if a[2].startswith("-"):  # a label-selected list (close_stale_proposals)
        log("kubectl list")
        if opt("-o") == "json": print(json.dumps({"items": []})); sys.exit(0)
        if os.path.exists(fx + "/stale.txt"): print(open(fx + "/stale.txt").read(), end="")
        sys.exit(0)
    name = a[2]
    cm = load(name)
    if cm is None:
        sys.stderr.write('Error from server (NotFound): configmaps "%s" not found\n' % name); sys.exit(1)
    o = opt("-o")
    if o == "name": print("configmap/" + name)
    elif o and o.startswith("jsonpath"): print(cm["data"].get(o.split(".data.")[1].rstrip("}"), ""), end="")
    elif o == "json": print(json.dumps(cm))
    sys.exit(0)
if verb == "create":
    name = a[2]; data = {}
    for x in a:
        if x.startswith("--from-literal="):
            k, v = x[len("--from-literal="):].split("=", 1); data[k] = v
    print(json.dumps({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": name}, "data": data})); sys.exit(0)
if verb == "apply":
    log("kubectl apply")
    o = json.loads(sys.stdin.read())
    if inject("kubectl:apply"):
        sys.stderr.write("error: the connection to the server was refused\n"); sys.exit(1)
    name = o["metadata"]["name"]; cm = load(name) or {"metadata": {"name": name, "labels": {}}, "data": {}}
    cm["data"].update(o["data"]); save(name, cm); print("configmap/%s configured" % name); sys.exit(0)
if verb == "patch":
    name = a[2]; log("kubectl patch " + name)
    if inject("kubectl:patch"):
        sys.stderr.write("error: the connection to the server was refused\n"); sys.exit(1)
    cm = load(name)
    if cm is None: sys.exit(1)
    body = json.loads(opt("-p"))
    if opt("--type") == "merge":
        for k, v in body.get("data", {}).items():
            if v is None: cm["data"].pop(k, None)
            else: cm["data"][k] = v
    else:
        for op in body:
            if op["op"] == "add" and op["path"].startswith("/data/"): cm["data"][op["path"][6:]] = op["value"]
    save(name, cm); sys.exit(0)
if verb == "label":
    name = a[2]; log("kubectl label")
    cm = load(name)
    if cm is None: sys.exit(1)
    for x in a[3:]:
        if "=" in x and not x.startswith("-"): k, v = x.split("=", 1); cm["metadata"]["labels"][k] = v
    save(name, cm); sys.exit(0)
sys.exit(0)
'''

FAKE_CURL = r'''#!/usr/bin/env python3
import json, os, sys
fx = os.environ["FX"]; a = sys.argv[1:]
def log(s): open(fx + "/calls", "a").write(s + "\n")
def inject(key):
    spec = json.load(open(fx + "/inject.json")).get(key)
    if not spec: return None
    f = fx + "/count-" + key.replace(":", "_")
    n = len(open(f).read().split()) if os.path.exists(f) else 0
    open(f, "a").write("x ")
    return spec if n < spec["fail"] else None
url = [x for x in a if x.startswith("http")][-1]
method = a[a.index("-X") + 1] if "-X" in a else "GET"
data = a[a.index("-d") + 1] if "-d" in a else ""
pr = fx + "/pr.json"
def pr_obj(): return json.load(open(pr)) if os.path.exists(pr) else None
if url == os.environ["ITSM_WEBHOOK_URL"]:
    act = json.loads(data)["action"]; log("itsm " + act)
    spec = inject("itsm:" + act)
    if spec:
        if spec.get("http", "000") == "000":
            sys.stderr.write("curl: (7) Failed to connect to itsm-mock port 8080 after 5 ms: Couldn't connect to server\n"); sys.stdout.write("\n000"); sys.exit(7)
        sys.stdout.write("denied\n" + spec["http"]); sys.exit(0)
    sys.stdout.write(json.dumps({"id": "chg-1", "number": "CHG0010034", "url": "https://itsm.test/chg-1", "state": "new", "approval": "requested", "approved": False}) + "\n200")
    sys.exit(0)
if "/pulls?state=open" in url:  # the reissue guard's list of open PRs
    print("[]"); sys.exit(0)
if "/pulls?head=" in url:
    log("curl GET pulls?head"); spec = inject("github:list")
    if spec: sys.stderr.write("curl: (7) Failed to connect\n"); sys.exit(7)
    print(json.dumps([pr_obj()] if pr_obj() else [])); sys.exit(0)
if method == "POST" and url.endswith("/pulls"):
    log("curl POST pulls"); spec = inject("github:post")
    body = json.loads(data)
    if spec and spec.get("lost"):  # GitHub created the PR, the answer was lost
        json.dump({"html_url": os.environ["PR_URL"], "number": 312, "created_at": "2026-11-16T12:00:00Z", "body": body["body"]}, open(pr, "w"))
    if spec: sys.stderr.write("curl: (22) The requested URL returned error: 502\n"); sys.exit(22)
    p = {"html_url": os.environ["PR_URL"], "number": 312, "created_at": "2026-11-16T12:00:00Z", "body": body["body"]}
    json.dump(p, open(pr, "w")); print(json.dumps(p)); sys.exit(0)
if method == "PATCH":
    log("curl PATCH pull"); spec = inject("github:patch")
    if spec: sys.stderr.write("curl: (7) Failed to connect\n"); sys.exit(7)
    p = pr_obj(); p["body"] = json.loads(data)["body"]; json.dump(p, open(pr, "w")); sys.exit(0)
if "/pulls/" in url:  # close_stale_proposals reading an older PR
    log("curl GET pull"); spec = inject("github:get")
    if spec: sys.stderr.write("curl: (7) Failed to connect\n"); sys.exit(7)
    print(json.dumps({"state": "closed", "merged": False})); sys.exit(0)
sys.exit(22)
'''

FAKE_GIT = r'''#!/usr/bin/env bash
echo "git $1" >> "$FX/calls"
case "$1" in
  clone) dest="${@: -1}"; mkdir -p "$dest/hangar-prod/prod"; printf 'release: {}\n' > "$dest/hangar-prod/prod/values.yaml" ;;
esac
exit 0
'''


class Survives(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        script = render_script()
        script = script.replace("$(results.pr-url.path)", '"$RESULTS/pr-url"').replace("$(results.pr-created-at.path)", '"$RESULTS/pr-created-at"') \
            .replace("$(results.change-request-number.path)", '"$RESULTS/change"')
        # the tee targets are quoted by the script itself: normalise to a plain path
        script = script.replace('""$RESULTS/pr-url""', '"$RESULTS/pr-url"').replace('""$RESULTS/pr-created-at""', '"$RESULTS/pr-created-at"').replace('""$RESULTS/change""', '"$RESULTS/change"')
        cls.script = os.path.join(cls.tmp, "step.sh")
        put(cls.script, script)
        cls.bin = os.path.join(cls.tmp, "bin")
        os.makedirs(cls.bin)
        for name, body in (("kubectl", FAKE_KUBECTL), ("curl", FAKE_CURL), ("git", FAKE_GIT)):
            put(os.path.join(cls.bin, name), body)
        put(os.path.join(cls.bin, "sleep"), '#!/usr/bin/env bash\necho "sleep $1" >> "$FX/calls"\n')
        put(os.path.join(cls.bin, "date"), '#!/usr/bin/env bash\ncase "$*" in *%N*) echo 2026-11-16T12:00:00.000000000Z ;; *) echo 2026-11-16T12:00:00Z ;; esac\n')
        for n in os.listdir(cls.bin):
            os.chmod(os.path.join(cls.bin, n), 0o755)
        cls.lib = os.path.join(cls.tmp, "lib")
        os.makedirs(cls.lib)
        put(os.path.join(cls.lib, "github-app.sh"), 'github_app_installation_token() { echo tok; }\n')
        put(os.path.join(cls.lib, "retry.sh"), 'with_retry() { shift; [[ "$1" == "--" ]] && shift; "$@"; }\n')

    def fixture(self, inject=None, pr=None, record=None, stale=None, annotations=None, itsm="webhook"):
        fx = tempfile.mkdtemp(dir=self.tmp)
        os.makedirs(os.path.join(fx, "cm"))
        put(os.path.join(fx, "inject.json"), json.dumps(inject or {}))
        put(os.path.join(fx, "calls"), "")
        os.makedirs(os.path.join(fx, "results"))
        if pr:
            put(os.path.join(fx, "pr.json"), json.dumps(pr))
        if record is not None:
            put(os.path.join(fx, "cm", f"{RECORD}.json"), json.dumps({"metadata": {"name": RECORD, "labels": {}}, "data": record}))
        if stale:
            put(os.path.join(fx, "stale.txt"), stale)
        if annotations is not None:
            put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": annotations}}))
        self.itsm = itsm
        return fx

    def run_step(self, fx, **env_extra):
        env = dict(os.environ, FX=fx, RESULTS=os.path.join(fx, "results"), PATH=self.bin + os.pathsep + os.environ["PATH"], PLATFORM_LIB=self.lib,
                   PR_URL=PR_URL, LATE_ATTEMPTS="3", LATE_BACKOFF_SECONDS="2",
                   ITSM_PROVIDER=self.itsm, ITSM_WEBHOOK_URL="https://itsm.test/webhook", ITSM_CREDENTIALS_DIR=os.path.join(fx, "no-creds"),
                   CHANGE_REQUEST_ID="", IMAGE_REF="ghcr.io/o/shop:1.0.2-7150a18", IMAGE_DIGEST="none", GIT_URL="https://github.com/o/shop",
                   APP_NAME="shop", GIT_REVISION="7150a186aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ENV="prod", CLUSTER="hangar-prod",
                   APP_NAMESPACE="app-shop", CHAIN_ID=CHAIN, FLOW_START_TIME="2026-11-16T11:00:00Z", CONFIG_JSON="{}", INITIATOR="",
                   RELEASE_KIND="promote", ROLLBACK_OF="", ROLLBACK_REASON="", REF_PROTECTION="verified", PIPELINE_RUN_NAME="run1",
                   NAMESPACE="app-shop-cicd", GITHUB_TOKEN_BROKER_URL="http://broker")
        env.update(env_extra)
        return subprocess.run(["bash", self.script], capture_output=True, text=True, env=env, cwd=fx)

    def calls(self, fx):
        with open(os.path.join(fx, "calls")) as f:
            return f.read().split("\n")[:-1]

    def record(self, fx):
        path = os.path.join(fx, "cm", f"{RECORD}.json")
        if not os.path.exists(path):
            return None
        with open(path) as f:
            return json.load(f)["data"]

    def existing_pr(self, body="**App**: shop\n<!-- glidepath:change-request:chg-1 -->"):
        return {"html_url": PR_URL, "number": 312, "created_at": "2026-11-16T09:00:00Z", "body": body}

    # ---- a normal run ----
    GOLDEN = {
        "prUrl": PR_URL, "prCreatedAt": "2026-11-16T12:00:00.000000000Z", "releaseId": f"{CHAIN}:hangar-prod/prod", "kind": "promote",
        "image": "ghcr.io/o/shop:1.0.2-7150a18", "rollbackOf": "", "rollbackReason": "", "refProtection": "verified", "originChainId": "",
        "originChainSlug": "", "originTraceId": "", "reissueOf": "", "appNamespace": "app-shop", "appName": "shop", "env": "prod",
        "cluster": "hangar-prod", "gitUrl": "https://github.com/o/shop", "gitRevision": "7150a186aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "flowStartTime": "2026-11-16T11:00:00Z", "configJson": "{}", "changeRequestId": "chg-1", "changeRequestNumber": "CHG0010034",
        "changeRequestUrl": "https://itsm.test/chg-1", "changeRequestState": "new", "state": "proposed", "stateAt": "2026-11-16T12:00:00Z",
    }

    def test_a_normal_run_writes_the_same_record_and_the_record_comes_before_the_change_link(self):
        fx = self.fixture()
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.record(fx), self.GOLDEN)
        calls = self.calls(fx)
        # the record is the first network call after the PR is opened; the change link comes after it
        self.assertLess(calls.index("curl POST pulls"), calls.index("kubectl apply"))
        self.assertLess(calls.index("kubectl apply"), calls.index("itsm link"))
        between = [c for c in calls[calls.index("curl POST pulls") + 1:calls.index("kubectl apply")] if c.startswith(("curl", "itsm"))]
        self.assertEqual(between, [])
        with open(os.path.join(fx, "results", "pr-url")) as f:
            self.assertEqual(f.read().strip(), PR_URL)

    # ---- a failure at each call after the PR exists ----
    def test_a_change_tool_that_stays_down_cannot_take_the_record(self):
        fx = self.fixture({"itsm:link": {"fail": 99}})
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        rec = self.record(fx)
        self.assertEqual(rec["prUrl"], PR_URL)
        self.assertEqual(rec["state"], "proposed")
        self.assertIn("did not take the PR link for change CHG0010034", rec["postPrError"])
        self.assertEqual(self.calls(fx).count("itsm link"), 3)  # bounded
        self.assertEqual([c for c in self.calls(fx) if c.startswith("sleep")], ["sleep 2", "sleep 4"])  # with backoff
        self.assertIn("error: the change tool did not take the PR link", p.stderr)
        self.assertEqual(self.calls(fx).count("curl POST pulls"), 1)

    def test_a_transient_change_tool_failure_is_retried_and_leaves_nothing_on_the_record(self):
        fx = self.fixture({"itsm:link": {"fail": 2}})
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.calls(fx).count("itsm link"), 3)
        self.assertEqual(self.record(fx), self.GOLDEN)

    def test_a_definite_4xx_from_the_change_tool_is_not_retried_but_is_recorded(self):
        fx = self.fixture({"itsm:link": {"fail": 99, "http": "403"}})
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.calls(fx).count("itsm link"), 1)
        self.assertIn("HTTP 403", self.record(fx)["postPrError"])

    def test_a_record_write_that_fails_once_is_retried(self):
        fx = self.fixture({"kubectl:apply": {"fail": 1}})
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.record(fx), self.GOLDEN)

    def test_a_record_that_cannot_be_written_fails_the_step_naming_the_PR_before_any_other_call(self):
        fx = self.fixture({"kubectl:apply": {"fail": 99}})
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn(PR_URL, p.stderr)
        self.assertIn("Re-run this release", p.stderr)
        self.assertNotIn("itsm link", self.calls(fx))

    def test_close_stale_and_link_reissued_failing_cannot_take_the_record(self):
        old = f"release-tracking-{OLD}"
        put(os.path.join(self.tmp, "x"), "")
        fx = self.fixture({"github:get": {"fail": 99}, "kubectl:patch": {"fail": 99}}, stale=old + "\n", annotations={"glidepath.io/reissue-of": OLD})
        put(os.path.join(fx, "cm", old + ".json"), json.dumps({"metadata": {"name": old, "labels": {"hangar.io/app": "shop", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"}},
                                                               "data": {"state": "proposed", "prUrl": "https://github.com/o/gitops-shop/pull/1", "prCreatedAt": "2026-11-10T00:00:00Z"}}))
        p = self.run_step(fx)
        # the reissue guard refuses a newer or open PR; nothing of that kind exists here
        self.assertEqual(p.returncode, 0, p.stderr)
        rec = self.record(fx)
        self.assertEqual(rec["prUrl"], PR_URL)
        self.assertEqual(rec["reissueOf"], OLD)

    # ---- no orphan: nothing claims a PR that does not exist ----
    def test_a_PR_that_could_not_be_opened_leaves_no_record(self):
        fx = self.fixture({"github:post": {"fail": 99}})
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIsNone(self.record(fx))
        self.assertNotIn("kubectl apply", self.calls(fx))

    def test_a_PR_call_whose_answer_was_lost_finds_the_PR_and_records_it(self):
        fx = self.fixture({"github:post": {"fail": 99, "lost": True}})
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.record(fx)["prUrl"], PR_URL)
        self.assertEqual(self.calls(fx).count("curl POST pulls"), 1)

    # ---- re-runs ----
    def test_a_re_run_from_PR_open_change_created_no_record_completes_the_job(self):
        fx = self.fixture(pr=self.existing_pr())
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        calls = self.calls(fx)
        self.assertNotIn("curl POST pulls", calls)
        self.assertNotIn("itsm create", calls)
        self.assertNotIn("git push", calls)
        self.assertNotIn("git clone", calls)
        rec = self.record(fx)
        self.assertEqual(rec["prUrl"], PR_URL)
        self.assertEqual(rec["prCreatedAt"], "2026-11-16T09:00:00Z")  # the PR's own, a retry's lead-time anchor
        self.assertEqual((rec["changeRequestId"], rec["changeRequestNumber"], rec["state"]), ("chg-1", "CHG0010034", "proposed"))
        self.assertNotIn("postPrError", rec)
        self.assertEqual(calls.count("itsm link"), 1)

    def test_a_re_run_with_the_change_tool_still_down_writes_the_record_first_and_keeps_it(self):
        fx = self.fixture({"itsm:get": {"fail": 99}, "itsm:link": {"fail": 99}}, pr=self.existing_pr())
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        rec = self.record(fx)
        self.assertEqual((rec["prUrl"], rec["changeRequestId"], rec["state"]), (PR_URL, "chg-1", "proposed"))
        self.assertIn("postPrError", rec)
        calls = self.calls(fx)
        self.assertLess(calls.index("kubectl apply"), calls.index("itsm get"))  # before any call to the change tool

    def test_a_re_run_that_finds_the_change_created_nowhere_still_keeps_the_record_but_fails(self):
        fx = self.fixture({"itsm:create": {"fail": 99}}, pr=self.existing_pr(body="**App**: shop"))
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertEqual(self.record(fx)["prUrl"], PR_URL)
        self.assertIn("no change request could be created or read", self.record(fx)["postPrError"])
        self.assertEqual(self.calls(fx).count("itsm create"), 1)  # a create is never sent twice

    def test_a_re_run_never_resets_a_record_the_relay_moved_on(self):
        data = dict(self.GOLDEN, state="progressing", stateAt="2026-11-16T12:30:00Z", postPrError="old failure", mergedAt="2026-11-16T12:20:00Z")
        fx = self.fixture(pr=self.existing_pr(), record=data)
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        rec = self.record(fx)
        self.assertEqual((rec["state"], rec["stateAt"], rec["mergedAt"]), ("progressing", "2026-11-16T12:30:00Z", "2026-11-16T12:20:00Z"))
        self.assertNotIn("postPrError", rec)  # the earlier failure healed
        self.assertEqual(self.calls(fx).count("itsm link"), 1)  # and the link it missed was retried

    def test_running_the_release_twice_opens_one_PR_and_one_change_and_one_record(self):
        fx = self.fixture()
        self.assertEqual(self.run_step(fx).returncode, 0)
        first = self.record(fx)
        self.assertEqual(self.run_step(fx).returncode, 0)
        calls = self.calls(fx)
        self.assertEqual(calls.count("curl POST pulls"), 1)
        self.assertEqual(calls.count("itsm create"), 1)
        again = self.record(fx)
        for k in ("prCreatedAt",):  # a re-run takes the PR's own created_at
            first.pop(k), again.pop(k)
        self.assertEqual(again, first)


if __name__ == "__main__":
    unittest.main()
