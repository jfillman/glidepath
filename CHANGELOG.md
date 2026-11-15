# Changelog

Notable changes to Glidepath. Newest first.

## Unreleased

### Pipeline trust, slice T1: the image build and the registry push credential are separate pods

- `build-image` (runs the `Containerfile` `RUN` lines, as `source-runner`) no longer mounts the registry push credential and pushes
  nothing: it builds an OCI layout into the workspace. New Task `push-image` (a separate pod, new `image-pusher` ServiceAccount: no
  Role, no token) validates that layout (no symlinks or traversal, digests and sizes checked, media types and platforms on an
  allowlist) and pushes it with `oras cp --from-oci-layout` to the repository the run's `image-repo` names. The image digest and
  reference every later task uses (scan, SBOM, API-spec attestation, the Pipeline's `IMAGE_URL`/`IMAGE_DIGEST`) now come from
  `push-image`; `build-image`'s only result is `version`, which `push-image` checks as a tag.
- A pull request's build imports the main BuildKit cache and exports none; a main build exports its cache to the workspace and
  `push-image` publishes it. New optional read-only pull credential for private base images and caches: catalog
  `buildPullCredential.secretName`, control-plane `buildPullCredential.remoteKey` (the push credential is refused for both).
- Admission: `image-pusher` only on `push-image` of the `build` Pipeline (`cicdRunAdmission.pusherTasks`), required by
  `cicd-run-source-isolation`; a build that a push did not start must pass `pr-tag-only=true`, which needs the `-pr` repository.
- `push-image` hardening: exact-key allowlists and duplicate-key detection for every JSON document it interprets (a `Layers` next to `layers` refuses), symlink checks on the whole path, whole-string name matching, tag-overwrite refusal for main tags, `image-pinned-ref` (`repo@digest`) for every later task, and a main cache that fails validation is skipped with a warning instead of failing the task.
- **Upgrade:** release the three charts together, resync every app's `.tekton/`, supply the pull credential where bases are private, raise
  `build.sourceVolume.size` for large images (the workspace now holds the image and cache layouts). docs/admin/security.md,
  "Image build and push" and the upgrade note.


### Pipeline least privilege: the repository's code runs with no Kubernetes credential

- New `source-runner` ServiceAccount (no Role, no token): `build-source`, `unit-test` and the two artifact builds, the tasks that run
  `build.sh`, `test.sh` and the dependency installs, run as it (`taskRunSpecs` in the generated `.tekton/` build run).
- New `pipeline-trigger` ServiceAccount, the only one that may create PipelineRuns (the Triggers, impersonated by the broker, and
  the onboarding bootstrap Job). `pipeline-runner` can no longer create `pipelineruns` or `taskruns`; the role
  `allow-broker-impersonate-pipeline-runner` is now `allow-broker-impersonate-pipeline-trigger`.
- glidepath-control-plane: admission policies `cicd-run-shape` and `cicd-run-source-isolation` for runs Pipelines-as-Code creates
  from a `.tekton/` file (values `cicdRunAdmission`). **Upgrade order and what is not covered yet: docs/admin/security.md,
  "Pipeline identities".** `glidepath-app` and `glidepath-catalog` release together; resync every app's `.tekton/` before
  setting `sourceIsolationActions` to `[Deny]`.
- `./integration-test.sh` (the test fallback) moves out of `run-testworkflow` into its own Task `run-integration-script`, run as
  `source-runner`; `run-testworkflow` reports `fallback-needed` and, for that case, `outcome: delegated`.
- The admission policy allows only the Pipelines the platform starts from a `.tekton` file, with their parameters validated and tied
  to what Pipelines-as-Code recorded; deploy/test/release start only from a push to `cicdRunAdmission.protectedBranches` or a tag.
- `build-image` runs as `source-runner` too (its Containerfile `RUN` lines are the repository's code): no Kubernetes token in that pod.
- `build-artifact`'s publish step pushes only a regular file inside its output directory (no symlink, no `..`);
  `attest-api-spec` refuses a spec path that is a symlink, behind one, or climbs out; `sast-scan` scans a copy without `.git` or
  symlinks.
- Admission: tags are decided on the full ref (a branch named `refs/tags/v1.0.0` is not a tag); `image-repo` must be the app's own
  repository; `onboarding-resync` may target only `gitops-<app>` under the repository's owner, from a push; accounts per task per
  Pipeline. A repository TestWorkflow is checked before it is applied; `helm template` in the gates refuses symlinks out of the chart and
  no longer prints helm's error.
- Clearance follow-up (not in this release): trust `app-*-cicd/clearance-gate`, then run the gate as it, then drop
  `app-*-cicd/pipeline-runner`.

### A-11: the `propose` scope class (contract version 2)

- App repos get the post-merge audit too: onboarding delivers `.tekton/push-propose-merge-check.yaml` (Pipeline `propose-merge-check`, Task
  `detect-propose-merge`): a PR marked for a person (`Hangar-Merge: person`, or Clearance's "needs a person" check) merged by a bot,
  an agents' login, a non-User account or no named merger is alerted to Slack. Quiet otherwise; never fails the run.

- `agentScope` gains **`propose`**: an agent may add or change a Flight environment, the release step into it and a new
  environment's `cluster` and `production`, but only in a pull request a person must merge. Rules may set `proposeOn`
  (`added`, `changed`, `removed`): any other kind of change to the field is `human`, so a new environment's cluster is
  propose and an existing one's still is not. Secrets, signers, the gates, `deploy.target`, `deploy.chart`, the cloud blocks,
  `api`, `gitopsRepo` and `manifestPath` stay `human`. Docs: contract.md, "propose".
- `tools/cicd_scope.py`: `needs_person(base, head, scope)` lists what is propose; `scope_problems` no longer refuses a Flight
  environment added or changed (GP-SCOPE-003 is now removing one, or any tier change). `glidepath validate`, the validator
  sidecar and `POST /v1/apps/{app}/cicd/validations` answer `needsPerson` (always present); `changes[].agentScope` can read
  `propose`. The `cicd-validate-` check passes such a change and says "needs a person to merge".
- **`contractVersion` is 2.** Roll out Clearance, then Autopilot (its bundle names version 2), then this.
- Catalog: a Clearance `agent-scope` check that is neutral and titled `needs a person: ...` is not success to any reader
  (neutralOkJq), so it holds a release until Clearance posts success (a person merged) or failure (a bot did).
  `detect-bypass-merge` reads it as success only when the PR's merger is a person (account type User, not a `[bot]` login, not in
  `agentGates.agentLogins`), and reports a bypass otherwise.

### C-7: the agent gates move to Clearance's GitHub App (opt-in per cluster)

- New catalog values `agentGates.clearanceChecks` (default `false`) and `agentGates.clearanceAppId`. With
  `agentGates.clearanceUrl` set and `clearanceChecks: true`, the release guardrail reads the Clearance GitHub App's check
  runs `Clearance / agent-identity` and `Clearance / agent-scope` instead of Glidepath's `agent-gates-` check. The
  registry has two new entries (`agent-identity`, `agent-scope`, class integrity, required) and every consumer (the
  ruleset scripts, the promotion fan-in, PROM-03, the guardrail wait, `detect-bypass-merge`, the release log and the
  release PR's governance table) matches them by their exact names. A neutral conclusion (Clearance's `unattested`) counts
  as passing.
- Registry entries may now carry `check` (the exact check-run name another App posts), `integration` and
  `clearanceChecks` (present only when the Clearance checks are on, or off).
- Onboarding stops delivering `.tekton/pull-request-agent-gates.yaml` to app and gitops repos when the switch is on, and
  removes the copy it delivered earlier (the resync PR deletes it).
- The fan-in waits up to two minutes for a check another App reports, which has no finally task to ask again when it lands.
- Without Clearance (empty `clearanceUrl`), or with `clearanceChecks` off, nothing changes: `agent-gates-` stays the
  guardrail and still fails PRs that look like an agent's.
- `hack/apply-guardrail-ruleset.sh` and `hack/apply-agent-gates-ruleset.sh` build the Clearance checks under the App's id
  (`-f <values>`, `--clearance-app-id`) and refuse without it.
- Migration steps and the rollout order: docs/admin/agent-gates.md, "Migration".
