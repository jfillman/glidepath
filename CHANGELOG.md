# Changelog

Notable changes to Glidepath. Newest first.

## Unreleased

### W-48: a platform release-file commit is recognised by who pushed it, not by its committer

- `clearance-gate`'s release source (`files/lib/release-source.sh`, step 3b, condition 2) no longer checks a deploy commit's committer. GitHub signs an App's API commit with GitHub (`web-flow`) as committer, so the old check never passed (flight-api `ce1f7f7`, the first deploy after glidepath#290, stayed unvouched). The commit's author is not used either: anyone with write access can create a web-flow-signed commit naming the App's bot as author. Condition 2 now reads GitHub's repository activity (`GET /repos/{owner}/{repo}/activity`): the commit must be the new tip of a `push` to the released branch, moved from its first parent, whose actor is the pipeline's App (typed `Bot`). At most 10 pages of 100 of the App's pushes are read. Anything else, or a log that cannot be read, keeps the commit counted. docs/admin/clearance-gate.md, "Platform release-file commits"; known-gaps #45.
- Tests: `clearance_gate_release_source_test.sh` section 15 (a forged-author web-flow commit pushed by a person, another bot, a User-typed actor, another branch, a force push, another parent, paging and the page cap, an unreadable log); `release_file_commits_test.sh` checks the one-commit, no-force ref update the gate looks up.

### Platform release-file commits no longer block a production release

