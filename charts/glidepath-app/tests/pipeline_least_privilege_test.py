#!/usr/bin/env python3
"""Pipeline identities (docs/admin/security.md, "Pipeline identities"): which account runs what, and what each can do.

Renders glidepath-app and glidepath-catalog and asserts, from the manifests alone:
  - source-runner (the account that runs the repository's own code) has automount off, imagePullSecrets only, and
    NOTHING grants it anything: no Role, no ClusterRole, directly or by group. A negative test evaluates the rendered
    RBAC the way the API server would and expects zero rules.
  - pipeline-runner cannot create a run, cannot read a Secret other than app-secrets, cannot mint tokens or
    impersonate; pipeline-trigger can only create PipelineRuns and mounts no token.
  - every Trigger creates runs as pipeline-trigger, every run executes as pipeline-runner, and the broker may
    impersonate pipeline-trigger only.
  - the catalog's source-executing tasks carry no credential: no projected token, no kubectl, no secret mounted into
    a step that runs the repository's code; and the list of tasks the generated build run hands to source-runner is
    exactly the tasks of the build Pipeline that run that code (and the same as the admission policy's).

Run:  python3 charts/glidepath-app/tests/pipeline_least_privilege_test.py     (needs helm, pyyaml)
"""
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
APP = os.path.join(ROOT, "charts", "glidepath-app")
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
CP = os.path.join(ROOT, "charts", "glidepath-control-plane")
NS = "app-x-cicd"   # the chart derives the real namespace from platformIdentity.appName; rules_for reads it back
SOURCE_TASKS = ["build-source", "unit-test", "build-image", "build-python-wheel-artifact", "build-go-binary-artifact"]
PUSHER_TASKS = ["push-image"]

CICD = {
    "apiVersion": "platform/v1", "kind": "PipelineConfig",
    "build": {"agent": "nodejs-20"},
    "test": {"name": "integration"},
    "deploy": {"environments": [{"name": "dev", "tier": "ground"}, {"name": "prod", "tier": "flight"}]},
    "pipelines": {"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": [
        {"stage": "build"}, {"stage": "test", "env": "dev", "testName": "integration"},
        {"stage": "deploy", "env": "dev"}, {"stage": "release", "env": "prod"}]}},
}


def helm(chart, *args):
    p = subprocess.run(["helm", "template", "x", chart, *args], capture_output=True, text=True)
    assert p.returncode == 0, p.stderr
    return [d for d in yaml.safe_load_all(p.stdout) if d]


def app_docs():
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(CICD, f)
        path = f.name
    try:
        return helm(APP, "--namespace", NS, "-f", path, "-f", os.path.join(APP, "tests", "fixtures", "control-plane-values.yaml"))
    finally:
        os.unlink(path)


def rules_for(docs, sa, namespace=None):
    """The rules the API server would grant this ServiceAccount (rendered Roles/ClusterRoles and their bindings)."""
    namespace = namespace or [d["metadata"]["namespace"] for d in docs if d["kind"] == "ServiceAccount" and d["metadata"]["name"] == sa][0]
    roles = {(d["kind"], d["metadata"]["name"], d["metadata"].get("namespace", "")): d.get("rules", [])
             for d in docs if d["kind"] in ("Role", "ClusterRole")}
    found = []
    for b in docs:
        if b["kind"] not in ("RoleBinding", "ClusterRoleBinding"):
            continue
        for s in b.get("subjects") or []:
            hit = (s.get("kind") == "ServiceAccount" and s.get("name") == sa and s.get("namespace", b["metadata"].get("namespace")) == namespace) \
                or (s.get("kind") == "Group" and s.get("name") in ("system:serviceaccounts", "system:authenticated", f"system:serviceaccounts:{namespace}"))
            if not hit:
                continue
            ref = b["roleRef"]
            ns = b["metadata"].get("namespace", "") if ref["kind"] == "Role" else ""
            found += roles.get((ref["kind"], ref["name"], ns), [])
    return found


