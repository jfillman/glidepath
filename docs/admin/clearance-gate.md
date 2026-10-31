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
- Pipeline **`clearance-gate-check`**, Task **`clearance-gate`** (`platform-catalog`), four steps:
  1. `fetch` (toolbox): the PR's base, head, author, changed files and line counts; the environments it changes
     (`<cluster>/<env>/values|release.yaml`, or a cluster's `base.yaml` = every environment on it); each environment's
     chart and whether it is **protected** (`cicd.yaml` `deploy.environments[].production: true`); the repo's
     `clearance.yaml`; the agent gates' verdict on who proposed the PR (`POST /v1/provenance`); the head's check runs;
     one-commit checkouts of base and head.
  2. `render` (the validator image): `helm template` of base and head per protected environment, with the value files
     and injected values the Flight ApplicationSet layers, then the object diff: each added, removed or changed object
     with the JSON pointers of the fields that changed and its new images, every object's digest and the **tree
     digest** (what a grant binds to). Nothing is sent to Clearance but this description: no manifest content.
  3. `register` (toolbox): `POST /v1/clearances` per environment with a projected token for audience `clearance`
     (the same `CLEARANCE_VERIFIERS` rule as the agent gates). Clearance answers with the change's state.
  4. `report`: exits 1 unless every environment is **cleared**.
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
- `charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-clearance-gate.yaml`
- `charts/glidepath-catalog/values.yaml`: the `clearance-gate` guardrail; `clearanceGate.towerUrl`; Clearance's URL
  and audience are `agentGates.clearanceUrl` / `agentGates.audience`
- `comment-pr-check-result.yaml`: the gate's advice text
- Apron `55-autopilot/clearance/networkpolicy.yaml` admits pods labelled `hangar.io/clearance-client: clearance-gate`
- Clearance: `src/clearance/gate.py`, `docs/gate.md`; the API `GET /v1/clearances`, `/v1/clearance-policy`