- `deploy-manifests` now commits a Ground environment's release file through GitHub's Git Data API with the App's installation token, and no author or committer. GitHub signs that commit, and its committer is the App's bot. It no longer runs `git push`. It still retries, and when main moves between the clone and the ref update (a 422, not a fast-forward) it re-clones and commits again, at most 3 attempts. The Argo CD refresh and the wait are unchanged.
- The `clearance-gate` release source (`files/lib/release-source.sh`) leaves a commit no PR contains out of `noPr` only when all four hold. GitHub verified it. Its committer is the pipeline's App (`clearanceGate.pipelineLogins`, or else the release PR's Bot author). Its full diff against its first parent has only resolved release files. None of them is the released environment's. The comment lists these commits on their own line, "platform release-file commits, not counted". When an environment is itself released, commits to its own release file stay counted.
- Unvouched commits (clearance#213): when the only cause of an `unknown` proposer is commits no merged PR contains (every source PR vouched for, a human named), the change carries `provenanceReason: unvouched-commits` and `unvouchedCommits` (the noPr left after the skip above), and Clearance holds it at tier 3 for two people. A source PR that is itself unknown sends `unrecorded-agent` (denied). Anything else sends nothing. A core that refuses the fields as unknown gets the change again without them, which is denied as before (fail closed), and the comment says so. The register step now allows three fallbacks instead of two.
- The `DG-PROVENANCE` note under a denial now names the actual cause (commits no PR contains, an unresolved source, or the agent case) instead of always naming the agent case. docs/admin/clearance-gate.md, "Platform release-file commits".
- Tests: `charts/glidepath-catalog/tests/release_file_commits_test.sh` (new, in catalog-ci `policy-validation`), and section 15 of `clearance_gate_release_source_test.sh`.

### I-1 interim guard: a flow may not have two release steps

- `validateFlows` (glidepath-app) now refuses a flow with more than one `release` step, naming the flow and both steps. A release record is named per release chain (one flow), so two release steps wrote the same record and the second overwrote the first's PR, image and state. The owner chose this interim guard (option a, strip Y-41); per-environment record names (option b, hangar `specs/plans/i1-release-record-per-env.md`) stay deferred and would lift the limit. A scan of the 14 onboarded apps' `cicd.yaml` found at most one release step per flow. docs/user/cicd-yaml-reference.md, "How flows work". Test: `charts/glidepath-app/tests/test_roles_test.py`.

### W-8: api-compat and policy-validation hand their report over in the step log (`gateReports.transport`)

- New catalog value `gateReports.transport` (`configmap` default: renders byte for byte as before; `log`). With `log`, `api-compat-gate` and `policy-validation-gate` write no ConfigMap: the full report goes into the gate step's own log as `glidepath:gate-report` lines (gzip + base64 chunks; their digest also goes to a new `report-digest` Task result), and `comment-pr-check-result` reads it from the TaskRuns of its own PipelineRun that run that gate's Task (step container `step-check` / `step-evaluate`), only the set that result names, never from a ConfigMap. Its log excerpts drop those lines. No new right, audience or endpoint; `pipeline-runner`'s `pods/log` read (kept by T4) does it. These were the last `pipeline-runner` ConfigMap writes once `promotion.gateLedger.writer` is `api`, so W-8 step 6 can remove `create`, `patch` and `delete` on ConfigMaps after both switches. `clearance-gate`'s report is unchanged. docs/admin/security.md, "Pipeline identities".
- Tests: `charts/glidepath-catalog/tests/gate_report_log_test.sh` (new, in catalog-ci), log-mode cases in `api_compat_test.sh` and `policy_validation_test.sh`.

### W-8: glidepath-api writes the gate ledger (`POST /v1/apps/{app}/pipelineruns/{name}/gate-verdict`)

- New glidepath-api route (API 0.14.0): records a gate run's verdict in the release record's gate ledger as glidepath-api's own account. Only the run's own `comment-pr` pod may call it, with a projected token for the new audience `glidepath-gate-verdict` (TokenReview with that audience, then the token's pod claims); the verdict, PR, commit and gate are derived from the PipelineRun and its TaskRuns, never from the request; one write per run (`gateRun.<sha12>.<gate>`), an older run never overwrites a newer one; resourceVersion CAS as before. docs/admin/api.md, "Gate verdicts".
- glidepath-app: Role `glidepath-api-gate-ledger` (`configmaps` `patch` only) in each app's `-cicd` namespace for `platform-system/glidepath-api`. Control plane: `gate-ledger-writes` ValidatingAdmissionPolicy (`cicdRunAdmission.gateLedgerActions`, default `[Deny]`): glidepath-api may only patch the ledger keys of an existing release record; new value `api.gateVerdict.serviceAccounts` (default `[pipeline-runner]`).
- Catalog: `promotion.gateLedger.writer` (`direct` default: unchanged; `api`: `promotion-ready` calls the route, never patches a ConfigMap, reads the other gates from the answer, and when the API does not answer after a bounded retry decides from the check runs alone and exits 1 so it shows). Switch to `api` once the glidepath-api image with the route is live; W-8 step 6 follows.
- Tests: `glidepath/api/internal/gateverdict`, `internal/server/gateverdict_test.go`, `record_policies_test.py` (gate-ledger-writes), `pipeline_least_privilege_test.py`, `promotion_fan_in_test.sh` (api scenarios).

### Pipeline trust T4, step 3: the writer, Clearance-token and test tasks run as their own accounts

- Every run the platform starts now names the per-task account in `taskRunSpecs`: `open-release-pr`, `mark-release-merged`, `itsm-*` as `release-record-writer`; `clearance-gate` (fetch, register, render-check), `agent-gates`, `agent-definitions` and `agent-release` as `clearance-gate`; `run-testworkflow` as `test-runner`. Trigger templates and the onboarding templates (resynced into each app's `.tekton/`) both carry it; the onboarding task fills in the task names the catalog renders (`task_names`). New chart value `itsm.enabled` (default `false`) for the two Triggers that start the itsm close tasks. `pipeline-runner` keeps every right until step 6.
- The Clearance join moved to the `join-release` finally task of `clearance-gate-check` (fed by a new `joins` result of the clearance-gate Task, which no longer writes the release record). The gate-ledger write of `comment-pr-check-result` stays: Tekton cannot order finally tasks (see docs/admin/security.md, "Task mapping").

### ref guard: an Off-here rerun names its real ref (I-11)

- With `refProtection.mode: off`, a rerun or chained run (no Pipelines-as-Code annotations) recorded "the commits of unknown" in `ref-protection-summary`. It now names the run's `glidepath.io/origin-ref`, or says it is a platform-created run when none is stamped. Cosmetic: the verdict and result are unchanged. Test: `charts/glidepath-catalog/tests/ref_protection_pipelines_test.py`.

### Pipeline trust T4 step 5: `release-record-writes` and `gate-report-writes` (Audit only)

- Two ValidatingAdmissionPolicies for ConfigMaps in `-cicd` namespaces: `pipeline-runner` and `clearance-gate` may not write a release record (`release-tracking-*` or `hangar.io/subcomponent=release-tracking`); `release-record-writer` may touch only release records and may not change `prUrl`, `releaseId`, `image`, `imageDigest`, `gitRevision` or `kind` once set; `clearance-gate` and `pipeline-runner` may write only `gate-report-<run>` owned by that PipelineRun. `cicdRunAdmission.recordWritesActions` and `gateReportActions` default to `[Audit]`: nothing is refused. docs/admin/security.md, "Pipeline identities". Tests: `charts/glidepath-control-plane/tests/record_policies_test.py`.

### stalled-pipeline detector: catch a PaC queue that waits with free slots (R-4)

- Pipelines-as-Code keeps its `concurrency_limit` queue in the watcher's memory; a missed completion leaks a slot and later runs stay `PipelineRunPending` with nothing running (SRVKP-7608). The stalled-pipeline detector now also flags a run that is pending with state `queued`, older than `stalledPipelineDetector.queueStallMinutes` (default 10), while fewer runs of the same Repository (`pipelinesascode.tekton.dev/repository`) are started and unfinished than its `concurrency_limit`. It logs `QUEUE STALL DETECTED` and creates one `QueueStalled` Warning Event per repository, with the repository, queued run, wait and running-against-limit counts and the watcher-restart recovery; it labels the stalled runs `hangar.io/stall-alerted` and changes nothing else. The ClusterRole gains read-only `get`/`list` on `repositories.pipelinesascode.tekton.dev`. docs/admin/stalled-pipeline-detector.md "Queue stalls", pipeline-performance.md. Tests: `charts/glidepath-control-plane/tests/stalled_queue_detector_test.sh`. On merge to `main` hangar-dev renders the CronJob within minutes.

### clearance-gate: a release is proposed by the author of its source, not by the release bot

- A release PR the pipeline opened is registered with Clearance under the author(s) of the app-repo commits it releases (previous release to this
  one) and the weakest provenance of their source PRs, not the pipeline's GitHub App (kind `person`, `not-agent`), which also let the author clear their
  own tier-2 change. Resolved from the release record, accepted only for a PR whose content is what the record says the pipeline wrote; an unresolvable
  source is registered `unknown` (Clearance denies it). `open-release-pr` keeps `imageDigest` and the promoter (`initiator`) on the record. Every author
  rides in `checks.sourceAuthors`; the promoter is in the gate's comment. See docs/admin/clearance-gate.md, "Who proposed a release".

### propose-merge-check no longer runs for the deploy write-back

- The app-repo audit `push-propose-merge-check.yaml` started on every push to main, including the pipeline's own deploy commits ("Deploy <app> to <env>: <image>"), which change only `glidepath/envs/<env>.release.yaml` and are never PR merges: a PipelineRun and pod per deploy per environment. It now does not start when every changed file, in every commit of the push, is such a release file, the push carries no `Hangar-Merge:` trailer and its tip is not a PR merge. `glidepath/`, `cicd.yaml` and `.tekton/` changes still start it. Checked with cel-go (`hack/ci/build-exemption-cel`, 21 payloads; a push of 20 or more commits, GitHub's cap, is never skipped, and the trailer is read from the head commit too). Existing app repos get it with their next onboarding resync PR. docs/admin/contract.md "propose".

### Release record: written the moment the PR exists, and it survives what comes after

- `open-release-pr` created the change request, pushed the branch and opened the PR, and only then called the change tool again and wrote the release record. 2026-11-16 (backstage 1.0.2 to prod) the change tool's link call failed during a cluster overload, the step exited 1 and no record was ever written; the PR merged and deployed with no Tower row, release log entry, lineage or lead time. Now the record is the first network call after the PR is created. The change link, closing stale proposals and linking a reissue come after it; the link is retried with backoff and, if it still fails, the failure is written on the record (`postPrError`, read by nothing) and logged, and the step succeeds (the itsm gate never reads that note). A re-run finds the open PR and finishes the job (record written or completed, the change id read from the PR's marker, no second PR or change; a record the relay moved on keeps its state). A PR call whose answer was lost adopts the PR it created. The branch's PRs are looked up in every state: a re-run whose branch's PR is already merged and is this release (default base, this cluster/environment, this image tag, still the environment's current tag) is refused, changing nothing, instead of opening a second PR and change. A re-run completes the record under the chain id named by the PR's commit (a rerun can mint a new one), accepted only as a lowercase uuid for this cluster/environment whose record, if any, is this app's and fails the run otherwise; it never blanks a field with an empty value, retries the change link until the record notes it (`changeLinkedAt`), and a link that cannot be made is also posted as one PR comment (a fixed sentence and a reason class, once per class set). docs/admin/release-state.md, tests/release_record_survives_test.py. On merge to `main` hangar-dev renders the Task within minutes.

### cicd-run-shape: a re-run of a push's check is a push to the same branch

- `cicd-run-shape` decided a push by the full ref only (`refs/heads/main`, `refs/tags/vX.Y.Z`). Pipelines-as-Code v0.52.0 records the other spelling when a check of a push is re-run from the git host (check_run / check_suite `rerequested`): `event-type: push` with `source-branch` = the check suite's `head_branch`, the bare name (`main`; observed on `bypass-check-42rhw`, `app-parachute-cicd`, the gitops repos' break-glass audit and release "merged" signal). Under `[Deny]` that run would be refused. A push is now also admitted when `source-branch` equals a `protectedBranches` entry exactly. A push to any other branch, in either spelling, a tag spelled bare, and every non-push event not aimed at a protected branch stay refused. Tests: the event shapes PaC records at runtime for every delivered template. docs/admin/security.md "What a .tekton file may start". On merge to `main` hangar-dev (Argo CD auto-sync, binding still `[Warn, Audit]`) renders the changed expression; nothing starts being refused, the warning count for this shape stops rising.

### Reissue a release PR that was closed unmerged

- `POST /v1/apps/{app}/releases/{chainId}/reissue` starts the app's `promote-release` run for a closed release's own image and commit; `open-release-pr` opens a fresh PR as a new release linked by `reissueOf`/`reissuedBy` (the old record ends `closed`). `GET /v1/apps/{app}/releases/reissuable` lists candidates. Refused unless the record is `proposed`/`closed`, its PR closed unmerged, it was not already reissued (`reissuedBy`), its image repository (exact) and tag match what the source environment's release file carries (the first environment: its own gitops release file, never other records), no newer release or open PR exists (checked again at PR time by `open-release-pr`, which then fails without changing anything), and the commit resolves. The old record is linked with a `resourceVersion`-preconditioned patch; `close_stale_proposals` never closes a release newer than the reissued one. docs/admin/api.md "Reissue a release PR", release-state.md, security.md.

### Promotion lineage: a promotion links back to the flow that built its image

- glidepath-api resolves the build of the image a promote or rollback releases, server-side, from the build's own run (on the cluster, else Tekton Results), bound to the image repository and the commit being
  released, and stamps `glidepath.io/origin-chain-id`, `-chain-slug`, `-trace-id` (and `-traceparent`, `-finished-at`) on the run. The promotion keeps its own chain. Unresolved or ambiguous is
  "origin unknown" and the promotion goes ahead. The release record gets `originChainId`, `originChainSlug`, `originTraceId`; the release read models return `origin`; `GET /v1/apps/{app}/releases?fromChain=` lists
  what came from a build. `stage-preflight` joins the build's trace when the build finished within `promotionLineage.traceJoinWindow` (default 12h, under Tempo's 24h retention) (only a promotion carrying validated origin annotations joins a trace; every other run of the task behaves as before). The lookup has a 5 s deadline and reads at most 20 archived builds; every stamped value is held to one table of shapes shared with the apron admission policy. docs/admin/api.md "Promotion lineage", docs/admin/tracing.md.

