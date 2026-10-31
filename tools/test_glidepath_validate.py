#!/usr/bin/env python3
"""Tests for tools/glidepath-validate, tools/cicd_scope.py and tools/gen_contract.py.

    python3 tools/test_glidepath_validate.py

Every rule gets a file that must trip it, and the real files (docs/user/examples/) must trip none,
so a rule that never fires can't pass for one that works. Needs: pyyaml, jsonschema.
"""
import copy
import json
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
VALIDATE = ROOT / "tools" / "glidepath-validate"
sys.path.insert(0, str(ROOT / "tools"))
import cicd_scope  # noqa: E402
import gen_contract  # noqa: E402

CONTRACT = json.loads((ROOT / "contract" / "glidepath-contract.json").read_text())
SCOPE = CONTRACT["scope"]

BASE = yaml.safe_load("""
apiVersion: platform/v1
kind: PipelineConfig
build: {agent: nodejs-20, cache: {enabled: false, size: small}}
deploy:
  environments:
    - {name: dev, tier: ground}
    - {name: int, tier: ground}
    - {name: prod, tier: flight, cluster: hangar-prod, production: true}
governance: {sast: true, imageScan: true, policyCheck: true, sbom: false, githubMerge: {mode: merger, requiredApprovals: 1}}
secrets: [{name: NPM_TOKEN, key: npm-token}]
pipelines:
  ci:
    trigger: {source: git, event: push, branch: main}
    steps:
      - {stage: build}
      - {stage: deploy, env: dev}
      - {stage: test, env: dev}
      - {stage: test, env: dev, role: check-ride}
      - {stage: release, env: prod}
      - {stage: gitops-image-bump, gitopsRepo: gitops-x, manifestPath: a/b.yaml}
""")

# What Airframe's AgentRepository (or any configuration-only repo) writes: no build, so PR checks only.
CHECKS_ONLY = {"apiVersion": "platform/v1", "kind": "PipelineConfig"}


def run(doc, *args, base=None):
    with tempfile.TemporaryDirectory() as d:
        f = Path(d) / "cicd.yaml"
        f.write_text(yaml.safe_dump(doc) if not isinstance(doc, str) else doc)
        extra = []
        if base is not None:
            b = Path(d) / "base.yaml"
            b.write_text(yaml.safe_dump(base))
            extra = ["--agent", "--base", str(b)]
        res = subprocess.run([sys.executable, str(VALIDATE), str(f), "--format", "json", *args, *extra], capture_output=True, text=True)
        out = json.loads(res.stdout)
        return res.returncode, [p["rule"] for p in out["problems"]], out


def edit(fn):
    doc = copy.deepcopy(BASE)
    fn(doc)
    return doc


class Schema(unittest.TestCase):
    def test_base_is_valid(self):
        code, rules, _ = run(BASE)
        self.assertEqual((code, rules), (0, []))

    def test_examples_are_valid(self):
        files = sorted((ROOT / "docs" / "user" / "examples").glob("*.yaml"))
        self.assertGreater(len(files), 5)
        res = subprocess.run([sys.executable, str(VALIDATE), *map(str, files), "--format", "json"], capture_output=True, text=True)
        out = json.loads(res.stdout)
        self.assertEqual(res.returncode, 0, [r for r in out if r["problems"]])

    def test_not_yaml(self):
        self.assertEqual(run("a: [b")[1], ["GP-YAML-001"])

    def test_not_mapping(self):
        self.assertEqual(run("- a\n- b\n")[1], ["GP-YAML-001"])

    def test_unknown_key_with_suggestion(self):
        code, rules, out = run(edit(lambda d: d["build"].update(cach={"enabled": True})))
        self.assertEqual((code, rules), (1, ["GP-SCHEMA-001"]))
        self.assertIn("cache", out["problems"][0]["hint"])
        self.assertEqual(out["problems"][0]["pointer"], "/build/cach")

    def test_wrong_value(self):
        self.assertEqual(run(edit(lambda d: d["build"].update(agent="cobol-85")))[1], ["GP-SCHEMA-002"])


