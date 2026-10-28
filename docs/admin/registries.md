# Ground and Flight registries

Builds push to the **Ground** registry. Flight environments run images from the **Flight** registry, which only the
promoter writes. A release file pins the image by digest and records where it was built. Decision record:
[ADR-0029](adr/0029-ground-and-flight-registries.md).

## What it guarantees

- **An upper cluster runs only promoted images** (once admission is on, phase 4): a Flight-prefix rule plus a
  promotion attestation signed by the promoter workflow, verified offline by Kyverno.
- **The tag cannot change what runs.** With `promotion.pinDigest` on, every release file carries
  `release.image.digest` and airframe-application renders `repository:tag@digest`.
- **The gates verify evidence where it was made**: they read `release.image.source` (Ground), not the Flight copy.
- **No re-signing.** The copy keeps every digest, so signatures, attestations and Rekor times stay valid.
- **Nothing in a cluster can write Flight.** GHCR refuses GitHub App tokens, so the copy is a workflow in the promoter
  repository whose own `GITHUB_TOKEN` is Flight's only writer. The cluster can only *start* it, through a token the
  broker mints for one ServiceAccount (`glidepath-promoter`), and then checks the result with a read-only credential.
- **Promotion refuses what the rules forbid**, and the `image-promotion` check says why: a merged or closed PR
  (PROM-08), a release without a digest, a release that pins the Ground repository instead of Flight (PROM-01: the
  merge would deploy the unpromoted copy), a source other than the app's own Ground repository (PROM-06), a required
  gate not green on the PR's head commit (PROM-03), and an image without a Chains signature and attestation.

## Configuration (catalog values, admin-owned)

```yaml
registries:
  ground: { host: ghcr.io, namespace: "" }   # "" = the app's GitHub owner (the old hardcoded value)
  flight: { host: ghcr.io, namespace: "" }
promotion:
  enabled: false     # Flight releases pin the Flight repository; requires pinDigest
  pinDigest: false   # write release.image.digest + .source; needs airframe-application v0.3.166+ everywhere
```

Never in `cicd.yaml` or Tower settings. With the defaults, every release renders exactly as before.

| Setting | Effect |
|---|---|
| `registries.ground` | Where builds push (onboarding's `image-repo`) and the preview (`-pr`) repository. glidepath-app reads its own `registries.ground`, empty = the same default. |
| `promotion.pinDigest` | `resolve-image-ref` reads the digest from the registry (`image-digest` result); `deploy-manifests` and `open-release-pr` write `digest` and `source`. Off: they delete both, so a release file never keeps an old digest under a new tag. `deploy-manifests` then waits for the Rollout to run `repo:tag@digest`, the string the chart renders. |
| `promotion.enabled` | `open-release-pr` pins `<flight host>/<flight namespace or owner>/<app>`. The render fails without `pinDigest`. |

## The promoter

| Piece | Where | Does |
|---|---|---|
| `promote` Pipeline + Task | glidepath-catalog, run in `platform-catalog` as `glidepath-promoter` | reads the PR and release file, applies the rules, opens and concludes the `Pipelines as Code CI / image-promotion-` check run |
| `promote-image` StepAction | glidepath-catalog | starts the workflow, finds its run by request id, waits, verifies Flight (digest, `.sig`, `.att`, and a promotion attestation for this request signed by the workflow identity) |
| `/promoter-token` | the broker (`cdevents-broker-auth`, `promoter.go`) | for `glidepath-promoter` only: `dispatch` = the promoter App, `actions: write` on the promoter repo; `release-pr` = the platform App, `checks: write` + read on a `gitops-<app>` repo |
| `promote.yaml` workflow | the promoter repo (lab: `riley-roooo/flight-promoter`) | checks the source is signed and attested, copies by digest with every referrer, keeps Flight's own referrers, signs the promotion attestation |

The check run carries the same name and App as a Pipelines as Code gate, so the registry, `detect-bypass-merge` and
the PR's gate table read it unchanged. Phase 3 adds the registry entry and the fan-in that starts the Pipeline.

Configuration:

```yaml
# glidepath-catalog
registries:
  flight: { host: ghcr.io, namespace: riley-roooo }
promotion:
  promoter: { repo: riley-roooo/flight-promoter, workflow: promote.yaml, ref: main, timeoutMinutes: 20 }
# glidepath-control-plane
promoter:
  enabled: true
  dispatchRepo: riley-roooo/flight-promoter
```

Secrets, in the platform's Infisical project (never in values): `flight-promoter-app-id` and
`flight-promoter-private-key` (the promoter App; synced into `platform-system` for the broker only) and
`flight-registry-read-dockerconfigjson` (a `read:packages` credential for Flight; synced into `platform-catalog`).

### Promote by hand (phase 2)

```sh
kubectl -n platform-catalog create -f - <<'YAML'
apiVersion: tekton.dev/v1
kind: PipelineRun
metadata: { generateName: promote- }
spec:
  pipelineRef: { name: promote }
  taskRunTemplate: { serviceAccountName: glidepath-promoter }
  params:
    - { name: repo-url, value: https://github.com/<owner>/gitops-<app> }
    - { name: pr-number, value: "<n>" }
YAML
```

The PR gets an `image-promotion-` check: green with the Flight ref, or red with the reason (for a red workflow run,
the name of the step that failed and the run's URL). Re-running is safe: the copy is idempotent and each run adds
its own attestation (PROM-07).

## Release file

```yaml
release:
  image:
    repository: ghcr.io/jfillman/boarding-api   # what the cluster pulls (Flight, once promotion is on)
    tag: 2.0.5-832e475                           # for people and Tower
    digest: sha256:53de…                         # what runs
    source: ghcr.io/jfillman/boarding-api        # Ground: where the gates read evidence
```

## Turning it on

1. airframe-application v0.3.166+ as every cluster's `defaultChart` (older charts reject `digest`/`source` and the
   Argo CD sync fails), and Tower/Backstage releases that parse `repo:tag@digest` (tower#143, backstage#198).
2. `promotion.pinDigest: true`. Check one Ground deploy and one release PR: the release file has both keys, the
   Rollout's image ends in `@sha256:`, every gate is green.
3. The promoter: the Infisical secrets above, `promoter.enabled` in the control plane, `promotion.promoter.repo` and
   `registries.flight.namespace` in the catalog. Promote one open release PR by hand.
4. `promotion.enabled: true` only with the fan-in (phase 3) in place: before that a Flight release would pin an image
   nobody copied.

## Files

- `charts/glidepath-catalog/values.yaml` `registries`, `promotion`
- `charts/glidepath-catalog/templates/_helpers.tpl` `glidepath-catalog.releaseImagePin`
- `templates/tasks/resolve-image-ref.yaml` (`image-digest`), `open-release-pr.yaml`, `deploy-manifests.yaml`,
  `deliver-onboarding-files.yaml`; `templates/stepactions/resolve-promoted-image.yaml` (reads `source`)
- `charts/glidepath-app/templates/argocd/ephemeral-envs.yaml` (preview repository)
- `charts/glidepath-catalog/tests/registries_test.sh`
- `templates/pipelines/promote.yaml`, `templates/tasks/promote.yaml`, `templates/stepactions/promote-image.yaml`,
  `templates/rbac/promoter.yaml`, `_helpers.tpl` `glidepath-catalog.promoterGitHub`; `tests/promote_test.sh`
- `glidepath/broker/cmd/token-review-interceptor/promoter.go`;
  `charts/glidepath-control-plane/templates/secretstore/flight-promoter-external-secrets.yaml`
