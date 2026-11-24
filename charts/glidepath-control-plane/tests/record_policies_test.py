#!/usr/bin/env python3
"""Tests for the release-record-writes and gate-report-writes admission policies (pipeline trust T4, step 5), and gate-ledger-writes
(W-8: glidepath-api's own gate ledger patch).

Renders the chart, then evaluates the policies' own CEL (matchConditions, variables, validations) over fixtures with the same real CEL
implementation (cel-python) cicd_run_admission_test.py uses, for each of the three task accounts, so a typo or a has()-less field access
is caught here and not by a refused production request. hack/ci/vap-compile compiles the same expressions under the API server's checker.

Run:  python3 charts/glidepath-control-plane/tests/record_policies_test.py     (needs helm, pyyaml, cel-python)
"""
import copy
import os
import subprocess
import unittest

import celpy
import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CHART = os.path.join(ROOT, "charts", "glidepath-control-plane")
VALUES = os.path.join(CHART, "tests", "values.example.yaml")
NS = "app-sample-cicd"
ENV = celpy.Environment()


def user(sa, ns=NS):
    return "system:serviceaccount:%s:%s" % (ns, sa)


RUNNER, GATE, WRITER = user("pipeline-runner"), user("clearance-gate"), user("release-record-writer")
RECORD = "release-tracking-abc123"
LABEL = "hangar.io/subcomponent"


