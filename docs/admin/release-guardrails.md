# Release guardrails: adding and removing a gate

The release stage's governance checks (docs/release.md) are the platform's release
**guardrails** - independent, required GitHub Checks on every gitops-repo release PR.
This doc covers the mechanism that makes a gate a one-file (or two-file) addition rather
than a change scattered across half a dozen places, and the checklist for actually doing
that. See [governance-stubs.md](governance-stubs.md) for how a stub stays honestly a
stub, and [ADR-0003](adr/0003-governance-stubs.md) for why stubs are structurally loud
rather than a silent `exit 0` - this doc is the "how to add one" companion to that
decision, not a new one.

## The single source of truth

`charts/glidepath-catalog/values.yaml`'s `releaseGuardrails` list is the canonical
registry of every gate enforced on a release PR - `name`, a human `description`, and
`status` (`real` or `stub`, cosmetic only - see below). Two places render off it instead
of hardcoding the gate list:

- `catalog/tasks/detect-bypass-merge.yaml` - the break-glass Slack alert's "did every
  required check actually pass" loop.
- `catalog/tasks/open-release-pr.yaml` - the release PR body's governance-checks table.

Everything else about a gate - which Pipeline it runs, what it actually checks, whether
it posts a PR comment - is standard Tekton/PaC config, not driven by this list. Adding a
name to `releaseGuardrails` **alone does nothing**; the gate exists once its own
`.tekton/pull-request-<name>.yaml` onboarding template and Pipeline exist too (below).
Branch protection (which checks are actually *required* to merge) is a separate, manual
GitHub setting - see "Branch protection" below.

## Two shapes, depending on what the gate needs

**A. Independent stub or real check (the common case)** - most gates (sast, image-scan,
provenance, sbom, itsm, qa, policy-validation) run in parallel off the same PR
webhook, with no dependency on each other's outcome:

- **Stub, fully generic (least effort)**: add one onboarding template file,
  `charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-<name>.yaml`,
  pointing at the existing shared `governance-check` Pipeline with `gate-name: <name>`
  (copy `pull-request-itsm.yaml` or `pull-request-policy-validation.yaml` as a starting
  point). That Pipeline already calls `governance-gate-stub` (loud "no real check
  implemented yet" logging + a `governance.stub=true` span + a `"stub"` result, never a
  silent pass) and posts a PR comment. **No new Pipeline or Task needed** - this is the
  whole change, plus the registry entry above. Good default when the gate's eventual
  real shape isn't known yet.
- **Stub, dedicated (scaffolded for graduation)**: if you already know roughly what the
  gate will eventually need to check, give it its own Pipeline+Task *now*, still calling
  `governance-stub` under the hood - see `qa-check.yaml`/`qa-gate.yaml` for the pattern.
  Same honest stub behavior and `status: stub` as the fully-generic case (this is NOT the
  "Real" case below - no stub marking gets removed), but graduating later is then a
  single-file swap of the Task's own step body, instead of writing a new Pipeline+Task
  and repointing the onboarding template all at once. Worth the extra two files when you
  already know the gate's real params (e.g. `qa-gate.yaml` already threads through
  `repo-url`/`revision`, unused by the stub step, ready for whatever real check ends up
  needing them).
- **Real**: once real logic exists, give the gate its own dedicated Pipeline if it
  doesn't already have one (see `sbom-check.yaml` for the shape: one or more real Tasks,
  a `finally: comment-pr` block identical to every other gate's), and repoint that
  gate's onboarding template file at it instead of `governance-check`. Flip its
  `releaseGuardrails` entry to `status: real`. Nothing about the onboarding template
  file's *shape* changes, only the `pipelineRef` name and dropping `gate-name` (see
  `pull-request-sbom.yaml` vs. this doc's stub example for the before/after). If the
  gate was already scaffolded dedicated-stub-style, this step shrinks further: just
  replace the Task's stub step with real logic, keeping its name/params/results
  contract the same - no Pipeline or onboarding-template change at all.

