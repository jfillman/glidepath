# Testing: the test stage, the check ride, and the evidence a release carries

Glidepath runs tests through Testkube and owns three things around it: the **contract**
(what a test step passes into a workflow and what it records), the **evidence** (the
result attested onto the image the release promotes), and the **fence** (where the
workflow a release depends on comes from, and who may change it). Everything else a test
platform needs (reports, retries, services, matrix runs, schedules) is Testkube's and is
not rebuilt here. The design and its roadmap are in Hangar's library, *A test contract
for Glidepath, and a QA gate that reads it* (2026-10-17); this page is the reference for
what is built.

The user-facing half (writing a workflow, the `test:` block, the config a workflow can
declare) is in [cicd-yaml-reference.md](../user/cicd-yaml-reference.md), "The test:
block". Platform pieces: [ADR-0007](adr/0007-testkube-shared-namespace.md) (one shared
namespace), [ADR-0008](adr/0008-kyverno-testkube-secret-policy.md) (the secret policy).

## Three roles for a test step

A `test` step in `cicd.yaml` has a `role` (`pipelines.<flow>.steps[].role`):

| Role | Workflow | Missing workflow | Chains the next stage | Evidence |
|---|---|---|---|---|
| `test` (default) | `glidepath/<testName>.yaml` in the app repo, applied by the stage | fails | on pass only (unless `required: false`) | results in the provenance; not what the gate reads |
| `execute` | same | fails | same | same; no test counts expected |
| `check-ride` | `<app>-check-ride`, synced from the check-rides repository; never applied from the app repo | fails, naming the file to add | on pass only; always required | the run the `qa` gate reads |

`required: false` lets a failed `test` or `execute` run be recorded as failed without
stopping the flow (a best-effort cache flush). `timeout` (`Ns`/`Nm`/`Nh`, default
`10m`) is how long the stage waits for the execution to finish; past it the outcome is
`timeout`. A flow has at most one check-ride step, and it takes no `testName`.

`skipped` is reserved for `test.enabled: false`. A test step whose workflow does not
exist fails: before 2026-10-19 it passed, which made a typo in `test.name`
indistinguishable from a passed test.

## The contract

`run-testworkflow` passes the platform's context to the workflow and records the
result in a fixed shape. The same fields ride on three carriers:

| Field | Task result | In the provenance (Chains) | CDEvent `testcaserun.finished` |
|---|---|---|---|
| `outcome` (`passed`/`failed`/`timeout`/`skipped`) | yes | yes | yes (as `outcome`) |
| `role` | yes | yes | yes |
| `execution-id` | yes | yes | |
| `workflow-source` (`argocd:<app>`, `repo:<url>@<rev>`, `script`) | yes | yes | |
| `workflow-digest` (sha256 of the canonical spec that ran) | yes | yes | |
| `tests-total`, `tests-passed`, `tests-failed`, `tests-skipped` | yes | yes | |
| `report-path` (the JUnit artifact parsed) | yes | yes | |
| `step-summary` (one line per Testkube step) | yes | yes | |
| `image-repo`, `image-digest` (`IMAGE_URL`, `IMAGE_DIGEST`) | yes | yes (subject) | `image` |
| `test-name`, `env`, `revision`, `git-url` | params | yes | yes |