def render(*extra):
    r = subprocess.run(["helm", "template", "cp", CHART, "-f", VALUES, *extra], capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    return [d for d in yaml.safe_load_all(r.stdout) if d]


def cel(expr, act):
    return ENV.program(ENV.compile(" ".join(expr.split())), functions=None).evaluate(act)


def evaluate(policy, obj, old=None, username=WRITER, namespace=NS, operation=None):
    """-> None when the policy does not apply, else the list of failing validation messages."""
    op = operation or ("CREATE" if old is None else "UPDATE")
    request = {"userInfo": {"username": username}, "namespace": namespace, "operation": op}
    act = {"object": celpy.json_to_cel(obj), "oldObject": celpy.json_to_cel(old), "request": celpy.json_to_cel(request),
           "variables": celpy.json_to_cel({})}
    for mc in policy["matchConditions"]:
        if not cel(mc["expression"], act):
            return None
    variables = {}
    for v in policy.get("variables", []):
        variables[v["name"]] = cel(v["expression"], act)
        act["variables"] = celpy.celtypes.MapType({celpy.celtypes.StringType(k): x for k, x in variables.items()})
    return [val["message"] for val in policy["validations"] if not cel(val["expression"], act)]


def cm(name, labels=None, data=None, owners=None):
    md = {"name": name}
    if labels is not None:
        md["labels"] = labels
    if owners is not None:
        md["ownerReferences"] = owners
    out = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": md}
    if data is not None:
        out["data"] = data
    return out


def record(**data):
    base = {"prUrl": "https://github.com/o/gitops/pull/7", "releaseId": "c1:dev/prod", "image": "reg/o/app:1", "imageDigest": "sha256:" + "a" * 64,
            "gitRevision": "b" * 40, "kind": "promote", "state": "proposed"}
    base.update(data)
    return cm(RECORD, {LABEL: "release-tracking", "hangar.io/app": "sample"}, base)


def owner(run, kind="PipelineRun", api="tekton.dev/v1"):
    return [{"apiVersion": api, "kind": kind, "name": run, "uid": "u-1"}]


class RecordPolicies(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        docs = render()
        cls.pols = {d["metadata"]["name"]: d["spec"] for d in docs if d["kind"] == "ValidatingAdmissionPolicy"}
        cls.binds = {d["metadata"]["name"]: d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding"}
        cls.rec = cls.pols["release-record-writes"]
        cls.rep = cls.pols["gate-report-writes"]

    def ok(self, pol, obj, **kw):
        self.assertEqual(evaluate(pol, obj, **kw), [], kw)

    def refused(self, pol, obj, n=1, **kw):
        got = evaluate(pol, obj, **kw)
        self.assertIsInstance(got, list, "the policy did not apply: %s" % kw)
        self.assertEqual(len(got), n, (got, kw))

    # ---- as rendered ----
    def test_audit_by_default_failclosed_with_guards_and_scoped(self):
        for n in ("release-record-writes", "gate-report-writes"):
            self.assertEqual(self.binds[n]["spec"]["validationActions"], ["Audit"], n)
            self.assertIn("NOT ENFORCING", self.binds[n]["metadata"]["annotations"]["glidepath.io/enforcement"], n)
            p = self.pols[n]
            self.assertEqual(p["failurePolicy"], "Fail")
            for rule in p["matchConstraints"]["resourceRules"]:
                self.assertEqual((rule["apiGroups"], rule["resources"]), ([""], ["configmaps"]))
            self.assertTrue(p["matchConditions"])
        self.assertEqual(self.rec["matchConstraints"]["resourceRules"][0]["operations"], ["CREATE", "UPDATE", "DELETE"])

    def test_the_switch_takes_deny_and_clears_the_annotation(self):
        docs = render("--set", "cicdRunAdmission.recordWritesActions={Deny}", "--set", "cicdRunAdmission.gateReportActions={Warn,Audit}")
        b = {d["metadata"]["name"]: d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding"}
        self.assertEqual(b["release-record-writes"]["spec"]["validationActions"], ["Deny"])
        self.assertNotIn("annotations", b["release-record-writes"]["metadata"])
        self.assertEqual(b["gate-report-writes"]["spec"]["validationActions"], ["Warn", "Audit"])
        self.assertIn("NOT ENFORCING", b["gate-report-writes"]["metadata"]["annotations"]["glidepath.io/enforcement"])

    def test_off_renders_neither(self):
        names = {d["metadata"]["name"] for d in render("--set", "cicdRunAdmission.enabled=false")}
        self.assertFalse(names & {"release-record-writes", "gate-report-writes"})

    # ---- who the policies concern ----
    def test_other_accounts_and_namespaces_are_not_matched(self):
        r = record()
        for pol in (self.rec, self.rep):
            for u in ("system:serviceaccount:platform-system:glidepath-relay", "system:serviceaccount:platform-system:release-record-sweeper",
                      user("source-runner"), user("pipeline-trigger"), user("pipeline-runner", "app-sample-prod"), "kubernetes-admin",
                      "system:serviceaccount:app-sample-cicd-extra:pipeline-runner", "system:serviceaccount:app-sample-cicd:pipeline-runner:x", ""):
                self.assertIsNone(evaluate(pol, r, username=u), u)
            for ns in ("app-sample-prod", "kube-system", "platform-catalog", ""):
                self.assertIsNone(evaluate(pol, r, username=RUNNER, namespace=ns), ns)
        # a SA of one cicd namespace writing in another cicd namespace is still checked (RBAC does not allow it; the policy does not rely on that)
        self.assertEqual(len(evaluate(self.rec, r, username=RUNNER, namespace="app-other-cicd")), 1)
        self.assertEqual(len(evaluate(self.rec, r, username=user("pipeline-runner", "infra-x-cicd"), namespace="infra-x-cicd")), 1)

    # ---- release-record-writes: pipeline-runner and clearance-gate ----
    def test_runner_and_gate_may_not_touch_a_record(self):
        r = record()
        for u in (RUNNER, GATE):
            self.refused(self.rec, cm(RECORD, None, {}), username=u)                                     # create by name
            self.refused(self.rec, cm("x", {LABEL: "release-tracking"}, {}), username=u)                # create by label
            self.refused(self.rec, r, old=r, username=u)                                                 # update
            self.refused(self.rec, None, old=r, username=u, operation="DELETE")                          # delete
            self.refused(self.rec, cm("legacy-name", {}, {}), old=cm("legacy-name", {LABEL: "release-tracking"}, {}), username=u)  # removing the label
            # adding the label to an ordinary ConfigMap
            self.refused(self.rec, cm("app-config", {LABEL: "release-tracking"}, {}), old=cm("app-config", {}, {}), username=u)

    def test_runner_and_gate_may_write_other_configmaps_here(self):
        for u in (RUNNER, GATE):
            self.ok(self.rec, cm("gate-report-r1", {"glidepath.io/gate-report": "clearance-gate"}, {}), username=u)
            self.ok(self.rec, cm("app-config", {LABEL: "config"}, {}), username=u)
            self.ok(self.rec, cm("app-config", None, {}), old=cm("app-config", None, {}), username=u)
            self.ok(self.rec, None, old=cm("scratch", {}, {}), username=u, operation="DELETE")

    # ---- release-record-writer ----
    def test_writer_may_write_a_record(self):
        self.ok(self.rec, cm(RECORD, None, {"prUrl": "u", "kind": "promote"}))                                   # create (apply, label comes after)
        r = record()
        self.ok(self.rec, record(state="merged"), old=r)                                                         # a re-run/merge patch
        self.ok(self.rec, record(changeRequestId="CHG1"), old=r)
        self.ok(self.rec, None, old=r, operation="DELETE")
        legacy = cm("legacy-name", {LABEL: "release-tracking"}, {})
        self.ok(self.rec, legacy, old=legacy)                                                                    # an existing labelled record

    def test_writer_may_not_touch_anything_else(self):
        self.refused(self.rec, cm("gate-report-r1", None, {}))
        self.refused(self.rec, cm("gate-report-r1", None, {}), old=cm("gate-report-r1", None, {}))
        self.refused(self.rec, cm("gate-report-r1", {LABEL: "release-tracking"}, {}), old=cm("gate-report-r1", {LABEL: "release-tracking"}, {}))
        self.refused(self.rec, cm("app-config", None, {}))
        self.refused(self.rec, cm("app-config", None, {}), old=cm("app-config", None, {}))
        self.refused(self.rec, None, old=cm("app-config", None, {}), operation="DELETE")
        # the label must not let it adopt a ConfigMap, nor create one under another name
        self.refused(self.rec, cm("app-config", {LABEL: "release-tracking"}, {}), old=cm("app-config", {}, {}))
        self.refused(self.rec, cm("other", {LABEL: "release-tracking"}, {}))

    def test_writer_may_not_change_an_immutable_key(self):
        old = record()
        for key, new in (("prUrl", "https://github.com/o/gitops/pull/8"), ("releaseId", "c2:dev/prod"), ("image", "reg/o/app:2"),
                         ("imageDigest", "sha256:" + "c" * 64), ("gitRevision", "d" * 40), ("kind", "rollback")):
            self.refused(self.rec, record(**{key: new}), old=old)
            gone = record()
            del gone["data"][key]
            self.refused(self.rec, gone, old=old)                                                                # removing it is a change too
        self.ok(self.rec, record(state="deployed", stateAt="2026-11-20T00:00:00Z"), old=old)

    def test_writer_may_fill_an_immutable_key_in_from_empty(self):
        for empty in ("", None):
            old = record(prUrl="") if empty == "" else record()
            if empty is None:
                del old["data"]["prUrl"]
            self.ok(self.rec, record(prUrl="https://github.com/o/gitops/pull/7"), old=old)

    # ---- gate-report-writes ----
    def test_a_gate_report_with_its_run_as_owner_is_admitted(self):
        for u in (GATE, RUNNER):
            self.ok(self.rep, cm("gate-report-run-1", {}, {"report": "x"}, owner("run-1")), username=u)
            self.ok(self.rep, cm("gate-report-run-1", {}, {"report": "y"}, owner("run-1")), old=cm("gate-report-run-1", {}, {}, owner("run-1")), username=u)
            self.ok(self.rep, cm("gate-report-run-1", {}, {}, owner("run-1", api="tekton.dev/v1beta1")), username=u)
            self.ok(self.rep, cm("gate-report-run-1", {}, {}, owner("other") + owner("run-1")), username=u)

    def test_a_gate_report_without_a_matching_owner_is_refused(self):
        for u in (GATE, RUNNER):
            self.refused(self.rep, cm("gate-report-run-1", {}, {}), username=u)                                   # no ownerReferences
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, []), username=u)
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, owner("run-2")), username=u)                   # another run
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, owner("run-1", kind="TaskRun")), username=u)   # not a PipelineRun
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, owner("run-1", api="v1")), username=u)
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, [{"kind": "PipelineRun", "apiVersion": "tekton.dev/v1"}]), username=u)
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, [{"kind": "PipelineRun", "apiVersion": "tekton.dev/v1", "name": ""}]), username=u)
            self.refused(self.rep, cm("gate-report-run-1", {}, {}, [{}]), username=u)

    def test_a_gate_report_update_that_drops_the_owner_is_refused(self):
        self.refused(self.rep, cm("gate-report-run-1", {}, {"a": "b"}), old=cm("gate-report-run-1", {}, {}, owner("run-1")), username=GATE)

    def test_anything_but_a_gate_report_is_refused_for_runner_and_gate(self):
        for u in (GATE, RUNNER):
            self.refused(self.rep, cm("app-config", {}, {}, owner("app-config")), username=u)
            self.refused(self.rep, cm("report-run-1", {}, {}, owner("run-1")), username=u)
            self.refused(self.rep, cm("gate-report-", {}, {}, owner("")), username=u, n=2)
            self.refused(self.rep, cm(RECORD, {}, {}, owner("abc123")), username=u)

    def test_the_record_writer_is_not_this_policys_concern(self):
        self.assertIsNone(evaluate(self.rep, cm(RECORD, {}, {}), username=WRITER))

    # ---- fail-closed: no shape raises ----
    def test_sparse_objects_do_not_raise(self):
        # An error under failurePolicy Fail refuses the request cluster-wide: every shape must evaluate, not raise.
        shapes = [{}, {"metadata": {}}, {"metadata": {"name": 3}}, {"metadata": {"name": "release-tracking-x", "labels": {}}},
                  {"metadata": {"name": "gate-report-x", "ownerReferences": []}}, {"metadata": {"name": "gate-report-x", "ownerReferences": [{}]}},
                  {"metadata": {"labels": {LABEL: "x"}}}, {"metadata": {"name": "x"}, "data": {}}, {"metadata": {"name": "x"}, "data": {"prUrl": "a"}},
                  {"data": {"prUrl": "a"}}]
        for u in (RUNNER, GATE, WRITER):
            for new in shapes:
                for pol in (self.rec, self.rep):
                    if pol is self.rep and u == WRITER:
                        continue  # not matched: covered by test_the_record_writer_is_not_this_policys_concern
                    self.assertIsInstance(evaluate(pol, copy.deepcopy(new), username=u, operation="CREATE"), list)
                    for old in shapes:
                        self.assertIsInstance(evaluate(pol, copy.deepcopy(new), old=copy.deepcopy(old), username=u, operation="UPDATE"), list)
            for old in shapes:
                self.assertIsInstance(evaluate(self.rec, None, old=copy.deepcopy(old), username=u, operation="DELETE"), list)
        # a request with no userInfo, or no namespace, is not matched and does not raise
        for request in ({}, {"userInfo": {}}, {"userInfo": {"username": RUNNER}}):
            act = {"object": celpy.json_to_cel(record()), "oldObject": celpy.json_to_cel(None), "request": celpy.json_to_cel(request)}
            for pol in (self.rec, self.rep):
                for mc in pol["matchConditions"]:
                    self.assertFalse(cel(mc["expression"], act))


