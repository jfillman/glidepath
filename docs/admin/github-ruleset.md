# The GitHub ruleset that enforces the guardrails

**This is a mandatory part of a Glidepath install.** The gates (`sast`, `image-scan`, `provenance`, `sbom`, `values`,
`itsm`, `qa`, `policy-validation`) only report; the ruleset's required-status-checks list is what stops a merge.
Nothing is blocked without it. (Until 2026-10-09 an `image-promotion` check also waited on its sibling gates; it was
retired as a stub, ADR-0025, and the audited export below predates that and still lists it.)

Reference: [`reference/glidepath-guardrail-checks.ruleset.json`](reference/glidepath-guardrail-checks.ruleset.json), an export of
the ruleset from `gitops-flight-api` (audited 2026-09-27). Generate and apply it with
[`hack/apply-guardrail-ruleset.sh`](../../hack/apply-guardrail-ruleset.sh), which builds the check list from
`releaseGuardrails` so the two cannot drift.

## What it is

One **repository** ruleset per gitops repo (`gitops-<app>`), named `Glidepath Guardrail Checks`, target `branch`, on the
default branch. GitHub has no account-wide ruleset on a personal account, so it is applied per repo (an organisation could
use one ruleset for `gitops-*`).

| Setting | Reference value | Why |
|---|---|---|
| Required status checks | one per `releaseGuardrails` entry, named `Pipelines as Code CI / <gate>-` | PaC names the check after the PipelineRun `generateName`, which is `<gate>-` (see `wait-for-release-guardrails`) |
| Check's `integration_id` | `4461953` (the Pipelines as Code GitHub App) | only that app's check counts; a look-alike status from anything else is ignored |
| Require a pull request | yes; **2 approvals normally**, 0 in the lab | the human gate. `require_last_push_approval` only matters when approvals > 0 |
| `strict_required_status_checks_policy` | `false` | the gates run on the PR head commit; the branch need not be up to date |
| `do_not_enforce_on_create` | `true` | a brand-new branch is not blocked before its first checks |
| Restrict deletion, block force pushes | yes | the release history is the audit trail |
| Bypass actors | none | see "Problems found" |

The detective control that complements it: `pull-request-merged.yaml` (check `bypass-check-`) runs when a PR closes and
flags a merge that did not have every required check green. It is the only control on a plan where the ruleset cannot be
enforced.

## Plan requirement

Rulesets (and branch protection) on **private** repositories need a paid GitHub plan; on the free plan the API answers
`403 Upgrade to GitHub Pro or make this repository public`, and a ruleset created in the UI is not in effect. The lab in
which this was built is on the free plan: the ruleset exists as the reference architecture and is not enforced.

## Problems found in the audit (2026-09-27)

These are invisible while the ruleset is not enforced and would surface the day it is.

1. **The required checks assume every PR is a release PR.** Every gate's `on-cel-expression` is
   `head.ref.startsWith("release-")`, so on any other PR (an onboarding-resync PR, a Tower Config PR, a hand edit, a
   seed PR) eight of the nine required checks are never created, and a required check that never reports blocks the
   merge forever. `values` runs on any PR that touches a `values.yaml`, so it is missing from a `.tekton`-only or
   `configmap/`-only PR too. Fix options: run every gate on every PR to `main` and have it pass immediately when the
   PR is not a release (one small pod per gate), or accept that non-release PRs need an admin bypass. A ruleset cannot
   condition on the head branch, so it cannot be solved in the ruleset alone.
2. **`update` and `creation` rules with no bypass actors.** "Restrict updates" stops every update to the default branch
   that is not from a bypass actor, and pull-request merges are updates; it makes the pull-request rule pointless and
   likely blocks merging entirely. The generated ruleset omits both. (If your export includes them, remove them and
   re-test; this needs confirming on a plan where the ruleset is enforced.)
3. **Bots write straight to the default branch.** The `ApplicationEnvironment` composition commits the bootstrap
   `values.yaml` (and, once built, `configmap/kustomization.yaml`) to `gitops-<app>` through provider-github, not through a
   PR, and the ruleset's pull-request rule would refuse it. The GitHub App or token those writes use must be a bypass
   actor (`--bypass-app-id`), scoped as narrowly as GitHub allows.
4. **App repos are not covered, on purpose.** Ground environments are edited by direct commits to the app repo's
   `main` (the self-service tier), so nothing enforces a check on them. Only the gitops repos are gated.
5. **`require_extra_approval_for_unattributed_changes`** appears in the export; it is a newer GitHub option and is not
   emitted by the generator (it defaults sensibly).

## Applying it

```bash
hack/apply-guardrail-ruleset.sh --dry-run --approvals 0 jfillman/gitops-flight-api     # inspect
hack/apply-guardrail-ruleset.sh --approvals 2 --bypass-app-id <crossplane-app-id> jfillman/gitops-<app> ...
```

Re-run it whenever `releaseGuardrails` changes: it updates the ruleset by name. Adding a gate to the registry without
re-running it leaves the gate reporting but not required; removing one without re-running leaves every release PR
unmergeable.
