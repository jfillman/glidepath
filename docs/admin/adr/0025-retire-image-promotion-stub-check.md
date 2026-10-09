# ADR-0025: Retire the `image-promotion` stub check

*Status: Accepted (2026-10-09). Built: the check's onboarding template, Pipeline and `releaseGuardrails` entry
removed; `wait-for-release-guardrails` kept and made generic (`self-gate`); onboarding resync removes a
platform-generated gate file whose template is gone. Not built: image promotion itself
([known-gaps.md](../known-gaps.md) #42).*

## Context

`image-promotion` was the ninth release gate and the only one that depended on the others. The owner's
requirement (2026-08) was that it run last, so nothing could promote an image past a failing gate. Its Pipeline
had one real part, `wait-for-release-guardrails`: a pod polling the GitHub Checks API for the same commit until
every other gate in the registry reported, failing closed if any did not succeed. Then a stub `promote` step
(`governance-gate-stub`, ADR-0003: loud, never a silent pass) and a PR comment.

What the Tekton Results archive showed for the 97 runs on real release PRs (Release Record PRs excluded,
2026-09-17 to 2026-10-09):

| Outcome | Runs | Mean wall |
|---|---|---|
| Succeeded | 21 | 312 s |
| Failed because a sibling gate failed | 73 | 197 s |
| Ran to the full 18-minute budget | 3 | 1164 s |

Three facts made the check cost without return:

- **It could not block a merge.** GitHub Free does not enforce rulesets or branch protection on private
  repositories (the API answers `403 Upgrade to GitHub Pro`; [github-ruleset.md](../github-ruleset.md)). Every
  release PR in the archive was merged by hand, with no auto-merge. The only enforcement is
  `detect-bypass-merge`, which runs after the merge and alerts Slack.
- **Its result carried no information.** It was the AND of the sibling checks, which the PR's check list already
  shows. Each of the 73 failures added a second "image-promotion failed" comment under the sibling's own.
- **There was nothing to promote.** Every cluster, including kind-prod, pulls straight from `ghcr.io` with
  `ghcr-pull-secret`. No upper registry exists for a copy to land in.

Cost per release PR: one of nine gate PipelineRuns, a pod held for as long as the slowest sibling ran, two
GitHub API reads a minute, one extra PR comment, and a 3% chance of a 20-minute hang.

Two other homes were weighed. Keeping it as a PR check is cheap after the 30 s poll interval
(`jfillman/glidepath#83`) and keeps a visible "last gate" placeholder, but the placeholder states an ordering
nothing enforces. Moving the stub into the post-merge `bypass-merge-check` Pipeline removes the poller, but it
puts a future real promotion on the wrong side of the merge for Kubernetes targets: ArgoCD on the upper cluster
syncs the moment the PR merges, so a post-merge registry copy races the Rollout into `ImagePullBackOff`, and a
failed copy leaves a merged release with no image.

## Decision

1. **The `image-promotion` check is retired.** Its onboarding template, `image-promotion-check` Pipeline and
   `releaseGuardrails` entry are removed. The gate set says only true things: ADR-0003 asks that a stub never be
   reported with a real check's confidence; here no check is reported at all, and
   [release-guardrails.md](../release-guardrails.md) records that promotion does not exist.
2. **`wait-for-release-guardrails` stays in the catalog**, with a `self-gate` parameter instead of a hardcoded
   exclusion. It is the documented "shape B" piece for a gate that genuinely must run last.
3. **Where a real promotion lives is decided by registry topology when it is built:**
   - A separate upper registry that an ArgoCD-managed upper cluster pulls from: promotion must complete
     *before* merge. Build the fan-in on events, not polling. Every gate's `debrief` already emits
     `pipelinerun.finished` through the broker; a reducer on the release record
     ([ADR-0021](0021-rollout-facts-and-release-record.md)) marks each gate as it lands, and the last green one
     fires promotion. No pod waits, no 18-minute budget.
   - One registry for every cluster (today): "promotion" is a retag or an attestation, cheap and safe
     *after* merge in `bypass-merge-check`, once `detect-bypass-merge` reports no bypass.
   - Cloud targets ([ADR-0020](0020-cloud-gated-promotion-by-release-pin.md)) are unaffected by the choice:
     Glidepath's own deploy stage runs after the pin PR merges and can order promote before deploy itself.
4. **Onboarding resync prunes a retired gate.** `deliver-gitops-repo-files` removes a
   `.tekton/pull-request-*.yaml` whose template no longer exists *and* whose first line carries the platform's
   own marker (`# STUB`, `# BOILERPLATE`, or `# The real ... gate`). A tenant's own `.tekton` file has no marker
   and is left alone, which keeps the original "never delete a tenant's customization" intent.

## Consequences

- A release PR carries eight checks instead of nine (seven on a cloud pin PR), posts one fewer comment, and can
  no longer hang for 20 minutes on a check-run that never appears.
- `detect-bypass-merge` and `hack/apply-guardrail-ruleset.sh` render their gate lists from the registry and need
  no change. The audited ruleset export in `docs/admin/reference/` predates this ADR and still lists the check.
- The catalog Application does not prune, so the live `image-promotion-check` Pipeline must be deleted by hand
  after every gitops repo's resync PR has merged. Until then the stale `.tekton` file still resolves against it,
  which is the safe order.
- Tower's gate ledger reads the PR's check runs and shows whatever exists; only comments and a pipeline-name
  list referenced the check (`jfillman/tower`, cleanup PR).
- Reversal is cheap: the template, Pipeline and registry entry are three files in git history, and
  `wait-for-release-guardrails` is still there to call.