Inputs, for every key the workflow declares under `spec.config`: `app`, `env`, `image`,
`url` (`http://<app>.<type>-<app>-<env>.svc.cluster.local:<port>`), `revision`,
`gitUrl`, `chain`. The same values, plus `test` and `role`, are set as execution tags on
every run, so a run can be found by what it tested (`tagSelector` on Testkube's list API).

Test counts come from the JUnit report the workflow leaves under `data/artifacts/`
(`junit.xml` by convention, else the first `*.xml` artifact that is one). Testkube CE
stores the artifact and does not parse it (`reports` is empty on 2.13.1, checked live),
so the stage downloads it from the execution's artifacts and counts `<testcase>`,
`<failure>`+`<error>` and `<skipped>`.

## The evidence: provenance from Tekton Chains

Nothing in the test stage signs anything. `test.yaml` carries the same Pipeline-level
`IMAGE_URL`/`IMAGE_DIGEST` results `build.yaml` does (Chains' type hints), filled from
`run-testworkflow`'s `image-repo`/`image-digest`. Tekton Chains, already configured for
in-toto provenance to OCI storage, therefore signs every successful test PipelineRun's
SLSA provenance and attaches it to the tested image digest in the registry, next to the
build's. Its `predicate.buildConfig.tasks[]` entry named `run-testworkflow` carries every
result above.

That is exactly the mechanism `verify-sast-attestation` uses to read `sast-scan`'s
outcome out of the build's provenance, and the `qa` gate reads a check ride the same way:
download the image's attestations, verify each against the platform's Fulcio identity
and Rekor, keep the ones whose `run-testworkflow` entry has `role: check-ride`, apply the
policy (`workflow-source` is the check-rides Application, `outcome: passed`,
`tests-failed: 0`, age) and pass or fail with the counts in the comment. The gate is phase
3 of the spec; today `qa` is still a stub.

Reading it by hand, from a pod that has the image's registry credentials and the
cluster's trusted root (the gate Tasks show the exact flags):

```bash
cosign download attestation ghcr.io/<owner>/<app>@sha256:<digest> \
  | jq -r '.payload | @base64d' \
  | jq 'select(.predicateType == "https://slsa.dev/provenance/v0.2")
        | .predicate.buildConfig.tasks[] | select(.name == "run-testworkflow")
        | .results | map({(.name): .value}) | add'
```

Chains attests every completed test PipelineRun, failed ones included (seen live on
2026-10-19: the first boarding-api run failed in the Task after writing its results, and
its provenance is on the digest). So the gate never treats the presence of a check-ride
entry as a pass: it reads `outcome`, `tests-failed` and `workflow-source` out of the
entry, and a digest can carry several test provenances (re-runs, the team's own tests),
of which the gate picks the check-ride ones.

Live on hangar-dev, 2026-10-19, boarding-api `2.0.0-926d566`: five attestations on the
digest - the SBOM, the build's provenance, and three test provenances, among them the
check ride's with `outcome=passed`, `role=check-ride`, `tests-total=8`,
`tests-failed=0`, `workflow-source=argocd:check-rides`.

A test-only, git-rooted flow has no image, so its provenance has nothing to attach to;
the Task results and the CDEvent still carry the result.

## The fence: the check-rides repository

The check ride is the one workflow a release depends on, so it does not live with the
team's own tests. It lives in the shared `check-rides` repository:

- `templates/<class>.yaml`: a `TestWorkflowTemplate` per service class, the standard
  (which steps every ride runs, in which order, with what budget, and that a JUnit
  report must be produced). Owned by SDETs and product owners.
- `apps/<app>/check-ride.yaml`: the team's `TestWorkflow` that `use`s a template and
  fills its config, above all `command`, the line that runs the team's suite inside the
  image under test. Owned by the team and an SDET.
- `CODEOWNERS` and a ruleset make the ownership real (both groups listed, two required
  approvals, code-owner review). On GitHub Free a private repository does not enforce it;
  the repo holds no secrets and can be public.

An Argo CD Application in the cluster repo (`50-glidepath/check-rides`) syncs
`templates/` and `apps/` into the `testkube` namespace with prune and self-heal. The test
stage runs `<app>-check-ride` from there and refuses one that carries no Argo CD tracking
annotation or another app's `hangar.io/app` label. The `testkube-secret-usage` policy lets
a synced workflow reference only `<hangar.io/app label>-app-secrets`, and the repo's own
CI keeps that label equal to the `apps/<app>/` folder.

Adding an app's check ride: copy `apps/boarding-api/check-ride.yaml`, set the name,
label, `command` and (optionally) `versionPath`, add the CODEOWNERS line, open a PR. The
app also needs `networkPolicy: {allowIngressFrom: [{namespace: testkube}]}` in its
`glidepath/base.yaml`: the environment chart's NetworkPolicy admits same-namespace pods
only. A platform-wide default for that is on the roadmap.

## What Testkube gives, and where the free agent stops

Used: templates with typed `config` and top-level `use`, execution tags and
`tagSelector`, artifacts, step `retry`/`timeout`/`condition`, services and matrix runs,
schedules, webhooks and CDEvents (not wired yet). Not in the free agent: per-tenant
execution namespaces (ADR-0007), per-run sensitive config (ADR-0008), a dashboard (the
OSS viewer uploads to view.testkube.io, so not for private results; Tower's Tests view
is the plan), and parsed JUnit in the record (hence the stage parses it).

Two agent findings worth knowing: executions of short workflows report `aborted` while
every step passed (a race with the Job controller, present through 2.13.1); the stage
trusts the steps. And chart 2.13.2 and later hardcode the bundled MinIO's command, which
breaks the drop-in image this lab uses; the cluster repo pins 2.13.1 with the reasons.

## Roadmap, as of 2026-10-19

Done and live-verified on boarding-api: phase 0 (the stage passes context, fails on a
missing workflow, timeout, pull secret), phase 1 (roles, the contract, provenance),
phase 2 (the check-rides repository, sync, policy). Next: phase 3, the real `qa` gate reading the provenance; phase 4, Tower's
primary-test strip, readiness row and Tests view; later, a check ride against a Flight
environment on *release healthy*, Testkube's CDEvents into the broker instead of
polling, and waivers.
