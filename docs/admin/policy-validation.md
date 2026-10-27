# The policy-validation release check

Every gitops release PR of a Kubernetes app gets a `policy-validation` check: Glidepath renders the environments the
PR changes and evaluates them, offline, against the **target cluster's admission policies**, read from that cluster's
GitOps repo. A release the cluster would refuse is red on its PR instead of failing in Argo CD after the merge.
Decision record: [ADR-0028](adr/0028-policy-validation-offline-against-target-cluster-policies.md).

## What it does

```
release PR on gitops-<app>          broker (platform-system)               Kyverno CLI
  <cluster>/<env>/release.yaml  ->  POST /policy-bundle {cluster, owner}
                                      reads gitops-<cluster>:30-policy@main
                                      returns policy documents + commit   ->  kyverno apply
  helm template (base<values<release, the env's chart)  ------------------->  per environment
                                                                              -> report on the PR
```

1. **Environments.** Every `<cluster>/<env>/values.yaml` or `release.yaml` the PR changes, and every environment under
   a changed `<cluster>/base.yaml`. None: the check passes as `skipped`.
2. **Render.** The values gate's renderer: the validator image (`valuesValidatorRepository` at the cluster's
   default-chart tag), or the environment's own chart (`deploy.chart` / `environments[].chart`), with the values the
   Flight ApplicationSet injects and its value files, into namespace `<type>-<app>-<env>`.
3. **Policies.** The broker's `/policy-bundle` returns the admission policies of the cluster's GitOps repo
   (below). The gate never receives a token for that repo: it also holds generated passwords.
4. **Evaluate.** `kyverno apply` (1.19.0, the clusters' version) per environment, after normalizing the bundle for an
   offline run (below).
5. **Report.** A PR comment with one row per finding: environment, policy, rule, resource, message.

| Result | Meaning | Check |
|---|---|---|
| ❌ deny | a `Deny` ValidatingPolicy / VAP binding, or an `Enforce` ClusterPolicy, fails: the cluster would refuse the sync | fails |
| ⚠️ warn | an `Audit`/`Warn` policy fails, or a rule could not be decided offline (`error`) | passes |
| ❌ could not evaluate | the render failed, the bundle could not be read, or Kyverno could not run | fails |

## Which policies, from where

`policyValidation` in **glidepath-control-plane** values (the broker's `POLICY_SOURCES`):

```yaml
policyValidation:
  repoURL: "https://github.com/{owner}/gitops-{cluster}.git"   # {owner}: the gitops repo's owner
  path: 30-policy                                              # Apron's layout
  revision: main                                               # what the cluster's Argo CD syncs
  clusters:
    edge-1: { repoURL: https://github.com/acme/platform-edge.git, path: policies }
```

Every YAML file under `path` is read; only these kinds are returned and evaluated:

| Kind | Blocking when |
|---|---|
| `ValidatingPolicy`, `NamespacedValidatingPolicy` (policies.kyverno.io) | `validationActions` includes `Deny` |
| `ClusterPolicy`, `Policy` (kyverno.io) | a validate rule's `failureAction` (or the policy's `validationFailureAction`) is `Enforce` |
| `ValidatingAdmissionPolicy` + `ValidatingAdmissionPolicyBinding` | the binding's `validationActions` includes `Deny` |

Not evaluated: `ImageValidatingPolicy` (needs the registry), mutating policies, VAPs with `paramKind`, Pod Security
Admission namespace labels, and any policy that is not in the repo under `path` (applied by hand, or shipped by a
chart elsewhere).

The GitHub App must be installed on the cluster repo. The broker mints a `contents: read` token for that one repo,
uses it, and drops it.

## Offline evaluation, and where it differs from admission

The gate normalizes a copy of the bundle so the CLI evaluates it the way the API server would:

- **Background evaluation on.** Every admission-only policy (`evaluation.background.enabled: false`, or a
  ClusterPolicy's `background: false`) would otherwise be skipped by the CLI.
- **VAP bindings become ValidatingPolicies.** The CLI ignores a native VAP's `matchConstraints` and applies it to
  every resource; the equivalent ValidatingPolicy (same CEL, the binding's actions and selectors) is matched
  correctly.
- **Custom kinds get stand-in CRDs**, plural from Airframe's XRDs, else the API server's own guess (`Rollout` ->
  `rollouts`), so `resourceRules` match them.
- **`namespaceSelector` sees the rendered Namespace's labels** (plus `kubernetes.io/metadata.name`).
- `request.namespace` is the object's namespace; the operation is `CREATE`.

Differences that remain: no API-server defaulting (a Service without `spec.type` has none offline: a rule reading it
without `has()` is an `error`, reported as a warning), no `request.userInfo`, no `oldObject`. Write admission
policies with `has()` guards (the platform rule, [security.md](security.md)) and they evaluate exactly.

## Acting on a red check

- **A ❌ deny row**: change the app's values (or chart) so the rendered object complies, push to the release branch
  or comment `/retest policy-validation-`. The release would fail its Argo CD sync otherwise.
- **The policy is wrong**: that is a change to the cluster's GitOps repo (`30-policy/`) by its admins, then
  `/retest policy-validation-`. There is no per-release override.
- **Could not evaluate**: a platform problem (the log names it: render, broker, Kyverno). Not a verdict.

## Required or advisory

The registry entry (`releaseGuardrails` in glidepath-catalog) is `required: true`, `class: integrity`,
`targets: [k8s-rollout]`. Only Deny/Enforce findings block; a cluster can make the whole gate advisory with
`releaseGuardrailOverrides: { policy-validation: { required: false } }` ([release-guardrails.md](release-guardrails.md)).
It has no rollback-advisory copy: the cluster's admission makes no exception for a rollback. Cloud targets (Lambda,
ECS, Container Apps) never get the check.

## Rolling it out to existing apps

The `.tekton/pull-request-policy-validation.yaml` in each gitops repo changes from the stub to the real Pipeline.
Pipelines as Code reads it from the PR's own branch, so a gitops repo picks it up once its onboarding resync PR is
merged; release PRs cut before that keep running the stub.

## Files

- `charts/glidepath-catalog/templates/tasks/policy-validation-gate.yaml` - fetch, render, evaluate
- `charts/glidepath-catalog/templates/pipelines/policy-validation-check.yaml` - the gate's Pipeline
- `charts/glidepath-app/files/onboarding-templates/gitops-repo/pull-request-policy-validation.yaml` - the trigger
- `glidepath/broker/cmd/token-review-interceptor/policybundle.go` - `/policy-bundle`
- `charts/glidepath-control-plane/values.yaml` `policyValidation` - the policy source
- `charts/glidepath-catalog/tests/policy_validation_test.sh` - every policy shape, green / warn / red / broken
