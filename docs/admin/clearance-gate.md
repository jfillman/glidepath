# The deployment gate: `clearance-gate`

Clearance's second boundary (its `docs/gate.md`, decided 2026-10-31). The runtime boundary decides an agent's tool
calls; this one decides **changes**: may this release land in a protected environment, and can we prove afterwards
that it did so under policy? Glidepath's part is one check, `clearance-gate-`, on every release PR to a protected
environment. Clearance classifies and decides; the check describes the change and reports the answer.

| Tier | What the release changes (the baseline Clearance owns) | Cleared by |
|---|---|---|
| **0** | nothing a cluster runs: docs, tests, labels or annotations only | policy, at once |
| **1** | an image bump on a Flight-registry image, with CI green, provenance confirmed or not-agent, and the render clean (under 400 changed lines) | policy, at once; else it becomes tier 2 |
| **2** | runtime shape: replicas, resources, a Rollout's strategy, config, network, routes, probes, any unknown kind | **one** person from the app's owner group, not the proposer |
| **3** | Secrets and anything that reaches one (`secretKeyRef`, `envFrom`, service accounts), RBAC, AppProjects, jobs, admission policy, `clearance.yaml` itself | **two** people: one from the owner group, one from the outside group (`group:default/admins`), neither the proposer, never the same person twice |

An agent's PR is at least tier 1 (a person sees agent output land); a PR that looks like an agent's with no Clearance
record is denied outright, as the agent gates fail it. A repo's `clearance.yaml` (at the gitops repo's root) may only
**tighten** the baseline: add kinds, paths and field tokens to tiers 2 and 3, raise the people a tier needs, shorten
the 7-day TTL. Anything looser is `DG-POLICY` and the change is denied.

## How it runs

- **`.tekton/pull-request-clearance-gate.yaml`** in every gitops repo (delivered by onboarding, `targets: [k8s-rollout]`),
  on release branches only (`release-*`, not `release-record-*`), like every release gate.