### Onboarding resync: "nothing to deliver" is read from the tree

- `deliver-onboarding-files` decided with `git status --porcelain | grep -q .` under `pipefail`: an early `grep` exit gives git SIGPIPE (141), the pipeline reads as failed and the
  `else` ("already up to date") ran for a gitops repository eleven files behind (lab, gate-api, twice; no resync PR, so no release check could start). Both halves now capture the status
  and test the value, log the number of files that differ (or were compared), and a failing `git` stops the step. The same pipe-into-`grep -q` is gone from the agent-login checks
  (`detect-bypass-merge`, `detect-propose-merge`, `cicd-validate`, `clearance-gate`, `agent-gates`, `agent-release`), the two `jq -e` reads of an API reply now fetch first, and two operator scripts under `hack/`.
  `tests/onboarding_resync_decision_test.sh` runs the rendered step against a local origin; `tests/pipefail_early_exit_test.py` fails on a new unannotated `| grep -q` (annotate `# pipe-ok: <reason>`).
  The upgrade note in docs/admin/security.md now says how to trigger a resync and what a repository that has not resynced shows.

### Pipeline trust T3b: ref protection rests on review evidence, not on bypass lists

- `GET /v1/apps/{app}/ref-protection` (API 0.13.0): `protected` now means every commit being released (the commits after the newest release the app's gitops repository corroborates, up to
  the released commit; a first release is the commit alone; at most 250) belongs to a pull request merged into the protected branch of the source repository
  and that pull request has an approving review on its final head, not dismissed, from someone who may push to the repository (GraphQL authorCanPushToRepository) who is not a bot, not its opener and not an author or committer
  of its commits; a merge commit that is not the approved head must be GitHub's (web-flow, verified), and a rebase-merged pull request counts only if what landed equals the reviewed head file by file (a rebase over a base that touched the same file is refused). The start of the range comes from merged release pull requests in the
  gitops repository for the run's own environment and cluster (identified by what was merged, never by title), never from the release-tracking ConfigMaps (the pipeline's account can write them); `api.pipelineLogins` must name the account that opens them. Verdicts `protected`, `unreviewed` (the commits are listed), `unverifiable`; `unprotected` and `unavailable` are gone. The answer carries
  `evidence` (commits, pull requests, openers, mergers, approvers: what the Clearance gate's proposer needs) and the ruleset and branch protection reads as an
  informational `configuration` that never decides. Read-only token only; no override yet (`evidence.accepted` is the empty place for an owner-declared baseline).
- **Removed:** `githubRead.refProtectionPermissions` and `githubRead.refProtectionCallers` (the control plane chart), and the broker's elevated `ref-protection` purpose:
  `/github-read-token` mints only contents, pull requests and metadata read. The platform's GitHub App is not asked for Administration. Values that still set them are ignored.
- **Upgrade: with `refProtection.mode: required`, a release whose commits were not each approved by someone other than their author is refused. A repository with a
  single maintainer cannot pass: set `off` (recorded) or add a reviewer.** A cluster on `off` (the lab) sees no change in behaviour: the guard asks nothing, the record still says
  "Off here". New control plane value `api.pipelineLogins` (pipeline accounts whose approval never counts). docs/admin/security.md "Ref protection".
- `hack/ci/vap-compile`: the package comment names the real flag `-minor`, and a truncated report line is printed instead of dropped.
- Guard and gate tasks unset the GitHub token variables in the step that fetches with it; three repository-name parses no longer cut a name at a dot (`app.example`).

### Fix: the control plane's admission policy `cicd-run-shape` compiles under the API server

- `cicd-run-shape` was refused by the Kubernetes API server (the Argo CD Application could not sync, and the policy was never created): the `catalog` variable was a
  CEL map literal with values of different shapes, and the tag pattern was an ordinary string holding `\.`. The catalog is now three one-shape values (Pipeline to
  parameter names, the push-only names, the any-branch names) and the pattern a raw string. Behaviour is unchanged.
- New CI check `hack/ci/vap-compile` (job `vap-compile`): a Go program that compiles every rendered `ValidatingAdmissionPolicy` with the API server's own CEL compiler,
  for the default values and for `tests/values.vap-full.yaml` (every optional list). The cel-python behaviour tests stay.

### Pipeline trust T3: verified ref protection

- Runs glidepath-api starts are verified from a stamped origin: a rerun copies the origin of the run it repeats (and no other annotation), promote and
  rollback stamp the release's commit and the configured protected branch (a rollback to an older commit of that branch passes by design). The cluster policy
  `glidepath-api-pipelinerun-shape` must allow the `glidepath.io/origin-*` annotations. The broker's elevated ref-protection token is bound to
  `githubRead.refProtectionCallers`, one source repository per token, never a gitops repository; docs/admin/security.md states the blast radius and recommends not setting it.

- Review settings count: a protected branch needs approvals AND `dismiss_stale_reviews_on_push` or `require_last_push_approval`. Chained stages verify the
  origin ref and sha the Trigger stamps on them (`glidepath.io/origin-*`; admission refuses those annotations from a `.tekton` file); a chained run with no
  origin is refused, so drain in-flight chains. `required` needs the platform's GitHub App to hold Administration: write (GitHub shows a ruleset's bypass
  list only to a token with write access to it) and `githubRead.refProtectionPermissions: {administration: write}` (broker, ref-protection call only);
  without that, set `off`. Upgrade order: glidepath-api, glidepath-app, catalog.