class Conventions(unittest.TestCase):
    def test_env_named_like_stage_is_a_warning(self):
        code, rules, _ = run(edit(lambda d: d["deploy"]["environments"].append({"name": "build", "tier": "ground"})))
        self.assertEqual((code, rules), (0, ["GP-ENV-001"]))

    def test_duplicate_env(self):
        self.assertIn("GP-ENV-002", run(edit(lambda d: d["deploy"]["environments"].append({"name": "dev", "tier": "ground"})))[1])

    def test_ground_cluster(self):
        self.assertEqual(run(edit(lambda d: d["deploy"]["environments"][0].update(cluster="x")))[1], ["GP-ENV-003"])

    def test_ground_production(self):
        self.assertEqual(run(edit(lambda d: d["deploy"]["environments"][0].update(production=True)))[1], ["GP-ENV-004"])

    def test_retired_env_keys(self):
        # Also unknown to the schema; the convention rule says what replaced them.
        rules = run(edit(lambda d: d["deploy"].update(lowerEnvironments=["dev"])))[1]
        self.assertIn("GP-ENV-005", rules)

    def test_step_names_unknown_env(self):
        self.assertEqual(run(edit(lambda d: d["pipelines"]["ci"]["steps"][1].update(env="qa")))[1], ["GP-PIPE-001"])

    def test_default_env_is_dev(self):
        doc = edit(lambda d: (d["deploy"].pop("environments"), d["pipelines"]["ci"]["steps"].__setitem__(slice(1, None), [{"stage": "deploy", "env": "dev"}])))
        self.assertEqual(run(doc)[1], [])

    def test_release_to_ground(self):
        self.assertEqual(run(edit(lambda d: d["pipelines"]["ci"]["steps"][4].update(env="int")))[1], ["GP-PIPE-002"])

    def test_two_check_rides(self):
        self.assertEqual(run(edit(lambda d: d["pipelines"]["ci"]["steps"].append({"stage": "test", "env": "dev", "role": "check-ride"})))[1], ["GP-PIPE-003"])

    def test_check_ride_test_name(self):
        self.assertEqual(run(edit(lambda d: d["pipelines"]["ci"]["steps"][3].update(testName="x")))[1], ["GP-PIPE-004"])


class ChecksOnly(unittest.TestCase):
    def test_no_build_is_valid(self):
        self.assertEqual(run(CHECKS_ONLY)[:2], (0, []))

    def test_governance_notifications_secrets_allowed(self):
        doc = dict(CHECKS_ONLY, governance={"githubMerge": {"mode": "merger"}}, notifications={"slack": {"channel": "#x"}},
                   secrets=[{"name": "A", "key": "a"}])
        self.assertEqual(run(doc)[:2], (0, []))

    def test_build_only_keys_refused(self):
        build_flow = {"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": [{"stage": "build"}]}}
        for k, v in (("pipelines", build_flow), ("test", {"enabled": True}), ("deploy", BASE["deploy"]),
                     ("api", {"spec": "openapi.yaml"}), ("ephemeralEnvironments", {"pullRequest": {"enabled": True}})):
            with self.subTest(k):
                code, rules, out = run(dict(CHECKS_ONLY, **{k: v}))
                self.assertEqual((code, rules), (1, ["GP-CHECKS-001"]))
                self.assertEqual(out["problems"][0]["pointer"], f"/{k}")
                self.assertIn("add build", out["problems"][0]["hint"])

    def test_adding_build_is_an_ordinary_change(self):
        head = dict(CHECKS_ONLY, build={"agent": "go-1.24"}, pipelines={"ci": BASE["pipelines"]["ci"] | {"steps": [{"stage": "build"}]}})
        self.assertEqual(run(head, base=CHECKS_ONLY)[:2], (0, []))