- Pipeline **`clearance-gate-check`**, Task **`clearance-gate`** (`platform-catalog`), five steps:
  1. `fetch` (toolbox): the PR's base, head, author, changed files and line counts; the environments it changes
     (`<cluster>/<env>/values|release.yaml`, or a cluster's `base.yaml` = every environment on it); each environment's
     chart; whether it is registered (every one, on a cluster with a Clearance: [Clearance decides which are protected](#which-environments-are-protected))
     and what `cicd.yaml` declares about it (`deploy.environments[].production`); the repo's
     `clearance.yaml` and the commit that last changed it; the agent gates' verdict on who proposed the PR
     (`POST /v1/provenance`; `confirmed` is an agent's change, `not-agent` and `unattested` a person's, anything else
     `unknown` - `unattested` is AI-co-authored work outside Clearance, a warning there, and Clearance's gate counts it
     as `not-agent`, so the proposer kind is `person`; for a release PR the pipeline opened the
     source change's author and provenance replace this, [below](#who-proposed-a-release)); the head's check runs; one-commit checkouts of base and head; and each protected
     environment's target cluster's **admission policy bundle** from the broker's `/policy-bundle` (the
     [policy-validation](policy-validation.md) source; an unreadable bundle is an `error` verdict below, not a failed
     check).
  2. `render` (**the `render-manifests` task, as `source-runner`: no token, no Secret**; the gate publishes the checkouts and the plan to
     the run's volume first and reads the result back read-only as data, then verifies it: [security.md](security.md#rendering-is-not-done-where-the-credentials-are)):
     `helm template` of base and head per protected environment **exactly as Argo CD
     renders it** (and so as Clearance's Argo CD plugin does): release name `<app>-<env>` (the Application's), the
     environment's namespace, the value files and the injected values the Flight ApplicationSet layers, including
     the **destination cluster's** `observability` block (that cluster's own `cluster-defaults.yaml`, which its
     tenant-onboarding ApplicationSet reads: for the control-plane cluster the `glidepath-observability` ConfigMap
     glidepath-app writes, for any other cluster the broker's `/policy-bundle` answer, which carries only that file's
     `observability.namespace` and `instrumentation`; the app's runtime is the ConfigMap's). A cluster whose facts
     cannot be read fails that environment's render rather than register a tree Argo CD would not produce. The trees are then equal, so
     the clearance a PR earned is found by tree when Argo CD syncs it. Then the object diff: **every rendered object**
     is listed, the ones the PR does not touch with `op: unchanged` (never tiered; a grant needs the whole tree, and
     Clearance's token carries them with `changed: false`), each added, removed or changed object with the JSON
     pointers of the fields that changed and its new images, every object's digest and the **tree digest** (what a
     grant binds to). A Clearance that predates `unchanged` refuses it: the task then registers the changed objects
     only and says so. Nothing is sent to Clearance but this description: no manifest content. The gate's `ingest` step computes
     the digests from the bytes it read and verified (the same function as before the render moved: the digests are unchanged). Then
     the Kyverno CLI's inputs for the head's tree (stand-in CRDs, Namespace labels, the normalized bundle).
  3. `admission` (toolbox): the **offline admission verdict** per protected environment, `kyverno apply` of the
     cluster's bundle to the rendered head: the policy-validation gate's own evaluation (the two tasks share
     `files/lib/policy-eval.sh` and `policy-eval-prepare.py`, so they cannot disagree). See "The policy in the record".
  4. `register` (toolbox): `POST /v1/clearances` per environment with a projected token for audience `clearance`
     (the same `CLEARANCE_VERIFIERS` rule as the agent gates). Clearance answers with the change's state.
  5. `report`: exits 1 unless every environment is **cleared**.
- An environment that is not protected is skipped (phase 1 gates protected environments only); a PR that changes
  none is `skipped` and passes.

## Which environments are protected

The platform decides, never the app. An app team that leaves `production: true` off an environment does not turn the
gate off for it. Until 2026-11, the fetch step registered only the environments `cicd.yaml` marked
`production: true` and skipped the rest as "not a protected environment". gitops-flight-api#48, a release to
`hangar-prod/prod`, was skipped that way because flight-api's `cicd.yaml` lacked the line, although the platform gates
that namespace at sync.

Now (`files/lib/protection.sh`):

- **On a cluster with a Clearance** (`agentGates.clearanceUrl` set), every environment the PR changes is rendered and
  registered. Clearance's core decides protection for each change it is sent (Clearance C-6): an environment is protected
  when the core's `CLEARANCE_PROTECTED_ENVIRONMENTS` names it, or when a cluster's Argo CD agent reported the app and
  environment as protected. The core's list is rendered from the core cluster's `cluster.yaml`
  (`clearance.protectedEnvironments`), which no app team or pipeline account can write; an empty list protects every
  environment. A change anywhere else is cleared at tier 0 with the reason `unprotected`, and recorded. The core always
  overrides the request's `protected` (it refuses `false`), so the pipeline holds no protection decision to get wrong.
- **Unknown is protected.** Before Clearance answers, nothing is known, so nothing is skipped. When Clearance does not
  answer (any status but 200), the environment's row is an error and the check fails.
- **`production: true` is a declaration.** It is compared with Clearance's answer, and every disagreement is a
  **Warning** under the table in the PR comment and is counted in the check's headline:
  - Clearance protects the environment, and `cicd.yaml` says `production: false` or nothing: the change is gated anyway.
    Add the flag so the two agree.
  - `cicd.yaml` says `production: true` and Clearance does not protect it: cleared at tier 0. An app cannot protect an
    environment by itself; a platform admin adds it to `clearance.protectedEnvironments`. (A tighten-only way for a
    repository to protect one of its own environments would be a Clearance product feature; nothing in Clearance today
    takes one for gitops environments, only `clearance.yaml` `workspaces.protected` for Terraform.)
  - `production: true` keeps every other meaning it has in Glidepath (ADR-0024: an upper-cluster Flight environment).
- **On a cluster with no Clearance**, no platform answer exists and no sync-time gate can either, so the declaration
  still decides, as before: a `production: true` environment fails the check ("no Clearance"), and any other is skipped.

The core's list can be wider than a destination cluster's sync-time gate. hangar-dev's core protects `hangar-prod/*`,
while hangar-prod's Argo CD gate covers `hangar-prod/prod` for its `gate.apps`. The check follows the core, because the
core is what it registers with and what records the decision. A staging release on hangar-prod is therefore registered and
classified: an image bump clears at tier 1 by policy, and a runtime-shape change holds for the owners. To protect less,
narrow the core's `clearance.protectedEnvironments`, a platform setting, never `cicd.yaml`.

## Green, red, and holding

A PaC check is green or red, so **holding** is red: the comment names the tier, why, who may clear it (the
requirement), and links to Tower's Clearances page when `clearanceGate.towerUrl` is set. Someone the requirement names
clears (or denies, with a reason) in Tower; then `/retest clearance-gate-` re-registers the same head and finds it
cleared. A new push is a **new change**: Clearance supersedes the holding one, and people are asked again. A change
not cleared within the TTL expires.

Clearance records every step in its evidence chain, boundary `deployment`: `deploy.request` (this check, the tier
and why), `deploy.clear` (policy, or the person and their role), `deploy.deny`, `deploy.expire`. Tower and the
Clearance plugin read them (`GET /v1/clearances`).

## The policy in the record (C-8)

Each change is registered with the policy that decided it, and Clearance writes it into every `deploy.*` record's
`policy` group (Clearance `docs/gate.md`, "The policy in the record"):

```json
"policy": {"ref": "clearance.yaml@<commit>", "version": "baseline-2026-10-31+sha256:<clearance.yaml digest>",
           "engine": "kyverno/1.19.0", "verdict": "pass",
           "bundle": "https://github.com/<owner>/gitops-<cluster>.git//30-policy@<commit>"}
```

What the check sends in `POST /v1/clearances`, beside `policy` (the `clearance.yaml` as JSON):

| Field | Value |
|---|---|
| `policyCommit` | the commit that last changed `clearance.yaml` at the PR head (`GET /repos/{repo}/commits?path=clearance.yaml&sha=<head>`); sent only with `policy`. A new `clearance.yaml` commit is a new `ref` |
| `admission.engine` | `kyverno/<version>`, from `kyverno version` in the toolbox (1.19.0 today). A CLI that does not say its version: no `admission` is sent (the record says `none`, `not-evaluated`) |
| `admission.verdict` | `pass`; `warn` (Audit/Warn findings, or a rule offline evaluation could not decide); `fail` (a Deny/Enforce policy fails: the cluster would refuse the sync); `skipped` (the cluster has no admission policy in its bundle); `error` (the bundle could not be read, or Kyverno could not run) |
| `admission.bundle` | the rule set: `<repoURL>//<path>@<commit>` from the broker's answer |
| `admission.deny`, `admission.warn` | the blocking and warning finding counts |
| `checks.admission` | the verdict (it was the `policy-validation-` check run's state before C-8) |

The verdict is **recorded, not enforced** by this check: a `fail` still registers the change (tiers do not read it),
and the `policy-validation-` check is the one that goes red on it. The PR comment gains an **Admission** column.

**Rollout order.** A Clearance before C-8 refuses the two new fields (strict decoding, `unknown field 'admission'`);
the register step then registers the change again without them, says so in the log, and the record reads
`not-evaluated`. Deploy the Clearance release first for the verdict to be recorded.

**Argo CD's config management plugin (G-ARGO, not built).** The CMP registers the tree it rendered through the same
route, so it sends the same `admission` object (`engine`, `verdict`, `bundle`, `deny`, `warn`) from its own offline
evaluation of that tree against the same bundle, and `policyCommit` when the gitops repo has a `clearance.yaml`.

## Who proposed a release

A release PR is written by the pipeline's GitHub App, so its author (`<app>[bot]`) and its single commit say nothing
about the change it releases. Registered naively, the proposer was the bot (kind `person`, provenance `not-agent`),
which also let the real author clear their own tier-2 change, because the rule is "one owner, not the proposer". The
gate now registers **the source change's** author and provenance instead
(`charts/glidepath-catalog/files/lib/release-source.sh`, run in the `fetch` step, which gets it from the chart in its `RELEASE_SOURCE_LIB` environment variable and evals it, never from the repository; it stays out of the step script because Tekton passes all of a Task's scripts to the pod as one argument capped at 131072 bytes, and `tests/task_script_size_test.py` fails CI when any Task's encoded scripts pass 120000; tested by
`tests/clearance_gate_release_source_test.sh`).

> **Caveat: the release record is not yet authenticated.** The record is a ConfigMap in the app's `-cicd` namespace, and
> `pipeline-runner` (the ServiceAccount every PipelineRun there uses, token mounted) may create, patch and delete
> ConfigMaps namespace-wide. Tasks that run code from the app repo (`build-source`, `run-tests`, the app's own `build.sh`
> / `test.sh`, dependency install scripts) run as it, so that code can forge or delete a release record. The source
> resolution below is only as trustworthy as that code. A separate hardening change will follow (source-executing tasks
> under a ServiceAccount with no ConfigMap rights or no token, and an admission policy that lets only `open-release-pr`
> and `mark-release-merged` write `release-tracking-*`).

**What it uses, and why.** Things the pipeline wrote and GitHub's own account facts:

| Trusted | Why |
|---|---|
| The **release record** (`release-tracking-<chainId>`, the one whose `prUrl` is this PR; two records for one PR are trusted as neither): image, `imageRepository`, `imageSource` and `imageDigest` (the last two only when the release pins a digest), `gitUrl` and `gitRevision` (the source commit), `kind`, `initiator` | written by `open-release-pr`; a release PR's author cannot write it, but code run by the app's own build and test tasks can (see the caveat above) |
| The **previous release's record** (same app, environment and cluster, merged, whose image is what the base manifest runs) | same |
| The app repo's **merged pull requests** (which PR contains each commit, who opened it, who merged it), read with the GitHub App's token. A PR covers a commit only if it was **merged into the released branch of the source repository itself** (the source repo's default branch, from GitHub: the pipeline records no branch, and the PR's own claim is never used) and the commit is that PR's `merge_commit_sha` or one of its commits | authenticated GitHub account facts. A **commit's author or committer is never used**: GitHub derives a commit's author login from the author email, which whoever pushes the commit chooses |
| **Clearance's provenance verdict** per source PR (`POST /v1/provenance`) | Clearance's own receipts decide `confirmed`; nothing here reimplements it |

Not trusted for anything: the release PR's author, title, body, branch name or commit message, and any text in it. The
PR is accepted as the pipeline's only if its **content is exactly what the record says the pipeline wrote**: one commit;
one file, `<cluster>/<env>/release.yaml` (or `values.yaml`) of the record's cluster and environment; nothing different
from base in that file except the image and release-tracking fields, and each of those equal to the record's: `release.image`
`repository`, `tag`, `source` and `digest` (**no digest in the record means the PR must carry none**: unresolved otherwise), no
`rollout.image`, `releaseTracking` `releaseId`, `cluster` and `appNamespace`; and the tag's `-<sha>` suffix, if it has one,
starts the record's source commit. A record written before the gate kept `imageRepository` cannot vouch for a PR: unresolved. A PR pre-opened on the release branch name, a second commit pushed to the branch, an
extra file or a changed field fails this: the source is **unresolved**, never borrowed.

**What is registered** (only fields Clearance accepts today):

| | |
|---|---|
| `proposer` | one **human**: bot accounts (a login ending `[bot]`, an account of API type `Bot`, or one in `clearanceGate.pipelineLogins`) are never chosen; among human openers, the opener of the source PR with the **most changed lines** (additions plus deletions; ties: the newest merged); if every opener is a bot, the human merger of the newest source PR; with no human at all the kind is `unknown` |
| `proposerKind`, `agent`, `checks.provenance` | from the **weakest** source PR (below) |
| `checks.sourceAuthors`, `checks.sourceMergers` | every PR opener and every PR merger, comma-separated: Clearance's request has one proposer, so the rest ride in `checks`, which Clearance stores with the change |
| `checks.sourceRange` | `<repo>@<previous>..<released> (<mode>, <n> commits)` |
| the gate's comment | the authors with commit counts, each source PR with its verdict, the promoter |

**Weakest verdict wins**, in this order, weakest first: `unknown` (a commit with no merged PR, a forged-trailer or
modified agent PR, Clearance not answering) < `unattested` (an AI co-author trailer Clearance has no record of) <
`not-agent` (no AI signal) < `confirmed` (an agent opened it through Clearance). `confirmed` is claimed only when
**every** source PR is confirmed; any `unattested` PR makes the release `unattested` (it contains agent work outside
Clearance, so `not-agent` would understate it); a person's PR beside a confirmed one makes it `not-agent`. `unknown`
becomes `proposerKind: unknown`, which Clearance **denies** (`DG-PROVENANCE`); `unattested` and `not-agent` are `person`.

| Case | Behaviour |
|---|---|
| Normal release | previous release's commit `..` this release's commit in the app repo. **Every** commit, merge commits included, must be the merge commit of, or a commit in, a merged PR (paginated; 300 or more associated PRs is not trusted); those PRs are the sources |
| Several openers or mergers | none dropped: all in `checks.sourceAuthors` / `checks.sourceMergers` and the comment. **Until Clearance excludes them, only the proposer is kept from clearing the change**: another opener or a merger of a source PR can still clear it as owner |
| Merge methods | merge commit: the merge commit is the PR's `merge_commit_sha`, its commits are in the PR. Squash: the one commit on the branch is `merge_commit_sha`. **Rebase merge**: GitHub documents `merge_commit_sha` as the resulting tip commit only; the earlier rebased commits have new shas that cannot be tied to the PR, so they count as no PR (fail closed: a multi-commit rebase-merged PR makes the release `unknown`) |
| First release to an environment | the single released commit, resolved through its merged PR like any other (the earlier history was never released here) |
| A commit no merged PR contains (a direct push, or a merge commit made outside a PR) | provenance `unknown` (Clearance denies); the comment names each such commit in full. It stays in the range of **every later release** (previous release `..` next), so that environment cannot advance until an owner decides how to baseline it: this is deliberately not designed here. A login Clearance cannot record as a principal is unresolved, never altered |
| Source cannot be resolved (record swept, first release to the environment, over 250 commits, an author with no GitHub account, GitHub unreadable) | proposer `user:<PR author>`, kind **`unknown`** (never `person`), provenance `unknown`; `checks.sourceUnresolved` and the comment say why. Clearance denies it: fix the cause and `/retest`, or release again |
| Rollback (`kind: rollback`, or the image is older than the one the environment runs) | the restored commit alone: its author and its PR's provenance (what is restored was released, and cleared, before) |
| Same commit released again | that commit alone |
| PR not opened by the pipeline (no record names it, e.g. a person's hand-made release PR) | unchanged: the PR's author and the agent gates' verdict |
| PR authored by the pipeline's App (`clearanceGate.pipelineLogins`, a setting; empty = off) but no record names it | not a person's change: kind `unknown`, with the reason |
| Release records unreadable | the check fails: it cannot tell which of the above this is |

**The guarantee, exactly:** an owner other than the chosen proposer must clear a tier-2 or tier-3 change. Other source authors and mergers are recorded (`checks.sourceAuthors`, `checks.sourceMergers`) and will be excluded once Clearance supports a set; today they are not, and nothing more than that is claimed.

**Upgrade.** A release PR that was opened before this version has a record without `imageRepository` (and `imageSource`), so its source is unresolved and Clearance denies it. Re-running `open-release-pr` on the same chain rewrites the record in place (it re-applies every key, keeping `state`), but a re-triggered release run mints a *new* chain, which leaves a second record for the same PR, and two records for one PR are trusted as neither. So the operator closes the stale release PR and releases again (a closed PR is not reused: a fresh PR and a fresh record), then `/retest clearance-gate-`. Not verified live.

**`clearanceGate.pipelineLogins`** is the pipeline's GitHub App as GitHub's REST API names it in `user.login`, e.g. `name[bot]` (not the App's display name, and not the GraphQL form without `[bot]`). It is empty by default; Hangar sets it. While it is empty and a PR's author account is a Bot with no record naming the PR, the gate prints a warning and treats the PR as a person's.

**The promoter** (who triggered the promotion; the bot only opens the PR for them) is Tower's `initiator`, which
`open-release-pr` now keeps on the release record (`initiator`, with `imageDigest`). It is empty for a release a flow
started rather than a person. Clearance has no field for it, so the gate names it in its comment and does not send it.
Clearance would need a `promoter` on the change request, stored with the change like `proposer`, and the "not the
proposer" rule (`DG-PROPOSER`, tier 2 and 3 approvers) refusing the proposer, **every** source author and the promoter,
with `sourceAuthors` and `sourceMergers` as real lists.

## Required or advisory

`clearance-gate` is a `releaseGuardrails` entry, class `process` (like `itsm`: a decision about the change, not a
property of the image, so a rollback is judged like any release). It starts **advisory** (`required: false`): the
check runs and posts its verdict but does not hold releases, because a required check that a gitops repo has not
received yet hangs promotion ([release-guardrails.md](release-guardrails.md), the rollout trap). **Required, it
replaces Tower's `hangar/release-signoff`** (decision 2 of the gate design: the gate's tier-2 rule is Hand-flown's
"one owner, not the proposer", so one rule and one record).

Nothing turns it on by itself. The flip is one catalog setting, made by an administrator once the order below is met:

```yaml
# the catalog Application's values for the cluster (the per-cluster override map, release-guardrails.md)
releaseGuardrailOverrides:
  clearance-gate: { required: true }
```

**Rollout order (G-ARGO-8).** Each step must hold before the next:

1. **Clearance is deployed with the deployment gate** (C-8, and the release the task's `register` step needs), and
   `agentGates.clearanceUrl` is set for the control-plane cluster. Required with no Clearance URL, a protected
   environment's check fails ("no Clearance to register the change with").
2. **Every gitops repo carries `.tekton/pull-request-clearance-gate.yaml`.** Onboarding delivers it; existing repos get
   it by the onboarding resync. Check the list of repos without the file is empty before going on: a required check
   that never starts leaves the PR waiting forever.
3. **Flip `required: true`** (above) and let the catalog sync. The advisory pair (`register-advisory` and
   `render-check`) becomes the single hard `register` task, and the fan-in (`wait-for-release-guardrails`),
   `promote`'s PROM-03, the bypass detectors and the release log's required set start naming `clearance-gate`, all
   from the registry.
4. **Apply the ruleset** so GitHub requires the check: `hack/apply-guardrail-ruleset.sh -f <the same values file>`.
   The context it requires is `<pipelinesAsCode.applicationName> / clearance-gate-`, and the promotion wait also
   accepts `legacyApplicationNames`; no script spells the prefix.
5. **Retire Tower's sign-off status** (after 3 and 4, so there is never a window with neither gate): set
   `tower.releaseSignoff.postStatus: false` in Backstage's app-config. Tower then stops posting
   `hangar/release-signoff` and no longer clears one; the "shipped without a sign-off" announcement stays. Until
   then both run, and the status is harmless.
6. `agent-gates-` retirement is **not** part of this: it is C-7 (`agentGates.clearanceChecks`, [agent-gates.md](agent-gates.md#migration)).

Rollback of the flip is the same setting back to `false` (and re-applying the ruleset); a held change then no longer
blocks, it still reports.

What a required check does:

- **Held:** the check is red until the change is cleared in Tower, then `/retest clearance-gate-` re-registers the
  same head and finds it cleared (tested: nothing about the required copy changes this).
- **A release to an environment Clearance does not protect** is registered and cleared at tier 0 (`unprotected`), so
  requiring the check does not stop it. Only a cluster with no Clearance still skips an environment that is not
  `production: true` (`skipped`). Tests: `clearance_gate_protection_test.sh`, `clearance_gate_parity_test.sh`.
- **Not a release PR on a `release-*` branch** (a replicas change, say): the check is unaffected, but the other
  release gates run on it and fail for want of a changed image. That is a separate known gap, not changed here.

**Parity with Clearance's Argo CD plugin.** The task renders the tree the plugin renders, so the clearance a PR earned
is found by tree at sync. `tests/clearance_gate_parity_test.sh` (catalog CI) holds it against the plugin's recorded
fixture. The check registers a change on the PR head; the plugin finds the cleared change by tree digest and registers
only when none exists, so if the two ever disagree the plugin's tree is the one that gets enforced and a PR's
clearance simply is not found (the sync is refused, not waved through). A live prod release on 2026-11-09 matched
(`765d2bfc6461`). Making the check read-only when the trees differ is not built: it needs a Clearance read-by-tree
the check can use before merge, and has no case yet.

**A release that cannot be rendered is never green, advisory or not.** The advisory copy ignores its task's failure
(`onError: continue`), so a change that is holding or denied stays green by design. A gate that could not render the
release has seen nothing. The Pipeline is `fetch`, `render`, then `register` (three tasks on one volume; `fetch` and `render`
have no `onError` in either copy, so a release that cannot be fetched or rendered is red). The advisory copy adds `render-check`
(the gate Task with `render-only: "true"`: `ingest` only, no register) with no `onError`, after `register-advisory`: when the render's
output is refused (a changed file, a tag, an alias bomb, an environment that did not render) that task fails and so does the check.
When every environment is read it stops there. The required copy has no second task; its `register` task already fails on a refusal.

## Files

- `charts/glidepath-catalog/templates/tasks/clearance-gate.yaml`, `templates/pipelines/clearance-gate-check.yaml`
- `charts/glidepath-catalog/files/lib/policy-eval.sh`, `policy-eval-prepare.py`: the admission evaluation shared with
  `policy-validation-gate.yaml`; `tests/clearance_gate_admission_test.sh` (render, admission, register against the
  policy-validation fixtures with the real Kyverno CLI); `tests/clearance_gate_parity_test.sh` (the render equals
  Clearance's Argo CD plugin's, against the plugin's recorded gate-api prod fixture in `tests/fixtures/clearance-parity`)
- `charts/glidepath-catalog/files/lib/protection.sh`: which environments are registered and the warnings when cicd.yaml
  and Clearance disagree ([above](#which-environments-are-protected)); `tests/clearance_gate_protection_test.sh` (the
  decision matrix, the fetch step's plan loop on gitops-flight-api#48's cicd.yaml, and #48 end to end)
- `charts/glidepath-app/templates/identity/observability-values.yaml`: the `glidepath-observability` ConfigMap the render reads (cluster, runtime, this cluster's block); `glidepath/broker/cmd/token-review-interceptor/policybundle.go` returns each cluster's own `observability`
- `charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-clearance-gate.yaml`
- `charts/glidepath-catalog/values.yaml`: the `clearance-gate` guardrail; `clearanceGate.towerUrl`; Clearance's URL
  and audience are `agentGates.clearanceUrl` / `agentGates.audience`
- `comment-pr-check-result.yaml`: the gate's advice text
- Apron `55-clearance/clearance` (its networkPolicy values) admits pods labelled `hangar.io/clearance-client: clearance-gate`
- Clearance: `internal/gate` (the Go core), `docs/gate.md`; the API `GET /v1/clearances`, `/v1/clearance-policy`

## The join to the release record (G-ARGO-6)

After each `POST /v1/clearances` answers, the register step writes the change's `id` and `tree_digest` into the
release's own record (`release-tracking-<chainId>`, found by the PR URL, cluster and environment) as `clearanceId` and
`clearanceTree`. It is best effort: a PR with no record (hand-made, or swept) is not a failed check. glidepath-api
returns them as `clearance: {id, tree}` on `GET /v1/apps/{app}/releases/{chainId}`, and `release-log-emit` adds
`clearance_id` and `clearance_tree` to the release log, so Tower and the release packet join a release to its change,
outcome and attestation by id. Test: `charts/glidepath-catalog/tests/clearance_gate_release_join_test.sh`.
