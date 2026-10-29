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
registry of every gate enforced on a release PR - `name`, a human `description`,
`status` (`real` or `stub`, cosmetic only - see below), `class` (`content`, `process`,
`integrity`, `approval`), optional `targets`, and `required` (default `true`; see
"Required and advisory gates" below). The chart resolves the list once
(`_helpers.tpl`, `glidepath-catalog.guardrails`: `required` defaulted, a cluster's
`releaseGuardrailOverrides` applied) and every consumer renders off that instead of
hardcoding the gate list:

- the gate Pipelines (`sast-check`, `image-scan-check`, `sbom-check`, `qa-check`,
  `values-check`) - whether the hard task renders at all;
- `catalog/tasks/detect-bypass-merge.yaml` - the break-glass Slack alert's "did every
  required check actually pass" loop;
- `catalog/tasks/release-log-emit.yaml` - every gate's conclusion in the release log,
  `bypass` from the required ones;
- `catalog/tasks/wait-for-release-guardrails.yaml` - the sibling set a shape-B gate waits on;
- `catalog/tasks/open-release-pr.yaml` - the release PR body's governance-checks table;
- `hack/apply-guardrail-ruleset.sh` - the GitHub ruleset's required status checks (offline,
  with `-f` for a cluster's values).

Everything else about a gate - which Pipeline it runs, what it actually checks, whether
it posts a PR comment - is standard Tekton/PaC config, not driven by this list. Adding a
name to `releaseGuardrails` **alone does nothing**; the gate exists once its own
`.tekton/pull-request-<name>.yaml` onboarding template and Pipeline exist too (below).
Branch protection (which checks are actually *required* to merge) is a separate, manual
GitHub setting - see "Branch protection" below.

## Two shapes, depending on what the gate needs

**A. Independent stub or real check (the common case)** - most gates (sast, image-scan,
provenance, sbom, itsm, qa, policy-validation) run in parallel off the same PR
webhook, with no dependency on each other's outcome. (`itsm`, since 2026-10-19, is a third shape:
its Pipeline `itsm-check` renders as the stub while the catalog names no change tool and as the real
gate once `itsm.provider` is set, so the onboarding template never changes, see
[change-management.md](change-management.md).)

- **Stub, fully generic (least effort)**: add one onboarding template file,
  `charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-<name>.yaml`,
  pointing at the existing shared `governance-check` Pipeline with `gate-name: <name>`
  (`pull-request-policy-validation.yaml` before 2026-10-26, in git history, is the
  shape). That Pipeline already calls `governance-gate-stub` (loud "no real check
  implemented yet" logging + a `governance.stub=true` span + a `"stub"` result, never a
  silent pass) and posts a PR comment. **No new Pipeline or Task needed** - this is the
  whole change, plus the registry entry above. Good default when the gate's eventual
  real shape isn't known yet.
