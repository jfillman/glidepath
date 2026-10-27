# ADR-0028: The policy-validation gate evaluates the release offline against the target cluster's policies from git

*Status: Accepted (2026-10-26). Built: `policy-validation-check` / `policy-validation-gate`, the broker's
`/policy-bundle`, the Kyverno CLI in the toolbox, `policyValidation` in glidepath-control-plane. Detail:
[policy-validation.md](../policy-validation.md).*

## Context

`policy-validation` was the last stub release gate (ADR-0003): the shared `governance-check` Pipeline, always green.
Its stated intent was to evaluate a release's rendered manifest against the target cluster's admission policies
before the merge, so that a release the cluster would refuse is caught on the PR, not as a failed Argo CD sync after
it.

Four questions decide the shape:

1. **Which policies.** The target cluster's, not a platform-wide set: Flight clusters carry their own (hangar-prod's
   are native ValidatingAdmissionPolicies, hangar-dev's Kyverno ValidatingPolicies). Apron renders them into the
   cluster's GitOps repo under `30-policy/`, and the cluster's own Argo CD syncs that repo's `main`. Git is the
   desired state; the live cluster lags it at most by a sync.
2. **How to read them.** Upper-zone clusters take changes only through a reviewed merge their own Argo CD syncs
   (ADR-0024): no pipeline credential to them exists, and none should be added for a read. The GitOps repo is
   readable by the platform's GitHub App, but it also holds per-cluster generated passwords (committed by design,
   Apron `cluster.yaml`), and an app's pipeline namespace runs the app team's own build scripts under the same
   ServiceAccount the gate uses. A repo token handed to the gate is a token handed to every app.
3. **How to render.** The values gate already renders a release (the validator image: helm and the
   airframe-application chart at the cluster's default-chart tag; an own chart fetched per ADR-0023).
4. **How to evaluate.** The clusters run Kyverno 1.19; its CLI evaluates ValidatingPolicy, ClusterPolicy and native
   VAPs offline, with two gaps found while building this (both covered by the gate's tests): a policy with
   background evaluation off, which is every admission-only policy, is skipped; and a native VAP's
   `matchConstraints` are ignored, so it is applied to every resource.

## Decision

1. **Source: the cluster's GitOps repo, at the revision its Argo CD syncs.** `policyValidation` in
   glidepath-control-plane: `repoURL` (default `https://github.com/{owner}/gitops-{cluster}.git`), `path`
   (`30-policy`), `revision` (`main`), per-cluster overrides. `{cluster}` is the environment's cluster, the first path
   segment of `<cluster>/<env>/values.yaml`.
2. **The broker reads it; the gate gets documents, never a token.** `POST /policy-bundle {cluster, owner}` on the
   existing broker (it already holds the GitHub App key): TokenReview with audience `policy-bundle`, the caller's
   namespace must hold a PaC Repository CR, a `contents: read` installation token scoped to that one repo that never
   leaves the broker, and only ValidatingPolicy / NamespacedValidatingPolicy / ClusterPolicy / Policy /
   ValidatingAdmissionPolicy / ValidatingAdmissionPolicyBinding documents in the response, with the commit read.
   Policies are not secret, so any Glidepath app may read any cluster's bundle.
3. **Render like the values gate.** Same validator image and chart resolution; the Flight ApplicationSet's injected
   values and value files (base < values < release); namespace `<type>-<app>-<env>`. Every environment the PR
   changes, and every environment under a changed `<cluster>/base.yaml`.
4. **Normalize, then evaluate with the Kyverno CLI at the clusters' version** (1.19.0, in the toolbox). Background
   evaluation is turned on in the evaluated copy; each VAP binding becomes the equivalent ValidatingPolicy (same CEL,
   the binding's actions, its selectors); stand-in CRDs for custom kinds (Airframe's XRD plurals, else the API
   server's guess); the rendered Namespaces' labels feed `namespaceSelector`.
5. **Only the policy's own mode blocks.** A failure under `Deny` (Kyverno `Enforce`) fails the check; `Audit`/`Warn`
   failures and rules offline evaluation cannot decide (`error`, typically a field the API server would default) are
   warnings. A render or evaluation that fails outright fails the check: an unreadable policy source is not a pass.
6. **Registry: `class: integrity`, `required: true`, `targets: [k8s-rollout]`, no rollback-advisory copy.** Like
   `values`, it judges whether the release can be applied, not the image; admission makes no exception for a
   rollback. Required from the start because the policy mode already separates blocking from reporting, and a Deny
   violation would fail the sync anyway; one cluster can still make it advisory through `releaseGuardrailOverrides`.

## Consequences

- A release the target cluster would refuse is red on its PR with the policy, rule, resource and message.
- What the gate sees is what is in git: a policy applied by hand to a cluster, or one outside `path`
  (`pipeline-runner-application-refresh-only` on hangar-dev lives in the control-plane chart), is not evaluated.
  ImageValidatingPolicy (needs registry access), mutating policies, VAPs with `paramKind`, and Pod Security
  Admission labels are not evaluated.
- Offline evaluation has no API-server defaulting and no `request.userInfo`; policies that guard optional fields
  with `has()` (the platform rule since 2026-10-11) evaluate exactly; others surface as warnings.
- Today's Flight policies (break-glass identity rules) do not match application objects, so every current release
  passes; the gate starts mattering the day an app-facing policy lands in `30-policy/`.
- The repo naming default (`gitops-{cluster}`) is a convention, not read from the fleet file; a cluster with another
  repo name needs an override. Adding the cluster repo URL to the fleet `clusters.yaml` record would make this
  generator-driven.