class Scope(unittest.TestCase):
    """The P0 exit check: what an agent's pull request may and may not change."""

    def scope(self, fn):
        # Rule ids only, once each: one change can trip a rule on several leaves.
        return sorted(set(run(edit(fn), base=BASE)[1]))

    def test_open_field_passes(self):
        self.assertEqual(self.scope(lambda d: d["build"]["cache"].update(enabled=True, size="large")), [])

    def test_ground_env_added_passes(self):
        self.assertEqual(self.scope(lambda d: d["deploy"]["environments"].insert(2, {"name": "qa", "tier": "ground"})), [])

    def test_env_reorder_is_not_a_change(self):
        self.assertEqual(self.scope(lambda d: d["deploy"]["environments"].reverse()), [])

    def test_step_inserted_before_human_fields_passes(self):
        self.assertEqual(self.scope(lambda d: d["pipelines"]["ci"]["steps"].insert(1, {"stage": "test", "env": "dev", "testName": "smoke"})), [])

    def test_gate_turned_on_passes(self):
        self.assertEqual(self.scope(lambda d: d["governance"].update(sbom=True)), [])

    def test_gate_turned_off(self):
        self.assertEqual(self.scope(lambda d: d["governance"].update(sast=False)), ["GP-SCOPE-002"])

    def test_gate_removed_falls_back_to_default(self):
        self.assertEqual(self.scope(lambda d: d["governance"].pop("imageScan")), ["GP-SCOPE-002"])

    def test_merge_mode_loosened(self):
        self.assertEqual(self.scope(lambda d: d["governance"]["githubMerge"].update(mode="")), ["GP-SCOPE-002"])

    def test_merge_mode_tightened(self):
        self.assertEqual(self.scope(lambda d: d["governance"]["githubMerge"].update(mode="strict", requiredApprovals=2)), [])

    def test_approvals_lowered(self):
        self.assertEqual(self.scope(lambda d: d["governance"]["githubMerge"].update(requiredApprovals=0)), ["GP-SCOPE-002"])

    def test_secrets(self):
        self.assertEqual(self.scope(lambda d: d["secrets"].append({"name": "AWS", "key": "aws"})), ["GP-SCOPE-001"])

    def test_signers(self):
        self.assertEqual(self.scope(lambda d: d["governance"].update(allowedCommitSigners=["agent@x"])), ["GP-SCOPE-001"])

    def test_gitops_repo(self):
        self.assertEqual(self.scope(lambda d: d["pipelines"]["ci"]["steps"][5].update(gitopsRepo="gitops-evil")), ["GP-SCOPE-001"])

    def test_flight_env_added(self):
        self.assertEqual(self.scope(lambda d: d["deploy"]["environments"].append({"name": "staging", "tier": "flight"})), ["GP-SCOPE-003"])

    def test_tier_changed(self):
        self.assertEqual(self.scope(lambda d: d["deploy"]["environments"][1].update(tier="flight")), ["GP-SCOPE-003"])

    def test_check_ride_removed(self):
        self.assertEqual(self.scope(lambda d: d["pipelines"]["ci"]["steps"].pop(3)), ["GP-SCOPE-002"])

    def test_test_made_optional(self):
        self.assertEqual(self.scope(lambda d: d["pipelines"]["ci"]["steps"][2].update(required=False)), ["GP-SCOPE-002"])

    def test_new_file_with_secrets(self):
        self.assertIn("GP-SCOPE-001", cicd_scope_rules(None, BASE))

    def test_base_ref(self):
        with tempfile.TemporaryDirectory() as d:
            git = lambda *a: subprocess.run(["git", "-C", d, *a], check=True, capture_output=True)
            git("init", "-q")
            git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
            f = Path(d) / "cicd.yaml"
            f.write_text(yaml.safe_dump(BASE))
            git("add", "cicd.yaml")
            git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base")
            f.write_text(yaml.safe_dump(edit(lambda x: x["governance"].update(sast=False))))
            res = subprocess.run([sys.executable, str(VALIDATE), str(f), "--agent", "--base-ref", "HEAD", "--format", "json"], capture_output=True, text=True)
            self.assertEqual(res.returncode, 1)
            self.assertEqual([p["rule"] for p in json.loads(res.stdout)["problems"]], ["GP-SCOPE-002"])


