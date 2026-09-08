# Secrets management

External Secrets Operator (ESO) is platform infrastructure, not just a bootstrap-time
install alongside Tekton/PaC/kube-prometheus-stack. Every chart in this platform consumes
secret material via a real `ExternalSecret`, not a hand-applied raw `Secret`.

## The backend: self-hosted Infisical

**2026-08-19: migrated off ESO's built-in `kubernetes` provider onto real Infisical.**
ESO was installed early in this platform's life but had zero configured backends until
Phase 3 item 7; the pragmatic bridge that followed (ESO's `kubernetes` provider,
mirroring real `Secret`s out of one hand-managed namespace) was always documented as a
stopgap pending "community Infisical, via Dream IDP" - that future step has arrived.
`airframe` runs a self-hosted Infisical instance (kind-dev only) plus
`infisical-secretstore-operator`, the same mechanism airframe-application-delivered apps
already use for their own runtime secrets.

The control plane gets its **own** Infisical project (not an app's), one per cluster it
runs on:

```
apiVersion: secrets.idp.io/v1alpha1
kind: InfisicalProject
metadata:
  name: platform-cicd-kind-dev
spec:
  projectName: platform-cicd-kind-dev
  slug: platform-cicd-kind-dev
  environmentSlug: shared
  credentialsSecretName: platform-cicd-infisical-creds
  authMethod: kubernetes
```

(`charts/glidepath-control-plane/templates/secretstore/infisical-project.yaml`).
`authMethod: kubernetes` (zero-persisted-credential, ESO's controller SA token verified
live against this cluster's own TokenReview API) is correct here because kind-dev is
also the Infisical host - see `infisicalHost`/Kubernetes-vs-Universal-Auth in
`idp/docs/service-catalog-design.md` Item 8 for the underlying mechanism this reuses. A
cluster that runs this control plane but does NOT host Infisical would need
`authMethod: universal` instead - not built, since kind-dev is currently the only
glidepath-control-plane install.

The resulting `ClusterSecretStore` (`platform-secret-store`, name unchanged from the old
`kubernetes`-provider version so every downstream `secretStoreRef.name` reference kept
working) points `provider.infisical` at that project, `environmentSlug: shared`,
`secretsPath: "/"` - every platform-wide key (`registry-credentials`,
`github-app-creds`, `relay-token-<cluster>` per registered upper cluster) lives flat
under that one path. **The user still plants real credential material by hand, via
Infisical directly (UI or API), never through this chart or committed to this repo** -
this changes nothing about who creates real credential material or how, only where it
lives and how every chart consumes it.

## Application-owned secrets: each app's own idp-managed ClusterSecretStore, directly

An Application's own secrets (Slack webhook, SAST scan credentials, ...) - see
[app-secrets.md](app-secrets.md) - come from THAT Application's own idp-managed
`ClusterSecretStore` (`<appName>-<devClusterName>`, e.g. `checkout-api-kind-dev`,
provisioned by idp's `NodeJSApplication` XR) - referenced **directly, by name**, from
`glidepath-app`'s own `app-secrets-external-secret.yaml`. No platform-cicd-rendered
store in between. Never a platform-cicd-owned project either - app secrets are the app
owner's to manage, once, in the one place idp already gives every onboarded app.

**Two corrections to this mechanism, both made the same day as the initial
migration**:

1. The first pass put every Application's secrets in a platform-cicd-owned
   `platform-cicd-kind-dev` project instead, path-scoped per app
   (`/<type>/<appName>/`) - a real design mistake. It meant `slack-webhook-url`
   specifically needed planting TWICE: once there, and once in the app's own project,
   where `airframe-application`'s own AI-triage Slack notifications already read it from.
2. The immediate fix still rendered a platform-cicd-owned `ClusterSecretStore`
   (`<type>-<appName>-secret-store`) per app, just repointed at the app's own project -
   a real, live-caught redundancy: `kubectl get clustersecretstore checkout-api-kind-dev
   app-checkout-api-secret-store -o yaml` showed byte-identical `spec.provider`
   blocks, except idp's own store also carried a `namespaceRegexes` scope the mirror
   never had - the mirror was strictly WIDER than the original, a real least-privilege
   gap, not just duplication. Fixed by deleting the mirror entirely
   (`app-secret-stores.yaml`, and the `appSecretStores` values list) and referencing
   idp's object directly.

`platform-cicd-kind-dev` now holds only genuinely platform-wide material
(`registry-credentials`, `github-app-creds`, relay tokens) - never any app's own
secret, and never a second copy of an object idp already owns.

This does mean an Application's CI secrets require that Application to have been
onboarded through idp's `NodeJSApplication` XR first (a real, accepted coupling, not
true for every platform-cicd tenant today) - an app that hasn't just gets a
not-ready `ExternalSecret` (no store to reference at all), the same graceful-degrade
shape every other "not configured yet" case in this platform already tolerates.

Before Infisical, this worked off each Application's own `<type>-<appName>-dev`
namespace, treated as an ad hoc vault (a real `Secret` created there by hand) - CI/
deploy infrastructure doubling as a secret backend. That's gone too.

## registry-credentials: disseminated via ClusterExternalSecret

**2026-08-19: no longer a per-app opt-in.** Every Application used to need its own
`registry-credentials` `ExternalSecret`, rendered by the app chart (glidepath-app)
or, for airframe-application-delivered workloads, gated behind a `registryCredentials.enabled`
values flag. Both are gone, replaced by ONE `ClusterExternalSecret`
(`charts/glidepath-control-plane/templates/secretstore/
registry-credentials-cluster-external-secret.yaml`, mirrored hand-authored on kind-prod
in `gitops-cluster-kind-prod/10-crds-operators/external-secrets/`), which generates the
`registry-credentials` `Secret` automatically in every namespace labeled
`hangar.io/managed-secrets: "true"` - applied by ArgoCD's own
`syncPolicy.managedNamespaceMetadata` on every namespace this platform's
`tenant-onboarding` `ApplicationSet`s create (both platform-cicd's own CI namespaces and
idp's `app-<appName>-<env>` ones), re-applied every sync so it also lands on
already-existing namespaces. Every app's own `ServiceAccount` (both
`charts/glidepath-app/templates/identity/pipeline-runner.yaml` and
`airframe-application`'s `workload/serviceaccount.yaml`) references that Secret name
unconditionally now - no per-app opt-in left to forget.

Populating the credential itself (one-time, manual, by design): plant a
`registry-credentials-dockerconfigjson` key holding the fully-rendered
`.dockerconfigjson` payload into the control plane's own Infisical project, once per
cluster - same "manual by design" posture as everything else here. A single-flat-value
key, not multiple Infisical fields extracted via `dataFrom.extract`, since ESO's real
provider behavior for that combination against Infisical wasn't confirmed live going
into this migration - safer to store the one already-shaped JSON blob and template
`kubernetes.io/dockerconfigjson` from it directly (the same pattern airframe-application's
own registry-credentials template already used).

## Relay-token distribution

**2026-08-19: built for real, closing the gap [multi-cluster.md](multi-cluster.md)
tracked as deferred.** Every app onboarded to a cluster-mapped env used to need a
hand-provisioned `platform-outcome-relay-token` `Secret`, copied by hand into every
app's namespace on the upper cluster (`hack/bootstrap-upper-cluster.sh`). Now:

- The dev-cluster (verifier) side - `cluster-<name>-relay-token` `Secret`s in
  `platform-system`, which `argocd-outcome-relay` compares bearer tokens against - sync
  from `platform-cicd-kind-dev`'s Infisical project (`relay-token-<cluster>` key per
  registered cluster), via `charts/glidepath-control-plane/templates/clusters/
  relay-token-external-secret.yaml`.
- The upper-cluster (caller) side - `platform-outcome-relay-token`, read by the
  release-tracking hook Jobs (`catalog/lib/argocd-outcome-hook.sh`) - syncs from that
  cluster's OWN `platform-cicd-<cluster>` Infisical project, via
  `airframe/charts/airframe-application`'s `templates/release-tracking/
  relay-token-external-secret.yaml`, gated on `releaseTracking` exactly like the hook
  Jobs/RBAC it accompanies.

The token value still has to originate somewhere real - plant the same value into both
clusters' own Infisical projects by hand, once per cluster (not per app). See
[multi-cluster.md](multi-cluster.md) for the full mechanism this feeds.

## What this deliberately is not

Not a real vault beyond what Infisical itself provides - real credential material now
lives in a real secrets manager (encrypted at rest, access-controlled by real Infisical
project membership) rather than base64-encoded etcd `Secret`s, a genuine improvement
over the old `kubernetes`-provider bridge, but ESO's `refreshInterval` still just
re-syncs on a timer - it doesn't rotate the underlying credential itself.

Excluded from this migration, deliberately: `fulcio-secret`/`fulcio-server-config`
(fresh, per-cluster-generated root key material by design - not something "synced from
a backend" makes sense for). **2026-09-05: this generation moved from a manually-run
script into an ArgoCD pre-install hook Job**
(`charts/glidepath-control-plane/templates/hooks/fulcio-bootstrap-job.yaml`) - the
key still never touches Infisical, a repo, or a human's terminal, only the trigger
changed (runs on first cluster sync instead of an operator remembering to run
`hack/generate-cluster-values.sh`, which is now a documented manual override path for
deliberately rotating a root - see `docs/admin/adr/0006-cluster-agnostic-bootstrap.md`).

Rekor+Trillian's MySQL credentials (deployed 2026-09-05, `docs/provenance-policy.md`)
are a narrower, deliberate exception in the other direction: genuinely internal,
ClusterIP-only DB auth for a local Merkle-tree store, with no ability to forge a
signature or impersonate this cluster's identity if leaked - unlike Fulcio's root key,
a committed value is an acceptable tradeoff here. Generated by
`gitops-cluster-template/hack/customize-cluster.sh` the same way this doc's own
Infisical PG/Redis passwords are, living in
`gitops-cluster-*/50-platform-cicd/rekor/values-secrets.yaml` - not routed through
Infisical, since this is lower-stakes than anything else this doc catalogs and adding a
real secrets-manager round trip for it wouldn't buy much.

**2026-08-23: `pipelines-as-code-secret` is no longer excluded.** It's still PaC's own
third-party Secret, outside this platform's *chart* boundary - but after kind-dev's full
etcd-WAL-corruption rebuild required hand-recreating it from scratch, the "outside our
chart" argument didn't justify the repeated manual-recreate-on-every-cluster-rebuild
cost, especially since the values it needs (`github-application-id`/`github-private-key`)
already sit in `platform-cicd-kind-dev`'s Infisical project for `github-app-creds`'s
sake. Now a second, independent `ExternalSecret` synced from the same Infisical keys,
living in PaC's own install boundary instead of this chart:
`gitops-cluster-dev/50-platform-cicd/tekton-operator/
pipelines-as-code-secret-external-secret.yaml` (targets `tekton-pipelines`, not
`pipelines-as-code` - see that file's own header for why). `github-app-creds` is
unaffected - still a separate copy, still what token-review-interceptor consumes; the
two `ExternalSecret`s just happen to read the same source values now.

**2026-08-24: a third key, `webhook.secret`, was missing from that first pass** -
`github-app-creds` never needed it (token-review-interceptor only mints installation
tokens, it doesn't validate inbound webhook deliveries), but PaC's own controller reads
it from this same Secret to verify GitHub webhook signatures before minting a token
itself - confirmed live: every push/PR was being rejected
(`"[SECURITY] Blocked GitHub App token minting before webhook signature validation
completed"`) until this was added. Exact key name confirmed against
`tektoncd/pipelines-as-code`'s own v0.49.0 source, not guessed. This value never existed
in Infisical at all (confirmed by listing every key already there) - not something lost
in the rebuild, since GitHub Apps make a webhook secret write-only after creation
anyway. Plant a freshly-generated value as `github-webhook-secret` in
`platform-cicd-kind-dev`'s Infisical project AND set the same value on the GitHub App's
own Settings > Webhook secret field - same "manual by design" posture as the id/private
key above, and the two sides (Infisical, GitHub) have to actually agree.

## GitHub credentials in this platform

A live audit (2026-09-01, both real clusters, hash-compared where a value's identity
mattered so no token was ever actually exposed) found this platform depends on one
shared GitHub App plus several independent classic PATs - worth a single reference
here since they'd otherwise only be discoverable by reading every consumer's own
manifest by hand.

**The shared platform GitHub App** - one App ID + private key, reused as-is by three
independent consumers (confirmed live: all three `Secret`s carry byte-identical key
material):

| Consumer | Secret | Purpose |
|---|---|---|
| ArgoCD (dev cluster only) | `github-app-repo-creds` (`argocd`/`argocd-apps`) | Private-repo git sync |
| `platform-cicd` control plane | `github-app-creds` (`platform-system`) | `token-review-interceptor`'s installation-token minting (release PRs, onboarding file delivery, PR-generator-token refresh) |
| Pipelines-as-Code | `pipelines-as-code-secret` (`tekton-pipelines`) | Webhook receipt + git operations; also carries `webhook.secret`, a separate HMAC value the other two consumers never needed |

All three sync from the same three Infisical keys (`github-application-id`/
`github-private-key`/`github-installation-id`) via independent `ExternalSecret`s - see
`pipelines-as-code-secret`'s own history above for why that one in particular is a
second, independently-synced copy rather than a shared `Secret` reference.

**Independent classic PATs** - each a distinct credential, not derived from the App
above, and none currently ESO-managed except where noted:

| Secret | Scope | Purpose | ESO-managed? |
|---|---|---|---|
| `provider-github-creds` (`crossplane-system`) | `repo` + `delete_repo` | Crossplane `provider-upjet-github` - repo create/delete for the Bootstrap-tier XRDs. A GitHub App can't create repos on a personal account, hence a raw PAT specifically here | Yes |
| `registry-credentials-dockerconfigjson` (Infisical key) | `read:packages`/`write:packages` | GHCR image pull, every tenant namespace, every cluster - see the dedicated section above | Yes |
| `<app>-pr-generator-token` (one per onboarded app, `app-<name>-cicd`) | undocumented upstream, presumed `repo` + PR-write | ArgoCD `ApplicationSet`'s `pullRequest.github.tokenRef` for PR-based ephemeral environments | No - hand-minted per app, deliberately (see below) |
| `argocd-repo-creds-<you>` (upper clusters, e.g. kind-prod) | classic PAT, username+password shape | ArgoCD's private-repo git sync, on any cluster not reusing the shared App above | No |
| `backstage-github-token` | `repo` only (no `delete_repo`) | Backstage scaffolder's `publish:github:pull-request` action | No |
| `github-mcp-token` (`holmesgpt`) | undocumented | GitHub MCP server token for HolmesGPT AI-triage | No |

**Known gap, not yet closed**: only dev-hosted clusters reuse the shared GitHub App for
ArgoCD repo access. Upper clusters (kind-prod) still carry their own standalone
`argocd-repo-creds-<you>` PAT instead - real drift between the two mechanisms doing the
same job, not an intentional design split. Migrating upper clusters onto the shared App
would let them drop this PAT entirely.

**Confirmed live, not assumed**: the six `<app>-pr-generator-token` values are six
genuinely distinct PATs (hash-compared, never decoded) - not one shared token copied
per app. `docs/admin/ephemeral-environments.md` describes these as short-lived,
GitHub-App-derived installation tokens refreshed automatically; the real, currently
deployed mechanism is static, hand-minted PATs instead - a prior review of minting
these via ESO's Webhook generator concluded the persisted-credential tradeoff wasn't
worth it at this call frequency, so treat these six as long-lived manual credentials to
track, not auto-rotating ones.
