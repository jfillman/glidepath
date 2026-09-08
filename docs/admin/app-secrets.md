# Application secrets (`secrets:`)

Lets an Application pull specific keys out of its own backend secret store into a
Kubernetes Secret its pipelines can mount - `slack-webhook-url` today, and open-ended
for whatever a future Task needs (SAST scan credentials was the second real case that
motivated building this generically instead of one-off per secret).

## Where the backend store lives

**2026-08-19: every Application secret comes directly from that Application's own
idp-managed Infisical `ClusterSecretStore`** - `<appName>-<devClusterName>` (e.g.
`checkout-api-kind-dev`), the exact same object airframe's
`NodeJSApplication` XR provisions and `airframe-application`'s own
`external-secret.yaml`/`notify-external-secret.yaml` already reference. `platform-cicd-
app`'s own `app-secrets-external-secret.yaml` references it **by name, directly** - no
platform-cicd-rendered store in between any more.

**This is the second correction in this mechanism's history, both made the same day**:

1. The first pass at this migration created a platform-cicd-owned
   `platform-cicd-kind-dev` Infisical project and expected every Application's
   secrets - including `slack-webhook-url`, already managed in the app's own project
   for `airframe-application`'s AI-triage notifications - to be planted there too, under a
   `/<type>/<appName>/` path. Two copies of the same credential, two places to keep in
   sync.
2. The immediate fix kept a platform-cicd-rendered `<type>-<appName>-secret-store`
   `ClusterSecretStore`, but repointed it at the app's own project instead - still a
   second, redundant object. Caught live: `kubectl get clustersecretstore
   checkout-api-kind-dev app-checkout-api-secret-store -o yaml` showed byte-identical
   `spec.provider` blocks - except idp's own store also carries a `namespaceRegexes`
   scope the platform-cicd mirror never had. Not just redundant: the mirror was
   strictly WIDER than the original, a real least-privilege gap. Fixed by deleting the
   mirror (`app-secret-stores.yaml`) entirely and referencing idp's object directly.

Before either of those, this worked off each Application's own `<type>-<app-name>-dev`
namespace, treated as an ad hoc vault (a real `Secret` created there by hand) - CI/
deploy infrastructure doing double duty as a secret backend. That's gone too.

```yaml
secrets:
  - name: slack-webhook-url      # key in the resulting app-secrets Kubernetes Secret,
                                   # and the secret's own name in this app's Infisical
                                   # project
```

**A real, accepted coupling**: an Application's CI secrets now require that Application
to have been onboarded through idp's `NodeJSApplication` XR (which is what actually
provisions `<appName>-<devClusterName>`) - a platform-cicd tenant that predates or
skips idp onboarding has no such store, so its `secrets:` entries simply stay not-ready
(a normal ESO error status, not a hard pipeline failure - nothing mounts a missing key
differently than an unconfigured one, see below).

No control-plane values list and no per-Application app-chart value name the backend
any more (the old `appSecretStores` list and `appSecretStore.secretName` field are both
gone) - the backend is entirely determined by `platformIdentity.appName` plus the app
chart's own `devClusterName` value, computed the same deterministic way idp's own
Composition computes its store's name.

## `cicd.yaml`

```yaml
secrets:
  - name: slack-webhook-url      # key in the resulting app-secrets Kubernetes Secret
  - name: sast-scan-token
    key: sast-creds-token        # only needed when the Infisical secret's own name differs
```

Each entry becomes one `data[]` entry in a single per-Application `ExternalSecret`
(`app-secrets-external-secret.yaml`), all landing in **one** target Secret
(`app-secrets`) - not one Secret per declared entry. A consuming Task always mounts the
same Secret name regardless of how many keys an Application has declared; adding a new
purpose is a `cicd.yaml` edit only, never a new volume/volumeMount anywhere. `key`
defaults to `name` - set it only when the secret's actual name in Infisical differs from
the name you want exposed as. No `remoteRef.property` - ESO's `infisical` provider
treats `property` as "extract a field from a structured/JSON secret value," not "which
flat secret to read" (confirmed live by airframe); every secret here is an
ordinary flat value, so `key` alone is correct.

The `ExternalSecret` renders **only** when `secrets:` is non-empty - an Application
that never uses this feature gets no dangling resource.

## Consuming a declared secret from a Task

Mount the `app-secrets` Secret, `optional: true` (most Applications won't declare every
key, and the pod must still start cleanly when they don't - same graceful-skip pattern
`notify-slack.yaml` already used before this existed):

```yaml
volumes:
  - name: app-secrets
    secret:
      secretName: app-secrets
      optional: true
steps:
  - name: my-step
    volumeMounts:
      - name: app-secrets
        mountPath: /var/run/secrets/platform
        readOnly: true
    script: |
      TOKEN="$(cat /var/run/secrets/platform/sast-scan-token 2>/dev/null || true)"
      if [[ -z "${TOKEN}" ]]; then
        echo "sast-scan-token not configured, skipping"
        exit 0
      fi
```

A Secret volume mounts one file per data key, named after the key - this is exactly
what `notify-slack.yaml` already does for `slack-webhook-url`, sourced from
`app-secrets`.

## Enabling Slack notifications for an Application

1. Onboard the Application through idp's `NodeJSApplication` XR first, if it hasn't
   been already - that's what actually provisions `<appName>-kind-dev`, the
   `ClusterSecretStore` everything below reads from.
2. Declare the secret in the Application's own `cicd.yaml`:
   ```yaml
   secrets:
     - name: slack-webhook-url
   notifications:
     slack:
       enabled: true
       channel: "#your-channel"
   ```
3. Plant `slack-webhook-url` in the Application's own Infisical project (`shared`
   environment, root path) - via the Infisical UI/API, never through this chart or
   committed to this repo. If `airframe-application`'s own AI-triage Slack notifications are
   already enabled for this app, this value already exists - nothing more to do.

No control-plane-side onboarding step any more - `app-secrets-external-secret.yaml`
computes the store name directly, and `notify-slack.yaml`'s volume mount is already
wired into every pipeline via the existing, unconditional `notify` `finally` task.

## Verification

- `helm lint`/`helm template` the app chart (with and without `secrets:` set),
  confirming the `ExternalSecret` renders correctly and `secretStoreRef.name` matches
  the `<appName>-<devClusterName>` naming convention idp's own store uses.
- Live: the real `<appName>-<devClusterName>` `ClusterSecretStore` confirmed to exist
  and validate (`kubectl get clustersecretstore <name> -o jsonpath='{.status.
  conditions}'`), and the resulting `app-secrets` Secret confirmed to hold the same
  value already live in that app's own Infisical project - not a copy anywhere else.
- End-to-end Slack check: a real pipeline run for an Application with `secrets:
  [{name: slack-webhook-url}]` and `notifications.slack.enabled: true` configured,
  confirming a real message lands in the real Slack channel via the Infisical-synced
  secret.