def cicd_scope_rules(base, head):
    return [p["rule"] for p in cicd_scope.scope_problems(base, head, SCOPE)]


class Pointers(unittest.TestCase):
    def test_globs(self):
        m = cicd_scope.pointer_matches
        self.assertTrue(m("/deploy/environments/*/cluster", "/deploy/environments/prod/cluster"))
        self.assertTrue(m("/secrets", "/secrets/0/key"))
        self.assertTrue(m("/a/**/z", "/a/b/c/z"))
        self.assertTrue(m("/a/**/z", "/a/z"))
        self.assertFalse(m("/deploy/environments/*/cluster", "/deploy/environments/prod/tier"))
        self.assertFalse(m("/build/cache", "/build/cacheX"))

    def test_escaping(self):
        ch = cicd_scope.changes({"pipelines": {"a/b": 1}}, {"pipelines": {"a/b": 2}})
        self.assertEqual(ch[0][0], "/pipelines/a~1b")


class Contract(unittest.TestCase):
    def test_contract_is_current(self):
        self.assertEqual(gen_contract.build(), (ROOT / "contract" / "glidepath-contract.json").read_text())

    def test_every_schema_leaf_has_a_scope(self):
        fields = CONTRACT["cicd"]["fields"]
        self.assertGreater(len(fields), 50)
        self.assertTrue(all(f["agentScope"] in ("open", "tighten", "human") for f in fields))

    def test_tighten_rules_have_an_order_and_default(self):
        for r in SCOPE["rules"]:
            if r["agentScope"] == "tighten":
                self.assertIn("order", r)
                self.assertIn("default", r, r["pattern"])

    def test_known_failures_compile_and_ids_unique(self):
        import re
        ids = [k["id"] for k in CONTRACT["knownFailures"]]
        self.assertEqual(len(ids), len(set(ids)))
        for k in CONTRACT["knownFailures"]:
            re.compile(k["match"])

    def test_known_failures_match_real_messages(self):
        # One real line per id, copied from the catalog Task that prints it, so a reworded message
        # that no longer matches fails here.
        import re
        samples = {
            "SourceVolumeFull": "write /workspace/source/node_modules/x: no space left on device",
            "CicdSchemaInvalid": "error: cicd.yaml failed schema validation (see above).",
            "CheckRideMissing": "error: no check ride for boarding-api: TestWorkflow testkube/boarding-api-check-ride does not exist.",
            "ItsmChangeNotApproved": "error: change CHG0001 for https://x is in state 'new' with approval 'requested' - not approved or scheduled. Get it approved",
            "RolloutDegraded": "error: rollout boarding-api in app-boarding-api-prod is Degraded on ghcr.io/x:1",
        }
        by_id = {k["id"]: k for k in CONTRACT["knownFailures"]}
        for fid, line in samples.items():
            first = next(k["id"] for k in CONTRACT["knownFailures"] if re.search(k["match"], line))
            self.assertEqual(first, fid, line)
            self.assertIn(fid, by_id)

    def test_api_routes_match_openapi(self):
        # Every route the server registers is in api/openapi.yaml (with a tier), and nothing else is.
        import re
        server = (ROOT / "glidepath" / "api" / "internal" / "server" / "server.go").read_text()
        served = {(m, p) for m, p in re.findall(r'"(GET|POST|PUT|PATCH|DELETE) (/v1/[^"]+)"', server)}
        documented = {(a["method"], a["path"]) for a in CONTRACT["api"]["actions"]}
        self.assertGreater(len(served), 3)
        self.assertEqual(served, documented)

    def test_gates_from_registry(self):
        names = [g["name"] for g in CONTRACT["gates"]]
        self.assertIn("provenance", names)
        self.assertTrue(all(isinstance(g["required"], bool) for g in CONTRACT["gates"]))


