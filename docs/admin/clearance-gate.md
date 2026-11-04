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
     chart and whether it is **protected** (`cicd.yaml` `deploy.environments[].production: true`); the repo's
     `clearance.yaml` and the commit that last changed it; the agent gates' verdict on who proposed the PR
     (`POST /v1/provenance`); the head's check runs; one-commit checkouts of base and head; and each protected
     environment's target cluster's **admission policy bundle** from the broker's `/policy-bundle` (the
     [policy-validation](policy-validation.md) source; an unreadable bundle is an `error` verdict below, not a failed
     check).
  2. `render` (the validator image): `helm template` of base and head per protected environment, with the value files
     and injected values the Flight ApplicationSet layers, then the object diff: each added, removed or changed object
     with the JSON pointers of the fields that changed and its new images, every object's digest and the **tree
     digest** (what a grant binds to). Nothing is sent to Clearance but this description: no manifest content. Then
     the Kyverno CLI's inputs for the head's tree (stand-in CRDs, Namespace labels, the normalized bundle).
  3. `admission` (toolbox): the **offline admission verdict** per protected environment, `kyverno apply` of the
     cluster's bundle to the rendered head: the policy-validation gate's own evaluation (the two tasks share
     `files/lib/policy-eval.sh` and `policy-eval-prepare.py`, so they cannot disagree). See "The policy in the record".
  4. `register` (toolbox): `POST /v1/clearances` per environment with a projected token for audience `clearance`
     (the same `CLEARANCE_VERIFIERS` rule as the agent gates). Clearance answers with the change's state.
  5. `report`: exits 1 unless every environment is **cleared**.
- An environment that is not protected is skipped (phase 1 gates protected environments only); a PR that changes
  none is `skipped` and passes.

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

## Required or advisory

`clearance-gate` is a `releaseGuardrails` entry, class `process` (like `itsm`: a decision about the change, not a
property of the image, so a rollback is judged like any release). It starts **advisory** (`required: false`): the
check runs and posts its verdict but does not hold releases, because a required check that a gitops repo has not
received yet hangs promotion ([release-guardrails.md](release-guardrails.md), the rollout trap). Flip it to required
once every gitops repo with a protected environment has the `.tekton` file. **Required, it replaces Tower's
`hangar/release-signoff`** (decision 2 of the gate design: the gate's tier-2 rule is Hand-flown's "one owner, not the
proposer", so one rule and one record); Tower stops posting the status then.

## Files

- `charts/glidepath-catalog/templates/tasks/clearance-gate.yaml`, `templates/pipelines/clearance-gate-check.yaml`
- `charts/glidepath-catalog/files/lib/policy-eval.sh`, `policy-eval-prepare.py`: the admission evaluation shared with
  `policy-validation-gate.yaml`; `tests/clearance_gate_admission_test.sh` (render, admission, register against the
  policy-validation fixtures with the real Kyverno CLI)
- `charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-clearance-gate.yaml`
- `charts/glidepath-catalog/values.yaml`: the `clearance-gate` guardrail; `clearanceGate.towerUrl`; Clearance's URL
  and audience are `agentGates.clearanceUrl` / `agentGates.audience`
- `comment-pr-check-result.yaml`: the gate's advice text
- Apron `55-autopilot/clearance/networkpolicy.yaml` admits pods labelled `hangar.io/clearance-client: clearance-gate`
- Clearance: `internal/gate` (the Go core), `docs/gate.md`; the API `GET /v1/clearances`, `/v1/clearance-policy`
