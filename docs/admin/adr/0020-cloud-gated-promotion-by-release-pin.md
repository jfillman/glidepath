# ADR-0020: Cloud Flight environments are gated by a release pin PR on the source repo

*Status: Accepted (2026-10-06). Slice 1 of the build is not started. Nothing here is implemented. The owner's decisions on the open questions are under [Decided at acceptance](#decided-at-acceptance-2026-10-06).*

## Context

A Flight environment means "never deploys without an explicit approval". For a Kubernetes
app that approval is a release PR on `gitops-<app>` ([ADR-0004](0004-gitops-only-release.md)):
the `release` stage opens it, the governance checks run on it as required GitHub Checks,
a human merges it and Argo CD deploys.

A cloud app (`aws-lambda`, `aws-ecs`, `azure-container-apps`) has no gitops repo and no Argo
CD Application, so today it has no Flight tier: the chart refuses
`deploy.environments[].tier: flight` for a cloud target (`_helpers.tpl`,
`validateEnvironments`), and Tower's Add dialog does not offer it. Every cloud environment is
effectively Ground, so a cloud `prod` deploys on every push to `main`
([requirements doc](../envs-overhaul-requirements.md), P4, R9 to R11).

The owner decided (Q1, 2026-10-04) that the approval is a PR on the **source** repo that
changes a small per-environment release pin, reusing the PR and guardrails model, with no
gitops repo.

## Decision

1. **The pin.** `glidepath/releases/<env>.yaml` on the source repo's default branch:

   ```yaml
   release:
     image:                            # same layout as a gitops release file, so every gate's
       repository: ghcr.io/o/app       # extract-promoted-image reads it unchanged
       tag: <tag>
       digest: sha256:...              # what is deployed: the same artifact that ran in the previous environment (R11)
     promotedFrom: dev
     sourceRevision: <git sha the image was built from>
   ```

   Glidepath writes it. Nobody edits it by hand. The file is the *desired* release; what the
   environment actually runs is observed from the cloud API, as Tower already does for cloud
   records.

2. **Opening the approval.** The `release` stage, when `deploy.target` is a cloud target,
   runs a new Task, `open-release-pin-pr`, instead of `open-release-pr`. It runs on the same
   event (the previous environment's `service.deployed`), takes the digest from that event,
   writes the pin on a branch of the source repo and opens a PR. It reuses
   `open-release-pr`'s GitHub App token minting and its dedupe behaviour (one open pin PR per
   environment; a newer build updates it rather than opening another). It does not need
   `releaseTracking` or the outcome relay: those exist for Argo CD's hook Jobs.

3. **Guardrails.** The gates in `releaseGuardrails` run on the pin PR exactly as they do on a
   gitops release PR. Onboarding delivers the same `pull-request-<gate>` PipelineRuns to the
   *source* repo, restricted by CEL to PRs whose changed files are all under
   `glidepath/releases/`, so an ordinary code PR never runs the release gates and a pin PR
   never runs a full build. Branch protection on the source repo requires those checks plus an
   approval from a code owner of `glidepath/releases/`. That setting is manual, as it is for
   gitops repos today ([github-ruleset.md](../github-ruleset.md)).

4. **Deploying on merge.** Merging the pin is a push to `main` that touches only
   `glidepath/releases/<env>.yaml`. Onboarding generates a git-rooted flow
   `promote-<env>` for each cloud Flight environment: `trigger.filePathPattern:
   ["glidepath/releases/<env>.yaml"]`, root stage `deploy`, `env: <env>`. `resolve-image-ref`
   learns a `pin-file` input and takes the image from the pin instead of reconstructing it
   from a tag or revision. The existing `deploy-lambda`, `deploy-ecs` and
   `deploy-azure-container-apps` Tasks then run unchanged, with the per-environment target
   config that `resolve-deploy-target` already merges.

5. **No rebuild on a pin merge.** The `build` flow's exemption guard
   (`deliver-onboarding-files.yaml`, today `cicd.yaml` or `platform/`) is extended to
   `glidepath/releases/`. Without that, merging a pin would rebuild the app and mint a new
   image, which breaks R11.

6. **The same release can be promoted by hand.** Tower's Promote action, for a cloud Flight
   environment, calls the same backend that writes the pin and opens the PR (the backend's
   `openGitopsPr` already does branch, single-file write and dedupe, and now deletes files). The
   pipeline is the usual trigger; the button is the manual one, as for Kubernetes.