class Serve(unittest.TestCase):
    """--serve: the validator as glidepath-api's sidecar (POST /validate)."""

    @classmethod
    def setUpClass(cls):
        import socket
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            port = s.getsockname()[1]
        cls.url = f"http://127.0.0.1:{port}"
        cls.proc = subprocess.Popen([sys.executable, str(VALIDATE), "--serve", f"127.0.0.1:{port}"], stderr=subprocess.DEVNULL)
        for _ in range(200):
            try:
                urllib.request.urlopen(cls.url + "/healthz", timeout=1)
                return
            except (urllib.error.URLError, ConnectionError):
                time.sleep(0.05)
        raise RuntimeError("the validator server did not start")

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        cls.proc.wait()

    def post(self, body):
        req = urllib.request.Request(self.url + "/validate", data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.loads(r.read())

    def test_valid_change_lists_what_it_touches(self):
        head = edit(lambda d: d["build"]["cache"].update(enabled=True))
        out = self.post({"head": yaml.safe_dump(head), "base": yaml.safe_dump(BASE), "agent": True})
        self.assertTrue(out["valid"])
        self.assertEqual(out["problems"], [])
        self.assertEqual(out["changes"], [{"pointer": "/build/cache/enabled", "change": "changed", "before": False, "after": True, "agentScope": "open"}])
        self.assertEqual(out["contractVersion"], CONTRACT["contractVersion"])

    def test_agent_loosening_a_gate(self):
        head = yaml.safe_dump(edit(lambda d: d["governance"].update(sast=False)))
        out = self.post({"head": head, "base": yaml.safe_dump(BASE), "agent": True})
        self.assertEqual(([p["rule"] for p in out["problems"]], out["valid"]), (["GP-SCOPE-002"], False))
        self.assertEqual(out["changes"][0]["agentScope"], "tighten")
        # The same change by a person: no scope rule, the change still listed.
        out = self.post({"head": head, "base": yaml.safe_dump(BASE), "agent": False})
        self.assertEqual((out["problems"], len(out["changes"])), ([], 1))

    def test_no_file_yet(self):
        out = self.post({"head": yaml.safe_dump(BASE), "base": None, "agent": True})
        self.assertIn("GP-SCOPE-001", [p["rule"] for p in out["problems"]])  # secrets: a human field, added
        self.assertTrue(out["changes"] and all(c["change"] == "added" for c in out["changes"]))
        # No base key at all: nothing to compare with, no changes reported.
        self.assertEqual(self.post({"head": yaml.safe_dump(BASE)})["changes"], [])

    def test_schema_error(self):
        out = self.post({"head": yaml.safe_dump(edit(lambda d: d["build"].update(cach={"enabled": True})))})
        self.assertEqual(([p["rule"] for p in out["problems"]], out["valid"]), (["GP-SCHEMA-001"], False))

    def test_not_yaml_is_a_finding_not_an_error(self):
        out = self.post({"head": "a: [b", "base": yaml.safe_dump(BASE)})
        self.assertEqual(([p["rule"] for p in out["problems"]], out["changes"]), (["GP-YAML-001"], []))

    def test_bad_body(self):
        req = urllib.request.Request(self.url + "/validate", data=b"not json", headers={"Content-Type": "application/json"}, method="POST")
        with self.assertRaises(urllib.error.HTTPError) as cm:
            urllib.request.urlopen(req, timeout=10)
        self.assertEqual(cm.exception.code, 400)


if __name__ == "__main__":
    unittest.main(verbosity=1)