**B. A gate that depends on the others (none today)** - Tekton has no cross-PipelineRun
`runAfter` (every gate is its own independently PaC-triggered PipelineRun off the same
webhook, run in parallel - see `governance-check.yaml`'s own header), so a gate that must
run *after every other gate succeeds* needs its own dedicated Pipeline that waits for the
rest. `catalog/tasks/wait-for-release-guardrails.yaml` does that by polling the GitHub
Checks API (Helm-rendered off `releaseGuardrails`, minus the gate named in `self-gate`).
`image-promotion` was the one such gate until 2026-10-09, when it was retired as a stub
check ([ADR-0025](adr/0025-retire-image-promotion-stub-check.md): it could not block a
merge on this GitHub plan, its result was the AND of its siblings, and there is no upper
registry to promote into). That ADR also records how a real promotion should be ordered:
event-driven fan-in on the release record rather than a polling pod.

## Timeouts: always split `tasks`/`finally`, never a single `pipeline` budget alone

**Found live 2026-08-23**: every onboarding template originally set only
`timeouts.pipeline` (e.g. `10m`) - a single shared budget for the real check tasks *and*
the `finally: comment-pr` step that reports the outcome. A check that ran merely slow
(not wrong) could burn the whole budget, leaving `comment-pr` no time to even start -
confirmed live: its pod sat `Pending`/`"containers with incomplete status"` for minutes,
then got `TaskRunCancelled` the instant the PipelineRun's own timeout hit, so the PR
never got a comment even though the underlying check itself had already produced a real
(if slow, or in one case never-finished) result.

Every onboarding template now sets `timeouts.tasks` and `timeouts.finally` explicitly
(not just `pipeline`) so `finally`/`comment-pr` always gets its own reserved window,
regardless of how long the real check tasks take:

```yaml
timeouts:
  tasks: "8m"      # budget for the real check task(s)
  finally: "2m"    # reserved, separate budget for comment-pr - never starved
  pipeline: "10m"  # must be >= tasks + finally
```

A new gate's onboarding template must set all three, not just `pipeline` - copy the
pattern from `pull-request-sast.yaml` (the default 8m/2m/10m split) or, if the real
check genuinely needs more room (see `pull-request-image-scan.yaml`'s 13m/2m/15m for
why Trivy scans got more), size `tasks` to what the check actually needs and keep
`finally` at 2m. A shape-B gate (one that waits on its siblings) is the exception: its
`tasks` must out-wait `wait-for-release-guardrails`' own timeout, which in turn must
out-wait the slowest sibling's PipelineRun timeout. `image-promotion` carried those
larger numbers until ADR-0025 retired it.

## Current gates

| Gate | What it verifies | Status |
|---|---|---|
| `sast` | Static analysis (Semgrep) of the promoted commit's source | real |
| `image-scan` | Vulnerability scan (Trivy) of the promoted image | real |
| `provenance` | Commit signature (gitsign) + SLSA provenance (Conforma) | real |
| `sbom` | Software bill of materials | real |
| `values` | Values-file validation by the validator image (Airframe's by default); check `Pipelines as Code CI / values-` | real |
| `itsm` | ServiceNow Change Request | stub |
| `qa` | Test-completion verification | stub |
| `policy-validation` | Gatekeeper/Kyverno admission-policy validation of the rendered manifest | stub |

**How the image gates find the image (2026-10-09).** `sast`, `image-scan`, `sbom` and the provenance half of
`provenance` share the `resolve-promoted-image` StepAction: two GitHub API reads against the PR (its base, the
files changed between that base and the head commit, then the one manifest's content) - no clone of the gitops
repo. The manifest is the first changed `*/release.yaml`, else `*/values.yaml`, else `glidepath/releases/*.yaml`;
a PR that changes none of them is not a promotion and the gate says so. `verify-commit-signature` reads the PR's
commits the same way for the `X-App-Repo`/`X-App-Commit` trailers, and `validate-values` fetches the PR head one
commit deep and takes its changed files from the same compare. Every gate pipeline passes `pr-number` for this;
without it (a manual run) each falls back to the revision's own commit. Before this every gate full-cloned the
gitops repo to diff one commit, and a release branch with more than one commit broke the three image gates.

**Release Record PRs are not releases.** Tower's Release Record poller opens PRs on the gitops repo from
`release-record-<app>-<hash>` branches that only add `releases/<app>@<version>.yaml`. Every gate template's
trigger excludes them (`!head.ref.startsWith("release-record-")`); before 2026-10-09 they matched
`startsWith("release-")` and the image gates failed on each one - 135 of 284 historical `extract-promoted-image`
runs.

`provenance` and `policy-validation` are deliberately separate gates, not one - the
former is about the *commit*'s signer identity and the *image*'s provenance attestation
(docs/commit-signing.md, docs/provenance-policy.md), the latter is about the *rendered
manifest*'s compliance with cluster admission policy. Different failure modes, different
fixes, kept as independent required checks rather than folded together.

## The values gate (`values`, registered 2026-09-27)

`values-validation` runs a **validator image** against every `values.yaml` a gitops PR changes (a release PR, or a
human's or an agent's hand edit) and fails the check on a non-zero exit. Glidepath does not know what the files
mean: the image is `valuesValidatorImage` in the catalog values, and it must provide
`validate-values [--app NAME] FILE...` on its PATH. The Hangar default is Airframe's `airframe-validate` image
(strict keys, component XRD specs, `helm template`), so Glidepath stays installable without Airframe: use another
image, or never register the gate.

Pieces: Task `validate-values`, Pipeline `values-check`, onboarding template
`gitops-repo/pull-request-values.yaml` (runs on every PR to `main` that touches a `values.yaml`).

**Rollout order matters.** Registering the gate in `releaseGuardrails` makes `detect-bypass-merge` treat its
check as required on every merge, so:
1. Publish the validator image and make its package public.
2. Let the onboarding-resync PR (which adds `.tekton/pull-request-values.yaml`) merge in every gitops repo.
3. Only then add `values` to `releaseGuardrails` (status `real`; the name must match the PipelineRun's `generateName` prefix, `values-`, because `detect-bypass-merge` looks up the check `Pipelines as Code CI / values-`). From then on a merge
   with that check red is reported as a bypass. It does **not** make the check required on other PRs
   (a human's or an agent's hand edit to a `values.yaml`): that is the GitHub ruleset's required-status-checks list,
   a separate setting kept in sync by hand (see "Branch protection" below). Add `Pipelines as Code CI / values-` there.
Before step 3 the check runs and reports but does not block.

## Removing a gate

Delete its onboarding template file
(`charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-<name>.yaml`)
and its `releaseGuardrails` entry. If it had a dedicated Pipeline/Task (a promoted-to-real
gate), remove those too once nothing else references them. Worked example: the
`image-promotion` retirement, 2026-10-09 (ADR-0025). Three things to know:

- **Already-onboarded gitops repos** still have the old `.tekton/pull-request-<name>.yaml`
  until their next onboarding-resync PR. Since 2026-10-09 that resync removes a
  platform-generated gate file whose template no longer exists (it recognises its own
  files by their `# STUB` / `# BOILERPLATE` / `# The real ... gate` first line; a
  tenant's own `.tekton` file carries no such marker and is left alone, as before).
  Trigger a resync for every app after the gate is removed.
- **The catalog Application does not prune.** Removing a Pipeline/Task from the chart
  leaves the live object in `platform-catalog` (OutOfSync, still resolvable). Delete it by
  hand once every gitops repo's resync has merged - until then the stale `.tekton` file
  still runs against it, which is the safe order.
- **Branch protection** on the gitops repo's `main` still lists the old check name as
  required (see below) - a required check that no PR will ever produce again blocks every
  future release PR from merging until removed.

## Branch protection

> **Gap (2026-09-27):** in practice the enforcement is a **GitHub ruleset** on the gitops repos, and it is an integral,
> mandatory part of a Glidepath install: without its required-status-checks list none of these gates block a merge.
> This section still describes classic branch protection. The ruleset is documented, and audited against a real export, in
> [github-ruleset.md](github-ruleset.md); generate and apply it with `hack/apply-guardrail-ruleset.sh`. Read that first.

Not IaC-managed by this platform (docs/release.md's own "Onboarding" section covers the
one-time per-repo setup) - `releaseGuardrails` and branch protection's required-status-
checks list are two independent settings that must be kept in sync **by hand**. Adding a
gate here without also adding it to branch protection means the check runs and reports,
but never actually blocks a merge. Removing a gate without removing it from branch
protection means every future release PR is permanently unmergeable (a required check
that will never run again). Neither mismatch is caught automatically - update both
together.

## RESOLVED 2026-09-05: `sast` no longer fails closed - Rekor is live

Rekor was deployed for real (`docs/admin/provenance-policy.md`), Tekton Chains now
uploads to it, and `verify-image-provenance.yaml`/`verify-sast-attestation.yaml` do real
tlog verification (`--insecure-ignore-tlog=false`) instead of checking the cert against
wall-clock now. Live-verified through a real release PR: both gates' cosign checks now
print `Verified OK` using the Rekor entry's own `integratedTime`, independent of how long
release-time re-verification takes. The rest of this section is left as written for
historical accuracy - the structural problem it describes is real, it's just fixed now
via the first option listed under "not fixed here" below.

## Historical: `sast` failed closed on essentially every real release

**Found live 2026-08-23. Two
gates failed in the same test run for what first looked like the same reason; live
evidence showed they're actually two different problems (see the `provenance` fix
just above for the other one - a slow-but-legitimate `ec validate` policy evaluation
tripping its own 5-minute internal cap, now given more headroom).

`sast`'s failure is different and NOT fixed by a bigger timeout.
`verify-sast-attestation.yaml` re-verifies the Fulcio leaf certificate that Tekton
Chains attached to the *build*-time attestation, at *release-PR* time - necessarily
minutes later, since build->test->deploy->release all run first. This platform's Fulcio
issues leaf certs with a **10-minute validity window** (confirmed live: `openssl x509
-noout -dates` on the real cert used in the failed check, `notBefore`/`notAfter` exactly
10 minutes apart), and deliberately runs with Rekor/tlog verification disabled
(`transparency.enabled=false`, `--insecure-ignore-tlog=true` - see
docs/provenance-policy.md). Without a tlog entry to anchor "this cert was valid *at
signing time*", cosign has no choice but to check the cert against wall-clock *now* -
and by release-PR-check time it's routinely already expired. Confirmed with `openssl
verify`, not just cosign's own terse error: `error 10 at 0 depth lookup: certificate has
expired`. (`verify-image-provenance.yaml`'s own cosign check does the same
leaf-cert-against-now verification and is exposed to the identical risk - it happened to
still be inside the 10-minute window on this particular run, "Verified OK" printed right
before the unrelated `ec validate` timeout, but that's timing luck, not a guarantee.)

This is **structural, not a resource-contention flake**: any release where more than
~10 minutes elapse between the build stage and the release-PR check running (i.e.
essentially all of them - test+deploy+release plus PR-check scheduling delay reliably
exceeds 10 minutes) will fail `sast` closed, every time, regardless of cluster load, and
`provenance` is one slow evaluation away from the same fate. The `image-scan`/`sbom`
gates aren't affected the same way - they scan/sign fresh at check time rather than
re-verifying an old cert.

Not fixed here because the real fixes are all bigger platform decisions than "reroute
the guardrails," and one of them was already tried and deliberately shelved:

- **Enable Rekor** (the sigstore-native fix - a tlog entry lets a later verification
  check cert validity *at signing time* instead of *now*). Previously attempted and
  abandoned after repeated live failures caused by the dev cluster's then-unstable local runtime,
  not a design objection - see the prior Rekor-install session notes. Would need to be
  re-attempted deliberately, not as a side effect of this work.
- **Run a Timestamp Authority (RFC3161)** as a lighter-weight alternative to full Rekor -
  cosign supports `--timestamp-server-url` for exactly this. Still new infrastructure to
  build and operate, not yet evaluated.
- **Do not** widen `--insecure-ignore-*` further to also ignore cert expiry - that
  discards the one thing short-lived-cert verification is actually for (proving the key
  that signed this wasn't already compromised/rotated away by the time anyone checks),
  not a fix.

Until one of these is decided and built, treat `sast`/`provenance` failures on a real
release PR as expected/known, not a signal something regressed - `/retest` will not help
(the cert only gets *more* expired). Bypass sits behind existing break-glass tooling
(`detect-bypass-merge.yaml`, docs/release.md's "Break-glass" section) if a real release
needs to merge in the meantime.
