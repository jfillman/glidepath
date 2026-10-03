# ADR-0018: Glidepath owns the per-app environments folder and defines the chart contract

## Context

Every onboarded app repo carries a `platform/` folder: `platform/base.yaml`,
`platform/envs/<env>.yaml`, `platform/envs/<env>.release.yaml` and
`platform/pr-env.yaml`. It was introduced under the "Ground environments" idea in
`hangar/docs/gitops-strategy.md` and has no owner on paper.

The 2026-09-26 revision of `hangar/docs/autopilot/release-file-split.md` (§1) decided
Airframe owns it and it should be named `airframe/`, on the test that each product must
be installable without the others. Applying that test to the real code says otherwise:

- The lower-envs ApplicationSet generates one environment per `platform/envs/*.yaml`.
  The ephemeral-envs ApplicationSet reads `platform/pr-env.yaml`. `deploy-manifests`
  writes into the folder. All three are Glidepath machinery. Airframe without Glidepath
  has nothing that reads these files.
- A file's existence is what declares an environment. That is Glidepath's concept. The
  file's contents are Helm values for whichever chart is deployed.
- Glidepath is meant to ship standalone, with any app Helm chart. Today both
  ApplicationSets hardcode `charts/airframe-application` at a pinned tag.

## Decision

1. Glidepath owns the folder. It is renamed `glidepath/`:
   `glidepath/base.yaml`, `glidepath/envs/<env>.yaml`,
   `glidepath/envs/<env>.release.yaml`, `glidepath/pr-env.yaml`.
2. Glidepath treats the contents of those files as opaque Helm values, except for the
   release keys it writes itself. It never interprets the rest.
3. The chart that consumes them is per-app configuration, not a constant. The interface
   between Glidepath and any chart is `chart-contract.md`.
4. Airframe's `airframe-application` chart is the reference implementation of that
   contract. Airframe still owns the values schema, the validator and Tower's Config tab;
   those take the folder path as a parameter.
5. `deploy.target` stays per app. Apps with a cloud target (`aws-ecs`, `aws-lambda`,
   `azure-container-apps`, and the function XRDs' equivalents) have no chart and no
   environments folder.

This supersedes §1 of `release-file-split.md`. Its §2 (`release.image` split) stands.

## Consequences

- The default of `deploy.releaseFile` changes from `glidepath/releases/{env}.yaml` to
  `glidepath/envs/{env}.release.yaml`.
- Migration follows the dual-path window already designed: both ApplicationSet
  generators list `platform/` and `glidepath/` while writers move first; an `envName`
  must not appear in both; the old path is dropped last. Tracked in `known-gaps.md`.
- Files that name the old path and must move: the lower-envs ApplicationSet (apron and
  gitops-cluster-dev copies), `deploy-manifests`, `open-release-pr`,
  `deliver-onboarding-files`, the deploy pipeline, `run-testworkflow`, `ephemeral-envs`,
  `deploy-rbac`, `appproject`, the PR-preview notify job, the PaC config-only-push
  exemption, Tower's `GlidepathTab` and `PromoteDialog`, the Airframe scaffolds, and
  every app repo.
- `cicd.yaml`'s `apiVersion: platform/v1` is unaffected; that is a separate decision.
- A new `deploy.chart` field is needed before bring-your-own chart works. Until then the
  contract is documented but only the reference chart satisfies it.
