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
    if name.startswith("release-tracking-") and inject("kubectl:get"):
        sys.stderr.write("Unable to connect to the server: dial tcp: connection refused\n"); sys.exit(1)
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
    # apply is three-way: a key the last apply set and this one does not carry is deleted, a key only others set stays
    for k in cm.get("applied", []):
        if k not in o["data"]: cm["data"].pop(k, None)
    cm["data"].update(o["data"]); cm["applied"] = list(o["data"]); save(name, cm); print("configmap/%s configured" % name); sys.exit(0)
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
    after = spec.get("after", 0)
    return spec if after <= n < after + spec["fail"] else None
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
        sys.stdout.write(spec.get("body", "denied") + "\n" + spec["http"]); sys.exit(0)
    sys.stdout.write(json.dumps({"id": "chg-1", "number": "CHG0010034", "url": "https://itsm.test/chg-1", "state": "new", "approval": "requested", "approved": False}) + "\n200")
    sys.exit(0)
if "/pulls?state=open" in url:  # the reissue guard's list of open PRs
    print("[]"); sys.exit(0)
if "/contents/" in url:  # the release file at a ref: commit.yaml (a PR head), current.yaml (main), else 404
    log("curl GET contents")
    if inject("github:contents"): sys.stderr.write("curl: (7) Failed to connect\n"); sys.exit(7)
    ref = url.split("ref=")[1]
    f = fx + ("/current.yaml" if ref == "main" else "/commit.yaml")
    if "values.yaml" in url or not os.path.exists(f): sys.stdout.write('{"message":"Not Found"}\n404'); sys.exit(0)
    sys.stdout.write(open(f).read() + "\n200"); sys.exit(0)
if "/pulls?head=" in url:
    log("curl GET pulls?head"); spec = inject("github:list")
    if spec: sys.stderr.write("curl: (7) Failed to connect\n"); sys.exit(7)
    extra = json.load(open(fx + "/others.json")) if os.path.exists(fx + "/others.json") else []
    print(json.dumps(([pr_obj()] if pr_obj() else []) + extra)); sys.exit(0)
if method == "POST" and "/comments" in url:
    log("curl POST comment"); spec = inject("github:comment")
    if spec: sys.stderr.write("curl: (7) Failed to connect\n"); sys.exit(7)
    open(fx + "/comment.txt", "w").write(json.loads(data)["body"]); sys.exit(0)