- **Stub, dedicated (scaffolded for graduation)**: if you already know roughly what the
  gate will eventually need to check, give it its own Pipeline+Task *now*, still calling
  `governance-stub` under the hood. `qa` was built this way and graduated on 2026-10-19:
  `qa-check.yaml`/`qa-gate.yaml` now show the real shape, and git history (before that
  date) the scaffold. Same honest stub behavior and `status: stub` as the fully-generic
  case (this is NOT the "Real" case below - no stub marking gets removed), but graduating
  later is then a swap of the Task's own step body, instead of writing a new
  Pipeline+Task and repointing the onboarding template all at once.
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
  `policy-validation` graduated this way from the generic stub on 2026-10-26 (ADR-0028):
  its own `policy-validation-check` Pipeline and `policy-validation-gate` Task, `class:
  integrity` like `values` (so no rollback-advisory copy), see
  [policy-validation.md](policy-validation.md).

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
event-driven fan-in on the release record rather than a polling pod. It came back that way with the Flight registry
([ADR-0029](adr/0029-ground-and-flight-registries.md) phase 3, [registries.md](registries.md#the-fan-in-phase-3)):
with catalog `promotion.enabled`, `image-promotion` is a required entry of the effective registry (not of
`releaseGuardrails`, and no `.tekton` file), started by the last gate's `finally` instead of a waiting Pipeline.

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

## Required and advisory gates (`required`, 2026-10-19)

Every registry entry carries `required` (default `true`). A real gate with `required: false` is
**advisory**: its Pipeline renders only the `-advisory` copy of its check (`onError: continue`,
the same copy every content gate already runs on an eligible rollback, ADR-0021 decision 8), so
the check run is green whatever the check finds, and `comment-pr-check-result` posts what it
found with the "advisory until the gate is required" wording (`advisory-reason: not-required`).
Everything downstream agrees: `detect-bypass-merge` and `release-log-emit` do not count a merge
past it as a bypass, `wait-for-release-guardrails` does not wait on it, the release PR body's
table says "real, advisory", and `hack/apply-guardrail-ruleset.sh` leaves it out of the required
status checks. The gate still runs and still reports on every release PR: advisory is not off.

Two gates refuse it. `sast` and `provenance` are the image's integrity, and advising on them is
the "advises rather than enforces" weakness the SDLC review named; `required: false` on either
fails the chart render with a message naming the gate, so no cluster can ship such a catalog. A
stub ignores the field (its check is always green; it stays in every required set so a missing
check run is still noticed).

**Who sets it.** The default is the registry entry in this chart (a glidepath PR, platform
maintainers, CI runs `gate_required_test.sh`). One cluster changes it in its catalog Application
values through the `releaseGuardrailOverrides` map, never by restating `releaseGuardrails` (a
list is replaced whole on override, so a gate added upstream would silently vanish there):

```yaml
releaseGuardrailOverrides:
  qa: { required: true }
```

It is not in `cicd.yaml` and not a Tower setting: no app team or release approver can turn a
gate advisory for one release. On GitHub Free a failed check still cannot block a merge
(GATE-03), so `required: true` makes a bypass detectable (GATE-04), not impossible; re-run the
ruleset script after changing it so the required status checks match.

`itsm` is required by default once a change tool is configured; a cluster that wants to ease into
change management sets it advisory the same way. Today `qa` and `api-compat` ([api-compat.md](api-compat.md)) are the advisory gates; `qa` is report-only while every app gets its check ride
([testing.md](testing.md)); flip it once every app with a Flight environment has
`apps/<app>/check-ride.yaml` in the check-rides repository and a `role: check-ride` test step.

## Current gates

| Gate | What it verifies | Status | Required |
|---|---|---|---|
| `sast` | Static analysis (Semgrep) of the promoted commit's source | real | yes (locked) |
| `image-scan` | Vulnerability scan (Trivy) of the promoted image | real | yes |
| `provenance` | Commit signature (gitsign, or a GitHub PR merge by an allowed login) + SLSA provenance (Conforma) | real | yes (locked) |
| `sbom` | Software bill of materials | real | yes |
| `values` | Values-file validation by the validator image (Airframe's by default); check `Pipelines as Code CI / values-` | real | yes |
| `itsm` | An approved or scheduled change request for the release in the change tool ([change-management.md](change-management.md)) | stub until `itsm.provider` is set, then real | yes once real |
| `api-compat` | The promoted image's signed API spec diffed against the target environment's, with the operations each breaking change touches and whether they are in use ([api-compat.md](api-compat.md)) | real | no (advisory) |
| `qa` | A passing, recent check ride from the shared check-rides repository, attested on the promoted image ([testing.md](testing.md)) | real | no (advisory) |
| `policy-validation` | The release's environments rendered and evaluated offline (Kyverno CLI) against the target cluster's admission policies from its GitOps repo; only Deny/Enforce findings block ([policy-validation.md](policy-validation.md)) | real | yes |
| `image-promotion` | Only with catalog `promotion.enabled`: the release image copied by digest to the Flight registry with its signatures and attestations, plus a promotion attestation ([registries.md](registries.md)) | real | yes |

**How `qa` judges a release (2026-10-19, phase 3 of [testing.md](testing.md)).** The gate never runs tests. It
resolves the promoted image like the other image gates, downloads the digest's attestations, verifies each SLSA
provenance against the platform's Fulcio identity and a real Rekor entry (the same cosign flags as
`verify-sast-attestation`), and keeps the `run-testworkflow` entries whose `role` is `check-ride`. Tekton Chains
attests every completed test PipelineRun, failed ones included, so presence is not a pass: one entry must have
`outcome: passed`, `tests-failed: 0`, `workflow-source: argocd:<qa.checkRidesApplication>` (default `check-rides`,
the fence) and be younger than `qa.maxAge` (default `72h`), measured from the attestation's Rekor `integratedTime`
(the provenance's `buildFinishedOn` only if the entry has none). The PR comment carries the counts and the Testkube
execution id on a pass; on a failure it says which of the four was missing: no check ride ran for this digest, it
failed, it was not from the shared repository, or it is too old. A new build is a new digest and needs its own
ride; `/retest qa-` re-reads the registry. `qa` is a content gate: on an eligible rollback it reports without
blocking (ADR-0021 decision 8), which is where an older ride would otherwise trip it.

**`qa` is advisory** (its registry entry has `required: false`, see "Required and advisory gates" above; this
replaced the one-off `qa.required` value on 2026-10-19). While it is, the gate runs on every release PR and posts
what it found, but never fails the check, so an app that has no check ride yet is not blocked while every app
gets one.

**`provenance` on an image with check rides (2026-10-19).** Every check-ride test PipelineRun's SLSA provenance is
attached to the tested digest too ([testing.md](testing.md)), so the Conforma half of `provenance` keeps only the
build provenance (`tekton.dev/pipeline=build` label or a `build-image` task) before `ec validate input`; the test
provenances belong to `qa`. Its commit-signature half resolves the PR number from the revision commit on a
`/retest provenance-` run, where PaC hands over the raw `body.pull_request.number` template string, the way the
other gates' resolve step and PR comment already did ([provenance-policy.md](provenance-policy.md)).

**`pr-number` comes from `{{ pull_request_number }}` (2026-10-28).** The delivered `.tekton/pull-request-*.yaml` files
used `{{ body.pull_request.number }}`, which reads the raw webhook body: a `/retest` arrives as an `issue_comment`
whose body has no `pull_request`, so every retest run showed the template string as its `pr-number` input (the gates
and the `comment-pr` finally task resolved it from the revision, so the result was right, but the input was not). PaC's
own `pull_request_number` variable is set from the PR for `pull_request`, `/retest` comments and check re-runs alike.
The tasks' resolve-from-revision fallback stays, for gitops repos still carrying the old files until their next resync.

**`provenance` accepts a GitHub PR merge (2026-10-20).** The commit-signature half has two paths
([commit-signing.md](commit-signing.md)): gitsign, always on and final for any gitsign signature; and, only for an app
whose admin-owned release-signing policy enables it (off by default), a commit GitHub's web-flow key signed as the
merge of a PR into the default branch, merged by a login in `allowedMergers`, with optional strict mode (every PR
commit gitsign-signed) and required approvals. Before this, 10 of the last 12 releases failed the gate because GitHub's
merge button replaces the signature, and two were merged past it. The policy, signers included, moved out of the app's
`cicd.yaml` into the catalog's `releaseSigning` values (ConfigMap `release-signing-policy`); `cicd.yaml` can only
tighten it. What changes in the trust:

| Claim | gitsign | GitHub merge |
|---|---|---|
| Commit unmodified since signing | yes (Sigstore cert, Rekor) | yes (GitHub's pinned web-flow key) |
| Who signed | an allowed person | GitHub's servers |
| Who caused it | the signer | the PR's `merged_by`, an allowed login |
| Who wrote the code | the signer | the PR's authors; gitsign-verified in strict mode |

A stolen GitHub session can merge, so the GitHub-merge path is weaker on non-repudiation; the gate's results
(`signature-path`, `signature-detail`) and its PR comment say which path verified each release. Since phase 3 the
comment also carries a machine-readable `<!-- glidepath:signature {...} -->` line that Tower seals into the Release
Record and scores ([commit-signing.md: Evidence](commit-signing.md#evidence-how-a-releases-commit-was-proven-2026-10-20-phase-3)).

**How the image gates find the image (2026-10-09).** `sast`, `image-scan`, `sbom`, `qa` and the provenance half of
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
   with that check red is reported as a bypass. (Since 2026-10-19 the registry entry can also be added early
   with `required: false`: the gate then reports without blocking, the `validate-advisory` task in place of
   `validate`, and flipping it to `true` later is a one-line change.) It does **not** make the check required on other PRs
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