- glidepath-api 0.12.0: `GET /v1/apps/{app}/ref-protection?repo=&ref=&sha=` answers whether the git host protects a pushed ref (rulesets or classic
  branch protection requiring a reviewed pull request, blocking force pushes and deletion, with no push-rights bypass; tag rulesets
  restricting creation, update and deletion, and the tagged commit on a protected branch). Verdicts `protected`, `unprotected`,
  `unavailable` (the host's plan), `unverifiable`; a host failure is 502. Read per request, never cached. New env `PROTECTED_BRANCHES`,
  `TAG_REF_PATTERN` (from `cicdRunAdmission`); `glidepath-app` lets `pipeline-runner` `get` the `refprotection` resource.
- glidepath-catalog: new task `verify-ref-protection`, the first task of `release`, `deploy`, `test`, `gitops-image-bump` and `build`; every other task runs after
  it. It reads the run's Pipelines-as-Code annotations, not a parameter, and refuses the run unless the verdict is `protected`.
  **Upgrade: `refProtection.mode` defaults to `required`. A cluster on a host plan without rulesets (the lab's GitHub Free) must set
  `refProtection.mode: off` in its values before taking this release, or every push build and privileged run is refused.** `off` records
  "ref protection: Off here" in the task result and in the release record (`refProtection`). Release the control plane (glidepath-api) before the
  catalog: a required check with no endpoint to ask refuses. docs/admin/security.md, "Ref protection".

### Pipeline trust, slice T2: charts the repository chooses are rendered where there is no token

- New Task `render-manifests`, run as `source-runner` (no Role, no token, no Secret, no projected token), renders the environments of
  `policy-validation-check` and `clearance-gate-check` with the same `helm template` arguments as before (so the objects and Clearance's
  tree digest are unchanged), with no plugin or post-renderer, a CPU-time and file-size limit (`renderManifests` values) and #249's
  symlink refusal. `policy-validation-gate` and `clearance-gate` no longer run helm: each runs twice in its Pipeline (phase `fetch`, then
  phase `decide`) around the render, on one per-run volume, and read the output as data: files opened without following links,
  digests recomputed from a `MANIFEST`, a bounded YAML parse (no tags, no recursive anchor, depth, document and alias-expansion limits).
  Findings shown in a PR comment are escaped and cut.
- A token is never in a URL or an argument any more (`git_auth`: a header scoped to `gitHost`); `with_retry` prints a label; log excerpts posted to PRs, Slack and Backstage are redacted. Names from repository paths and chart sources are validated before use, and no repository string enters a `yq`/`jq` program. `values-check` renders token-less too. Workspace claims in a `.tekton` file are restricted.
- Admission: `render` is in `cicdRunAdmission.sourceTasks` for the three gate Pipelines. The two gitops-repo check templates gain a `gate` workspace
  and `taskRunSpecs` for `render`.
- **Upgrade:** release the three charts together, resync every gitops repository's `.tekton/` (until then its two checks fail, they do not
  pass), check the volume is writable by `source-runner`. A chart with remote dependencies must carry its `charts/` (as before).
  docs/admin/security.md, "Rendering is not done where the credentials are" and its upgrade note.

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
- `push-image` hardening: exact-key allowlists and duplicate-key detection for every JSON document it interprets (a `Layers` next to `layers` refuses), symlink checks on the whole path, whole-string name matching, a main tag that already exists is never overwritten (a rerun reuses the published image, reports its digest, and attests no build of it), `image-pinned-ref` (`repo@digest`) for every later task, and a main cache that fails validation is skipped with a warning instead of failing the task.
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