if method == "POST" and url.endswith("/pulls"):
    log("curl POST pulls"); spec = inject("github:post")
    body = json.loads(data)
    if spec and spec.get("malformed"): print("{}"); sys.exit(0)
    if spec and spec.get("lost"):  # GitHub created the PR, the answer was lost
        json.dump({"html_url": os.environ["PR_URL"], "number": 312, "state": "open", "merged_at": None, "head": {"sha": "abc123"}, "created_at": "2026-11-16T12:00:00Z", "body": body["body"]}, open(pr, "w"))
    if spec: sys.stderr.write("curl: (22) The requested URL returned error: 502\n"); sys.exit(22)
    p = {"html_url": os.environ["PR_URL"], "number": 312, "state": "open", "merged_at": None, "head": {"sha": "abc123"}, "created_at": "2026-11-16T12:00:00Z", "body": body["body"]}
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

    def fixture(self, inject=None, pr=None, record=None, stale=None, annotations=None, itsm="webhook", others=None, commit_chain=None, chain=CHAIN, commit_id=None, current_tag="1.0.2-7150a18", commit_tag="1.0.2-7150a18"):
        fx = tempfile.mkdtemp(dir=self.tmp)
        os.makedirs(os.path.join(fx, "cm"))
        put(os.path.join(fx, "inject.json"), json.dumps(inject or {}))
        put(os.path.join(fx, "calls"), "")
        os.makedirs(os.path.join(fx, "results"))
        if pr:
            put(os.path.join(fx, "pr.json"), json.dumps(pr))
        if record is not None:
            put(os.path.join(fx, "cm", f"{RECORD}.json"), json.dumps({"metadata": {"name": RECORD, "labels": {"hangar.io/app": "shop", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"}}, "data": record, "applied": list(record)}))
        if others is not None:
            put(os.path.join(fx, "others.json"), json.dumps(others))
        rid = commit_id or (f"{commit_chain}:hangar-prod/prod" if commit_chain else None)
        if rid:
            put(os.path.join(fx, "commit.yaml"), f"release:\n  image:\n    tag: {commit_tag}\nreleaseTracking:\n  releaseId: {rid}\n")
            put(os.path.join(fx, "current.yaml"), f"release:\n  image:\n    tag: {current_tag}\nreleaseTracking:\n  releaseId: {rid}\n")
        if stale:
            put(os.path.join(fx, "stale.txt"), stale)
        if annotations is not None:
            put(os.path.join(fx, "run.json"), json.dumps({"metadata": {"annotations": annotations}}))
        self.itsm = itsm
        self.chain = chain
        return fx

    def run_step(self, fx, **env_extra):
        env = dict(os.environ, FX=fx, RESULTS=os.path.join(fx, "results"), PATH=self.bin + os.pathsep + os.environ["PATH"], PLATFORM_LIB=self.lib,
                   PR_URL=PR_URL, LATE_ATTEMPTS="3", LATE_BACKOFF_SECONDS="2",
                   ITSM_PROVIDER=self.itsm, ITSM_WEBHOOK_URL="https://itsm.test/webhook", ITSM_CREDENTIALS_DIR=os.path.join(fx, "no-creds"),
                   CHANGE_REQUEST_ID="", IMAGE_REF="ghcr.io/o/shop:1.0.2-7150a18", IMAGE_DIGEST="none", GIT_URL="https://github.com/o/shop",
                   APP_NAME="shop", GIT_REVISION="7150a186aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ENV="prod", CLUSTER="hangar-prod",
                   APP_NAMESPACE="app-shop", CHAIN_ID=self.chain, FLOW_START_TIME="2026-11-16T11:00:00Z", CONFIG_JSON="{}", INITIATOR="",
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
        return {"html_url": PR_URL, "number": 312, "state": "open", "merged_at": None, "head": {"sha": "abc123"}, "created_at": "2026-11-16T09:00:00Z", "body": body}

    def merged_pr(self, merged_at="2026-11-16T11:00:00Z", body="**App**: shop\n<!-- glidepath:change-request:chg-1 -->"):
        return {"html_url": PR_URL, "number": 312, "state": "closed", "merged_at": merged_at, "base": {"ref": "main"}, "head": {"sha": "abc123"}, "created_at": "2026-11-16T09:00:00Z", "body": body}

    # ---- a normal run ----
    GOLDEN = {
        "prUrl": PR_URL, "prCreatedAt": "2026-11-16T12:00:00.000000000Z", "releaseId": f"{CHAIN}:hangar-prod/prod", "kind": "promote",
        "image": "ghcr.io/o/shop:1.0.2-7150a18", "imageDigest": "", "imageRepository": "ghcr.io/o/shop", "imageSource": "", "initiator": "", "rollbackOf": "", "rollbackReason": "", "refProtection": "verified", "originChainId": "",
        "originChainSlug": "", "originTraceId": "", "reissueOf": "", "appNamespace": "app-shop", "appName": "shop", "env": "prod",
        "cluster": "hangar-prod", "gitUrl": "https://github.com/o/shop", "gitRevision": "7150a186aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "flowStartTime": "2026-11-16T11:00:00Z", "configJson": "{}", "changeRequestId": "chg-1", "changeRequestNumber": "CHG0010034",
        "changeRequestUrl": "https://itsm.test/chg-1", "changeRequestState": "new", "changeLinkedAt": "2026-11-16T12:00:00Z", "state": "proposed", "stateAt": "2026-11-16T12:00:00Z",
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
        self.assertIn("the change request could not be linked to this PR", rec["postPrError"])
        self.assertTrue(rec["postPrError"].startswith("[connection refused]"))
        self.assertEqual(self.calls(fx).count("itsm link"), 3)  # bounded
        self.assertEqual([c for c in self.calls(fx) if c.startswith("sleep")], ["sleep 2", "sleep 4"])  # with backoff
        self.assertIn("error: the change request could not be linked to this PR", p.stderr)
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

    # ---- item: the relay-moved record is completed with a patch, never an apply that drops keys ----
    def test_a_re_run_completes_an_existing_record_with_a_patch_not_an_apply(self):
        data = dict(self.GOLDEN, state="healthy", stateAt="2026-11-16T12:30:00Z")
        fx = self.fixture(pr=self.existing_pr(), record=data)
        self.assertEqual(self.run_step(fx).returncode, 0)
        calls = self.calls(fx)
        self.assertNotIn("kubectl apply", calls)
        self.assertIn(f"kubectl patch {RECORD}", calls)
        self.assertEqual(self.record(fx)["state"], "healthy")

    def test_a_failing_read_of_the_existing_record_is_not_taken_for_a_missing_one(self):
        data = dict(self.GOLDEN, state="progressing")
        fx = self.fixture({"kubectl:get": {"fail": 99}}, pr=self.existing_pr(), record=data)
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        calls = self.calls(fx)
        self.assertNotIn("kubectl apply", calls)
        self.assertEqual(self.record(fx)["state"], "progressing")

    # ---- a PR already merged: a re-run refuses, changing nothing; what is not this release is not refused ----
    def refused(self, fx):
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("is already merged", p.stderr)
        self.assertIn(PR_URL, p.stderr)
        calls = self.calls(fx)
        for c in ("curl POST pulls", "itsm create", "itsm link", "git push", "git clone", "kubectl apply"):
            self.assertNotIn(c, calls)
        self.assertIsNone(self.record(fx))

    def test_a_re_run_whose_PR_is_already_merged_is_refused_with_no_side_effects(self):
        self.refused(self.fixture(others=[self.merged_pr()], commit_chain=CHAIN))

    def test_a_merged_PR_of_the_same_branch_name_for_another_cluster_is_not_this_release(self):
        fx = self.fixture(others=[self.merged_pr()], commit_id=f"{CHAIN}:other-cluster/prod")
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertIn("curl POST pulls", self.calls(fx))

    def test_a_merged_PR_carrying_another_image_is_not_this_release(self):
        fx = self.fixture(others=[self.merged_pr()], commit_chain=CHAIN, commit_tag="1.0.1-aaaaaaa")
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertIn("curl POST pulls", self.calls(fx))

    def test_a_merged_PR_into_another_base_is_not_this_release(self):
        other = dict(self.merged_pr(), base={"ref": "release-x"})
        fx = self.fixture(others=[other], commit_chain=CHAIN)
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertIn("curl POST pulls", self.calls(fx))

    def test_a_merged_PR_the_environment_has_moved_on_from_is_not_this_release(self):
        # promoted again after a rollback: main's release file carries another tag now
        fx = self.fixture(others=[self.merged_pr()], commit_chain=CHAIN, current_tag="0.9.0-bbbbbbb")
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertIn("curl POST pulls", self.calls(fx))

    def test_a_merged_PR_whose_release_file_cannot_be_read_fails_the_run(self):
        fx = self.fixture({"github:contents": {"fail": 99}}, others=[self.merged_pr()], commit_chain=CHAIN)
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertNotIn("curl POST pulls", self.calls(fx))

    def test_a_PR_closed_unmerged_is_never_reopened_and_a_new_PR_follows_the_normal_path(self):
        closed = dict(self.merged_pr(), merged_at=None)
        fx = self.fixture(others=[closed])
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.calls(fx).count("curl POST pulls"), 1)
        self.assertEqual(self.record(fx)["state"], "proposed")

    # ---- the chain id of a re-run ----
    def test_a_re_run_with_a_new_chain_id_completes_the_record_under_the_original_chain(self):
        other = "99999999-0000-4000-8000-000000000000"
        fx = self.fixture(pr=self.existing_pr(), commit_chain=CHAIN, chain=other)
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.record(fx)["releaseId"], f"{CHAIN}:hangar-prod/prod")
        self.assertFalse(os.path.exists(os.path.join(fx, "cm", f"release-tracking-{other}.json")))

    # ---- the chain id in the PR's commit is not trusted blindly ----
    def refused_chain(self, **kw):
        fx = self.fixture(pr=self.existing_pr(), chain="99999999-0000-4000-8000-000000000000", **kw)
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("refused", p.stderr)
        self.assertNotIn("kubectl apply", self.calls(fx))
        self.assertNotIn("kubectl patch " + RECORD, self.calls(fx))
        self.assertNotIn("itsm get", self.calls(fx))
        return fx

    def test_an_edited_releaseId_pointing_at_another_apps_chain_is_refused(self):
        foreign = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
        fx = self.fixture(pr=self.existing_pr(), chain="99999999-0000-4000-8000-000000000000", commit_chain=foreign)
        put(os.path.join(fx, "cm", f"release-tracking-{foreign}.json"), json.dumps({"metadata": {"name": f"release-tracking-{foreign}", "labels": {"hangar.io/app": "other", "hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"}}, "data": {"changeRequestId": "their-change"}}))
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("another app, environment or cluster", p.stderr)
        self.assertNotIn("itsm get", self.calls(fx))
        self.assertNotIn("kubectl apply", self.calls(fx))

    def test_a_releaseId_for_another_cluster_or_environment_is_refused(self):
        self.refused_chain(commit_id=f"{CHAIN}:hangar-prod/staging")
        self.refused_chain(commit_id=f"{CHAIN}:other/prod")

    def test_an_uppercase_chain_id_is_refused(self):
        self.refused_chain(commit_chain=CHAIN.upper())

    def test_a_release_file_that_cannot_be_read_fails_the_run_and_does_not_fall_back(self):
        fx = self.fixture({"github:contents": {"fail": 99}}, pr=self.existing_pr())
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("re-run this release later", p.stderr)
        self.assertIsNone(self.record(fx))

    def test_a_readable_release_file_without_a_releaseId_falls_back_to_this_runs_chain(self):
        fx = self.fixture(pr=self.existing_pr())  # contents answer 404
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.record(fx)["releaseId"], f"{CHAIN}:hangar-prod/prod")

    # ---- empty values never blank a record ----
    def test_a_re_run_that_cannot_read_its_annotations_does_not_blank_what_the_record_holds(self):
        data = dict(self.GOLDEN, originChainId="aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", originChainSlug="quiet-falcon", reissueOf=OLD)
        fx = self.fixture(pr=self.existing_pr(), record=data)  # no run.json: the run cannot be read
        self.assertEqual(self.run_step(fx).returncode, 0)
        rec = self.record(fx)
        self.assertEqual((rec["originChainId"], rec["originChainSlug"], rec["reissueOf"]), (data["originChainId"], "quiet-falcon", OLD))

    # ---- the failure is where a person sees it; the link is retried until taken ----
    def test_a_failed_link_is_commented_on_the_PR_once(self):
        fx = self.fixture({"itsm:link": {"fail": 99}})
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertEqual(self.calls(fx).count("curl POST comment"), 1)
        with open(os.path.join(fx, "comment.txt")) as f:
            self.assertIn("The release itself is unaffected", f.read())
        self.assertNotIn("changeLinkedAt", self.record(fx))

    def test_a_comment_that_cannot_be_posted_does_not_fail_the_step(self):
        fx = self.fixture({"itsm:link": {"fail": 99}, "github:comment": {"fail": 99}})
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertIn("postPrError", self.record(fx))

    def test_a_re_run_retries_the_link_while_the_record_has_no_link_noted(self):
        data = {k: v for k, v in self.GOLDEN.items() if k != "changeLinkedAt"}  # no postPrError either
        fx = self.fixture(pr=self.existing_pr(), record=data)
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertEqual(self.calls(fx).count("itsm link"), 1)
        self.assertIn("changeLinkedAt", self.record(fx))
        # and once noted, a further re-run does not link again
        fx2 = self.fixture(pr=self.existing_pr(), record=self.GOLDEN)
        self.assertEqual(self.run_step(fx2).returncode, 0)
        self.assertEqual(self.calls(fx2).count("itsm link"), 0)

    # ---- what is said when the PR call and its lookup disagree ----
    def test_a_PR_answer_that_is_not_a_PR_is_a_failed_call(self):
        fx = self.fixture({"github:post": {"fail": 99, "malformed": True}})
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIsNone(self.record(fx))
        self.assertNotIn("/null", p.stdout + p.stderr)

    def test_when_the_call_and_the_lookup_both_fail_it_says_it_does_not_know(self):
        fx = self.fixture({"github:post": {"fail": 99}, "github:list": {"fail": 99, "after": 1}})
        p = self.run_step(fx)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("could not confirm whether the release PR was opened", p.stderr)
        self.assertNotIn("was not opened: the call failed and GitHub lists no open PR", p.stderr)
        self.assertIsNone(self.record(fx))

    def test_a_re_run_with_no_body_marker_reads_the_change_the_record_holds_and_creates_none(self):
        # the first attempt saved change chg-1 on the record but its PR-body update failed: the body has no marker
        fx = self.fixture(pr=self.existing_pr(body="**App**: shop"), record=dict(self.GOLDEN, state="proposed"))
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertNotIn("itsm create", self.calls(fx))
        self.assertEqual(self.record(fx)["changeRequestId"], "chg-1")

    # ---- the PR comment carries a reason class only ----
    def test_the_PR_comment_carries_a_reason_class_and_no_text_from_the_tool(self):
        fx = self.fixture({"itsm:link": {"fail": 99, "http": "500", "body": "SECRETBODY " + "x" * 700}})
        p = self.run_step(fx)
        self.assertEqual(p.returncode, 0, p.stderr)
        with open(os.path.join(fx, "comment.txt")) as f:
            comment = f.read()
        self.assertIn("(HTTP 500)", comment)
        for leak in ("SECRETBODY", "itsm.test", "xxxx"):
            self.assertNotIn(leak, comment)
        self.assertLessEqual(len(self.record(fx)["postPrError"]), 600)
        self.assertTrue(self.record(fx)["postPrError"].startswith("[HTTP 500]"))
        self.assertEqual(self.calls(fx).count("curl POST comment"), 1)

    def test_a_re_run_that_fails_the_same_way_does_not_comment_again(self):
        fx = self.fixture({"itsm:link": {"fail": 99, "http": "500", "body": "b" * 700}})
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertEqual(self.run_step(fx).returncode, 0)  # the PR is open now: a re-run
        self.assertEqual(self.run_step(fx).returncode, 0)
        self.assertEqual(self.calls(fx).count("curl POST comment"), 1)


if __name__ == "__main__":
    unittest.main()