API = "system:serviceaccount:platform-system:glidepath-api"
SHA12 = "0123456789ab"


class GateLedgerWrites(unittest.TestCase):
    """gate-ledger-writes: glidepath-api may only patch the gate ledger keys of an existing release record."""

    @classmethod
    def setUpClass(cls):
        docs = render()
        cls.pol = {d["metadata"]["name"]: d["spec"] for d in docs if d["kind"] == "ValidatingAdmissionPolicy"}["gate-ledger-writes"]
        cls.bind = {d["metadata"]["name"]: d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding"}["gate-ledger-writes"]

    def ev(self, new, old=None, **kw):
        kw.setdefault("username", API)
        return evaluate(self.pol, new, old=old, **kw)

    def test_deny_from_the_start_and_fail_closed(self):
        self.assertEqual(self.bind["spec"]["validationActions"], ["Deny"])
        self.assertNotIn("annotations", self.bind["metadata"])
        self.assertEqual(self.pol["failurePolicy"], "Fail")
        self.assertEqual(self.pol["matchConstraints"]["resourceRules"][0]["operations"], ["CREATE", "UPDATE", "DELETE"])
        b = {d["metadata"]["name"]: d for d in render("--set", "cicdRunAdmission.gateLedgerActions={Audit}")
             if d["kind"] == "ValidatingAdmissionPolicyBinding"}["gate-ledger-writes"]
        self.assertIn("NOT ENFORCING", b["metadata"]["annotations"]["glidepath.io/enforcement"])

    def test_a_ledger_patch_is_admitted(self):
        old = record(**{"gate.%s.sbom" % SHA12: "success"})
        self.assertEqual(self.ev(record(**{"gate.%s.sbom" % SHA12: "success", "gate.%s.sast" % SHA12: "failure",
                                           "gateRun.%s.sast" % SHA12: "2026-11-20T11:50:00Z sast-abcde"}), old), [])
        # overwriting a verdict (a later run of the gate), and the infra namespaces
        self.assertEqual(self.ev(record(**{"gate.%s.sbom" % SHA12: "failure"}), old), [])
        # an object that already carries annotations, owners and immutable: false, left as they were
        keep = record(); keep["immutable"] = False; keep["metadata"]["annotations"] = {"a": "b"}; keep["metadata"]["ownerReferences"] = [{"kind": "X", "name": "y"}]
        after = copy.deepcopy(keep); after["data"]["gate.%s.sast" % SHA12] = "success"
        self.assertEqual(self.ev(after, keep), [])
        self.assertEqual(self.ev(record(**{"gate.%s.sast" % SHA12: "success"}), record(), namespace="infra-queue-cicd"), [])

    def test_anything_else_is_refused(self):
        old = record()
        cases = [
            (record(**{"gate.%s.sast" % SHA12: "success"}), None, "CREATE"),                       # create
            (None, old, "DELETE"),                                                                 # delete
            (record(prUrl="https://github.com/o/gitops/pull/8"), old, "UPDATE"),                   # another key
            (record(state="healthy"), old, "UPDATE"),
            (record(extra="x"), old, "UPDATE"),                                                    # a new non-ledger key
            (record(**{"gate.%s.sast" % SHA12: "maybe"}), old, "UPDATE"),                          # not a verdict
            (record(**{"gate.short.sast": "success"}), old, "UPDATE"),                             # not a ledger key
            (record(**{"gate.%s.Bad_Name" % SHA12: "success"}), old, "UPDATE"),
        ]
        gone = record()
        del gone["data"]["image"]
        cases.append((gone, old, "UPDATE"))                                                        # a removed key
        relabel = record(**{"gate.%s.sast" % SHA12: "success"})
        relabel["metadata"]["labels"]["hangar.io/app"] = "other"
        cases.append((relabel, old, "UPDATE"))                                                     # a label change
        other = cm("app-config", {}, {"gate.%s.sast" % SHA12: "success"})
        cases.append((other, cm("app-config", {}, {}), "UPDATE"))                                  # not a record
        frozen = record(**{"gate.%s.sast" % SHA12: "success"})
        frozen["immutable"] = True
        cases.append((frozen, old, "UPDATE"))                                                      # immutable: true freezes it
        for field, val in (("binaryData", {"x": "eA=="}),):
            o2 = record(**{"gate.%s.sast" % SHA12: "success"}); o2[field] = val
            cases.append((o2, old, "UPDATE"))
        for field, val in (("annotations", {"a": "b"}), ("ownerReferences", [{"kind": "X", "name": "y"}]), ("finalizers", ["f"])):
            o2 = record(**{"gate.%s.sast" % SHA12: "success"}); o2["metadata"][field] = val
            cases.append((o2, old, "UPDATE"))
        named = cm(RECORD, {}, {"gate.%s.sast" % SHA12: "success"})
        cases.append((named, cm(RECORD, {}, {}), "UPDATE"))                                        # named like one, not labelled
        for new, o, op in cases:
            got = self.ev(new, o, operation=op)
            self.assertIsInstance(got, list, (new, op))
            self.assertTrue(got, (new, op))

    def test_only_glidepath_api_in_a_cicd_namespace(self):
        r = record(state="healthy")
        for u in (RUNNER, WRITER, "system:serviceaccount:platform-system:glidepath-relay", "system:serviceaccount:other:glidepath-api", "kubernetes-admin"):
            self.assertIsNone(self.ev(r, record(), username=u), u)
        for ns in ("app-sample-prod", "platform-system", "kube-system", ""):
            self.assertIsNone(self.ev(r, record(), namespace=ns), ns)

    def test_sparse_objects_do_not_raise(self):
        shapes = [{}, {"metadata": {}}, {"metadata": {"name": 3}}, {"metadata": {"name": RECORD, "labels": {}}}, {"data": {}},
                  {"metadata": {"name": RECORD, "labels": {LABEL: "release-tracking"}}}, {"metadata": {"name": "x"}, "data": {"a": "b"}},
                  {"immutable": True, "binaryData": {}, "metadata": {"annotations": {}, "ownerReferences": [], "finalizers": []}}]
        for new in shapes:
            self.assertIsInstance(self.ev(copy.deepcopy(new), operation="CREATE"), list)
            for old in shapes:
                self.assertIsInstance(self.ev(copy.deepcopy(new), old=copy.deepcopy(old), operation="UPDATE"), list)
            self.assertIsInstance(self.ev(None, old=copy.deepcopy(new), operation="DELETE"), list)

    def test_a_change_kind_is_added_once_to_a_record(self):
        old = record()
        ck = "changeKind.%s" % SHA12
        self.assertEqual(self.ev(record(**{ck: '{"kind":"config"}'}), old), [])
        self.assertEqual(self.ev(record(**{ck: '{"kind":"config"}', "gate.%s.sast" % SHA12: "success"}), record(**{ck: '{"kind":"config"}'})), [])
        for new in (record(**{ck: '{"kind":"no-op"}'}), record()):                                        # changed, removed
            self.assertTrue(self.ev(new, record(**{ck: '{"kind":"config"}'})), new)
        for bad in ("changeKind.short", "changeKind.%s.x" % SHA12, "changekind.%s" % SHA12):
            self.assertTrue(self.ev(record(**{bad: "x"}), old), bad)

    def test_the_change_kind_store_is_not_this_policys(self):
        store = cm("change-kind-pr-7", {LABEL: "change-kind"}, {SHA12: "{}"})
        self.assertIsNone(self.ev(store, operation="CREATE"))
        self.assertIsNone(self.ev(store, old=cm("change-kind-pr-7", {LABEL: "change-kind"}, {}), operation="UPDATE"))
        self.assertIsNone(self.ev(None, old=store, operation="DELETE"))


class ChangeKindWrites(unittest.TestCase):
    """change-kind-writes: only glidepath-api writes change-kind-pr-<n>, only by adding <sha12> keys (CG-1)."""

    @classmethod
    def setUpClass(cls):
        docs = render()
        cls.pol = {d["metadata"]["name"]: d["spec"] for d in docs if d["kind"] == "ValidatingAdmissionPolicy"}["change-kind-writes"]
        cls.bind = {d["metadata"]["name"]: d for d in docs if d["kind"] == "ValidatingAdmissionPolicyBinding"}["change-kind-writes"]

    def ev(self, new, old=None, **kw):
        kw.setdefault("username", API)
        return evaluate(self.pol, new, old=old, **kw)

    @staticmethod
    def store(data=None, name="change-kind-pr-7", labels=None):
        return cm(name, {LABEL: "change-kind", "hangar.io/app": "sample", "hangar.io/pull-request": "7"} if labels is None else labels,
                  {SHA12: '{"kind":"config"}'} if data is None else data)

    def test_deny_from_the_start_and_fail_closed_never_on_delete(self):
        self.assertEqual(self.bind["spec"]["validationActions"], ["Deny"])
        self.assertNotIn("annotations", self.bind["metadata"])
        self.assertEqual(self.pol["failurePolicy"], "Fail")
        self.assertEqual(self.pol["matchConstraints"]["resourceRules"][0]["operations"], ["CREATE", "UPDATE"])
        b = {d["metadata"]["name"]: d for d in render("--set", "cicdRunAdmission.changeKindActions={Audit}")
             if d["kind"] == "ValidatingAdmissionPolicyBinding"}["change-kind-writes"]
        self.assertIn("NOT ENFORCING", b["metadata"]["annotations"]["glidepath.io/enforcement"])

    def test_glidepath_api_creates_and_adds_keys(self):
        self.assertEqual(self.ev(self.store()), [])
        other = "b" * 12
        self.assertEqual(self.ev(self.store({SHA12: '{"kind":"config"}', other: '{"kind":"no-op"}'}), self.store()), [])
        self.assertEqual(self.ev(self.store(), namespace="infra-queue-cicd"), [])

    def test_anything_else_is_refused(self):
        old = self.store()
        cases = [
            (self.store(), None, {"username": RUNNER}),                                              # another writer
            (self.store(), old, {"username": WRITER}),
            (self.store(), None, {"username": "kubernetes-admin"}),
            (self.store({SHA12: '{"kind":"no-op"}'}), old, {}),                                      # a stored kind changed
            (self.store({}), old, {}),                                                               # removed
            (self.store({"x": "y"}), None, {}),                                                      # not a head key
            (self.store(name="change-kind-pr-x"), None, {}),                                         # not a number
            (self.store(name="change-kind-pr-07"), None, {}),
            (self.store(labels={}), None, {}),                                                       # not labelled
            (self.store(labels={LABEL: "change-kind", "hangar.io/app": "other"}), old, {}),          # a label changed
        ]
        frozen = self.store(); frozen["immutable"] = True
        cases.append((frozen, None, {}))
        owned = self.store(); owned["metadata"]["ownerReferences"] = [{"kind": "X", "name": "y"}]
        cases.append((owned, None, {}))
        annotated = self.store({SHA12: '{"kind":"config"}', "b" * 12: "{}"}); annotated["metadata"]["annotations"] = {"a": "b"}
        cases.append((annotated, old, {}))
        for new, o, kw in cases:
            got = self.ev(new, o, **kw)
            self.assertIsInstance(got, list, (new, kw))
            self.assertTrue(got, (new, kw))

    def test_only_change_kind_stores_in_a_cicd_namespace(self):
        self.assertIsNone(self.ev(record(), username=RUNNER))
        self.assertIsNone(self.ev(cm("app-config", {}, {"a": "b"}), username=RUNNER))
        for ns in ("app-sample-prod", "platform-system", ""):
            self.assertIsNone(self.ev(self.store(), username=RUNNER, namespace=ns), ns)

    def test_sparse_objects_do_not_raise(self):
        shapes = [{}, {"metadata": {}}, {"metadata": {"name": 3}}, {"metadata": {"name": "change-kind-pr-7"}},
                  {"metadata": {"name": "change-kind-pr-7", "labels": {}}}, {"metadata": {"name": "change-kind-pr-7"}, "data": {}},
                  {"metadata": {"name": "change-kind-pr-7", "annotations": {}, "ownerReferences": [], "finalizers": []}, "immutable": False, "binaryData": {}}]
        for new in shapes:
            self.assertIsInstance(self.ev(copy.deepcopy(new), operation="CREATE"), (list, type(None)))
            for old in shapes:
                self.assertIsInstance(self.ev(copy.deepcopy(new), old=copy.deepcopy(old), operation="UPDATE"), (list, type(None)))


if __name__ == "__main__":
    unittest.main()