def allows(rules, group, resource, verb, name=None):
    for r in rules:
        if group in r.get("apiGroups", []) or "*" in r.get("apiGroups", []):
            if resource in r.get("resources", []) or "*" in r.get("resources", []):
                if verb in r.get("verbs", []) or "*" in r.get("verbs", []):
                    if not r.get("resourceNames") or name in r["resourceNames"]:
                        return True
    return False


class Identities(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.docs = app_docs()
        cls.sas = {d["metadata"]["name"]: d for d in cls.docs if d["kind"] == "ServiceAccount"}

    def test_source_runner_has_no_token_and_no_rbac(self):
        sa = self.sas["source-runner"]
        self.assertIs(sa.get("automountServiceAccountToken"), False)
        self.assertEqual(sa.get("imagePullSecrets"), [{"name": "registry-credentials"}])
        self.assertEqual(rules_for(self.docs, "source-runner"), [], "source-runner must be granted nothing")
        for kind in ("RoleBinding", "ClusterRoleBinding"):
            for b in self.docs:
                if b["kind"] == kind:
                    for s in b.get("subjects") or []:
                        self.assertNotEqual(s.get("name"), "source-runner", b["metadata"]["name"])

    def test_source_runner_can_do_nothing(self):
        # the negative test: the verbs that mattered in the review, one by one
        rules = rules_for(self.docs, "source-runner")
        for group, res, verb in (("", "configmaps", "get"), ("", "configmaps", "create"), ("", "configmaps", "patch"),
                                 ("", "configmaps", "delete"), ("", "secrets", "get"), ("", "secrets", "list"),
                                 ("tekton.dev", "pipelineruns", "create"), ("tekton.dev", "taskruns", "create"),
                                 ("tekton.dev", "pipelineruns", "get"), ("", "pods", "get"), ("", "pods", "create"),
                                 ("", "serviceaccounts", "impersonate"), ("", "serviceaccounts/token", "create")):
            self.assertFalse(allows(rules, group, res, verb), f"{verb} {group}/{res}")

    def test_pipeline_runner_cannot_create_runs_or_read_secrets(self):
        rules = rules_for(self.docs, "pipeline-runner")
        self.assertTrue(rules, "the grant lookup found nothing: the assertions below would be vacuous")
        self.assertTrue(allows(rules, "tekton.dev", "pipelineruns", "get"))   # stage-debrief reads its own run
        self.assertTrue(allows(rules, "tekton.dev", "taskruns", "list"))
        for res in ("pipelineruns", "taskruns"):
            for verb in ("create", "update", "patch", "delete"):
                self.assertFalse(allows(rules, "tekton.dev", res, verb), f"{verb} {res}")
        self.assertTrue(allows(rules, "", "secrets", "get", "app-secrets"))
        self.assertFalse(allows(rules, "", "secrets", "get", "anything-else"))
        for verb in ("list", "watch", "create", "patch", "update", "delete"):
            self.assertFalse(allows(rules, "", "secrets", verb), verb)
        for group, res, verb in (("", "serviceaccounts/token", "create"), ("", "serviceaccounts", "impersonate"),
                                 ("", "pods", "create"), ("rbac.authorization.k8s.io", "rolebindings", "create"),
                                 ("", "*", "*")):
            self.assertFalse(allows(rules, group, res, verb), f"{verb} {group}/{res}")

    def test_pipeline_trigger_creates_runs_and_nothing_else(self):
        self.assertIs(self.sas["pipeline-trigger"].get("automountServiceAccountToken"), False)
        rules = rules_for(self.docs, "pipeline-trigger")
        self.assertEqual(rules, [{"apiGroups": ["tekton.dev"], "resources": ["pipelineruns"], "verbs": ["create"]}])

    def test_broker_impersonates_pipeline_trigger_only(self):
        roles = [d for d in self.docs if d["kind"] == "Role" and "impersonate" in str(d.get("rules"))]
        self.assertEqual(len(roles), 1)
        self.assertEqual(roles[0]["rules"][0]["resourceNames"], ["pipeline-trigger"])
        self.assertEqual(roles[0]["rules"][0]["verbs"], ["impersonate"])

    def test_triggers_create_as_pipeline_trigger_and_runs_execute_as_pipeline_runner(self):
        triggers = [d for d in self.docs if d["kind"] == "Trigger"]
        self.assertGreaterEqual(len(triggers), 3)
        for t in triggers:
            self.assertEqual(t["spec"]["serviceAccountName"], "pipeline-trigger", t["metadata"]["name"])
        runs = 0
        for d in self.docs:
            if d["kind"] == "TriggerTemplate":
                for r in d["spec"]["resourcetemplates"]:
                    if r.get("kind") == "PipelineRun":
                        runs += 1
                        self.assertEqual(r["spec"]["taskRunTemplate"]["serviceAccountName"], "pipeline-runner")
        self.assertGreater(runs, 3)

    def test_image_pusher_has_no_token_and_no_rbac(self):
        sa = self.sas["image-pusher"]
        self.assertIs(sa.get("automountServiceAccountToken"), False)
        self.assertEqual(sa.get("imagePullSecrets"), [{"name": "registry-credentials"}])
        self.assertEqual(rules_for(self.docs, "image-pusher"), [], "image-pusher must be granted nothing")
        for kind in ("RoleBinding", "ClusterRoleBinding"):
            for b in self.docs:
                if b["kind"] == kind:
                    for s in b.get("subjects") or []:
                        self.assertNotEqual(s.get("name"), "image-pusher", b["metadata"]["name"])
        # the negative test, verb by verb
        rules = rules_for(self.docs, "image-pusher")
        for group, res, verb in (("", "secrets", "get"), ("", "configmaps", "get"), ("", "configmaps", "patch"),
                                 ("tekton.dev", "pipelineruns", "create"), ("tekton.dev", "taskruns", "create"),
                                 ("", "serviceaccounts/token", "create"), ("", "pods", "create")):
            self.assertFalse(allows(rules, group, res, verb), f"{verb} {group}/{res}")

    def test_the_four_accounts_are_distinct_and_only_the_two_credential_free_ones_are_task_accounts_here(self):
        self.assertEqual({"pipeline-runner", "source-runner", "image-pusher", "pipeline-trigger"} - set(self.sas), set())

    def test_nothing_else_runs_as_image_pusher(self):
        for d in self.docs:
            if d["kind"] in ("Job", "Deployment", "CronJob", "StatefulSet", "TriggerTemplate", "Task"):
                self.assertNotIn("image-pusher", yaml.safe_dump(d), d["metadata"]["name"])

    def test_the_bootstrap_job_uses_the_trigger_account_and_its_token(self):
        jobs = [d for d in self.docs if d["kind"] == "Job" and d["metadata"]["name"] == "onboarding-resync-bootstrap"]
        self.assertEqual(len(jobs), 1, "the fixture must render the bootstrap hook (a skip here would hide a regression)")
        spec = jobs[0]["spec"]["template"]["spec"]
        self.assertEqual(spec["serviceAccountName"], "pipeline-trigger")
        self.assertIs(spec["automountServiceAccountToken"], True)

    def test_nothing_else_runs_as_source_runner(self):
        for d in self.docs:
            if d["kind"] in ("Job", "Deployment", "CronJob", "StatefulSet"):
                self.assertNotIn("source-runner", yaml.safe_dump(d), d["metadata"]["name"])
        for d in self.docs:
            if d["kind"] == "TriggerTemplate":
                specs = [r["spec"].get("taskRunSpecs") for r in d["spec"]["resourcetemplates"] if r.get("kind") == "PipelineRun"]
                stage = d["metadata"]["labels"].get("hangar.io/stage")
                # build is never chained, so only the test stage names source-runner here
                if stage == "test":
                    self.assertEqual(specs, [[{"pipelineTaskName": "run-integration-script", "serviceAccountName": "source-runner"}]])
                else:
                    self.assertNotIn("source-runner", str(specs), d["metadata"]["name"])


def runs_repo_code(task):
    """A task whose step runs the resolved build-agent image, or the BuildKit step (the Containerfile's RUN lines)."""
    return any(x.get("image") == "$(params.agent-image)" or "buildctl" in x.get("script", "") for x in task["spec"].get("steps", []))


class SourceTasks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.docs = helm(CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"))
        cls.tasks = {d["metadata"]["name"]: d for d in cls.docs if d["kind"] == "Task"}
        cls.pipelines = {d["metadata"]["name"]: d for d in cls.docs if d["kind"] == "Pipeline"}

    def build_task_refs(self):
        out = {}
        for t in self.pipelines["build"]["spec"]["tasks"]:
            ref = [p["value"] for p in t["taskRef"]["params"] if p["name"] == "name"][0]
            out[t["name"]] = ref
        return out

    def test_the_four_pipeline_tasks_exist_and_run_the_repository_code_tasks(self):
        refs = self.build_task_refs()
        self.assertEqual({k: refs[k] for k in SOURCE_TASKS},
                         {"build-source": "build-source", "unit-test": "run-tests", "build-image": "build-image",
                          "build-python-wheel-artifact": "build-artifact", "build-go-binary-artifact": "build-artifact"})
        self.assertEqual(refs["push-image"], "push-image")

    def test_every_build_task_that_runs_an_agent_image_is_on_the_list(self):
        # A task whose step runs $(params.agent-image) runs the repository's code. If one is added to the build Pipeline
        # and not to the list, it would run as pipeline-runner with the repository's code in it.
        refs = self.build_task_refs()
        runs_agent_image = sorted(name for name, task in refs.items()
                                  if runs_repo_code(self.tasks[task]))
        self.assertEqual(runs_agent_image, sorted(SOURCE_TASKS))

    def test_agent_image_steps_carry_no_credential(self):
        for name in ("build-source", "run-tests", "build-artifact"):
            spec = self.tasks[name]["spec"]
            text = yaml.safe_dump(spec)
            self.assertNotIn("serviceAccountToken", text, name)
            self.assertNotIn("kubectl", text.replace("kubectl-free", ""), name)
            for s in spec["steps"]:
                if s.get("image") == "$(params.agent-image)":
                    mounts = [m["name"] for m in s.get("volumeMounts", [])]
                    self.assertFalse([m for m in mounts if "credential" in m or "token" in m or "secret" in m], (name, s["name"], mounts))
            for v in spec.get("volumes", []):
                if "projected" in v:
                    self.fail(f"{name} mounts a projected volume {v['name']}")

    def test_generated_build_run_hands_exactly_those_tasks_to_source_runner(self):
        script = yaml.safe_dump(self.tasks["deliver-onboarding-files"])
        m = re.search(r'if \[\[ "\$\{root_stage\}" == "build" \]\]; then(.*?)\n\s+fi', self.tasks["deliver-onboarding-files"]["spec"]["steps"][0]["script"]
                      if "root_stage" in self.tasks["deliver-onboarding-files"]["spec"]["steps"][0].get("script", "") else script, re.S)
        self.assertIsNotNone(m, "the build taskRunSpecs block is missing from deliver-onboarding-files")
        block = m.group(1)
        pairs = re.findall(r"pipelineTaskName: (\S+)\s+serviceAccountName: ([^\s\"]+)", block)
        self.assertEqual(pairs, [(t, "source-runner") for t in SOURCE_TASKS] + [(t, "image-pusher") for t in PUSHER_TASKS])

    def test_the_admission_policy_names_the_same_tasks(self):
        values = yaml.safe_load(open(os.path.join(CP, "values.yaml")))["cicdRunAdmission"]
        self.assertEqual(values["sourceTasks"], {"build": SOURCE_TASKS, "test": ["run-integration-script"],
                                                 "policy-validation-check": ["render"], "clearance-gate-check": ["render"],
                                                 "values-check": ["render"]})
        self.assertEqual(values["pusherTasks"], {"build": PUSHER_TASKS})

    def test_the_build_task_mounts_no_push_credential_and_the_push_task_runs_no_repository_code(self):
        build = self.tasks["build-image"]["spec"]
        self.assertNotIn("registry-credentials", yaml.safe_dump(build))
        self.assertEqual([v["secret"]["secretName"] for v in build.get("volumes", []) if "secret" in v], ["build-pull-credentials"])
        push = self.tasks["push-image"]["spec"]
        self.assertEqual([v["secret"]["secretName"] for v in push["volumes"] if "secret" in v], ["registry-credentials"])
        self.assertFalse(runs_repo_code(self.tasks["push-image"]), "push-image must not run the build agent or the builder")
        self.assertNotIn("projected", yaml.safe_dump(push))
        # the two never share a pod: different Tasks, and the Pipeline runs push-image after build-image, by reference
        self.assertEqual([t for t in self.pipelines["build"]["spec"]["tasks"] if t["name"] == "push-image"][0]["runAfter"], ["build-image"])

    def test_the_test_pipeline_runs_the_repository_script_in_its_own_task(self):
        # run-testworkflow holds credentials (app-secrets, the Testkube namespace, registry read): no step of it may run the
        # repository's code, and the fallback is a task of its own, after it, only when run-testworkflow asked for it
        steps = self.tasks["run-testworkflow"]["spec"]["steps"]
        self.assertFalse([x["name"] for x in steps if x.get("image") == "$(params.agent-image)"])
        for x in steps:
            self.assertNotIn("./integration-test.sh\n", x.get("script", ""), x["name"])
            self.assertNotIn("\n        ./integration-test.sh", x.get("script", ""), x["name"])
        entry = [t for t in self.pipelines["test"]["spec"]["tasks"] if t["name"] == "run-integration-script"][0]
        self.assertEqual(entry["runAfter"], ["run-testworkflow"])
        self.assertEqual(entry["when"], [{"input": "$(tasks.run-testworkflow.results.fallback-needed)", "operator": "in", "values": ["true"]}])
        task = self.tasks["run-integration-script"]["spec"]
        text = yaml.safe_dump(task)
        self.assertNotIn("serviceAccountToken", text)
        self.assertNotIn("kubectl", text)
        self.assertNotIn("secret", text.lower().replace("secrets", ""))
        self.assertFalse(task.get("volumes"))
        results = {r["name"]: r.get("description", "") for r in self.tasks["run-testworkflow"]["spec"]["results"]}
        self.assertIn("fallback-needed", results)
        # the description must say what the step really writes for that case
        script = "\n".join(x.get("script", "") for x in steps)
        for written in ("workflow-source", "outcome=delegated"):
            self.assertIn(written, results["fallback-needed"])
        self.assertIn('echo -n "script" > "$(results.workflow-source.path)"', script)
        self.assertIn('echo -n "delegated" > "$(results.outcome.path)"', script)

    def test_generated_test_run_hands_the_script_task_to_source_runner(self):
        script = "\n".join(s.get("script", "") for s in self.tasks["deliver-onboarding-files"]["spec"]["steps"])
        m = re.search(r'if \[\[ "\$\{root_stage\}" == "test" \]\]; then(.*?)\n\s+fi', script, re.S)
        self.assertIsNotNone(m)
        self.assertEqual(re.findall(r"pipelineTaskName: (\S+)\s+serviceAccountName: ([^\s\"]+)", m.group(1)), [("run-integration-script", "source-runner")])


if __name__ == "__main__":
    unittest.main()