7. **Validation.** Once slices 1 to 3 below exist, the chart's `validateEnvironments`, the
   schema's cloud-target rules and Tower's `validateEnvironments` and Add dialog stop
   refusing `tier: flight` for a cloud target. Not before: allowing it earlier would let an
   app declare an environment nothing can ever deploy to.

8. **Credentials.** One cloud credential set per app (Q4). A Flight environment on the same
   account as Ground uses the same secrets; a different account per environment stays a later
   extension.

## Consequences

- A cloud app gets a real approval gate with no new infrastructure: the source repo, the PR
  model and the checks already exist.
- Source-repo history gains pin-only commits, authored by the Glidepath App as `deploy`'s
  dev commits to `platform/envs/<env>.yaml` already are (`deploy@glidepath.invalid`, which
  the build flow's loop guard keys on). Whether commit-signature rules apply to them, and
  how `allowedCommitSigners` treats the App, is **unverified** and must be checked in slice 1.
- Governance checks start running on a repo that is not a gitops repo. Each gate's Pipeline
  was written for a gitops values file. How each one finds the image to check (for example
  `provenance-check` through `verify-image-provenance`) has not been read for this ADR; any
  that parse a values file need a pin-file reader beside it. This is the largest unknown and
  the reason it is slice 3.
- The pin PR is a second kind of PR on the source repo. Tower's Pull requests tab and the
  Release record must recognise it, or it appears as an unexplained PR.

## Build order

| Slice | What | Verifiable by |
|---|---|---|
| 1 | `open-release-pin-pr` Task, branch in `release.yaml` on target, pin file shape | Run the Task against a scratch repo; assert the PR, the file, and dedupe on a second run |
| 2 | `resolve-image-ref` reads the pin; generated `promote-<env>` flow; extend the build exemption guard | Merge a pin on a scratch app: exactly one deploy run, none for build, the digest equals the pin |
| 3 | Gates on the pin PR (source-repo onboarding templates, CEL, pin readers in provenance and scan) | A pin PR shows every required check; an ordinary PR shows none of them |
| 4 | Allow `tier: flight` for a cloud target (chart, schema, Tower) | smoke-az-fn with a second Container App as `prod`: dev deploys on push, prod only after the pin PR merges |
| 5 | Tower: Promote for cloud Flight, pin PR in the Pull requests tab and Release record | Click Promote, see the PR, merge it, see the Cloud deployments tab update |

Slices 1 to 3 change the shared catalog and need a catalog tag and a pin bump per cluster.
Slice 4 changes `cicd.schema.json`, so it also needs a toolbox rebuild and a `toolboxImage`
bump in all three values files. Live verification uses Azure (smoke-az-fn works; AWS is
blocked on credentials), and each step that creates a cloud resource or a PR on a real repo
needs the owner's go-ahead.

## Decided at acceptance (2026-10-06)

- **Pin layout.** `release.image.{repository,tag,digest}`, the gitops release file's layout plus a
  digest. `extract-promoted-image` finds the one changed file in the PR and reads
  `release.image`, so the scan, SAST and SBOM gates need no pin-specific reader. Deploy tasks
  use the digest.
- **Commit signatures.** Not required for cloud targets: the provenance gate runs its
  image-provenance half (cosign/SLSA, produced by CI) and skips the commit-signature half.
  The pin Task writes through the GitHub contents API, so a ruleset that requires signed
  commits is still satisfied (GitHub signs App commits made through the API).
- **Auto-merge.** Allowed per environment as an opt-in (off by default). Glidepath enables
  GitHub auto-merge on the pin PR; whether it merges unattended is decided by the repo's
  ruleset (required checks and reviews still apply).
- **Rollback.** Required: Tower offers "Roll back" on a cloud Flight environment, which
  opens a pin PR restoring the previous pin from the file's history. Same gates, same merge.
- **Order.** Not enforced. Each cloud Flight environment's pin PR is independent;
  `promotedFrom` defaults to the previous environment in list order but any environment may
  be the source.

## Superseded open questions (kept for history)

- Whether a pin PR may be auto-merged for an environment the owner marks as low risk. The
  default is no: an approval that can be skipped is not an approval.
- Rollback. Reverting the pin PR is the mechanism, since it is a normal commit, but
  whether Tower should offer "revert to the previous pin" as a button is open.
- Several Flight environments in sequence (`staging` then `prod`). Each has its own pin and
  its own PR; whether `prod`'s PR may be opened before `staging`'s is merged is open. The
  conservative answer is no.
