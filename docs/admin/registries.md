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
| `promotion.pinDigest` | `resolve-image-ref` reads the digest from the registry (`image-digest` result); `deploy-manifests` and `open-release-pr` write `digest` and `source`. Off: they delete both, so a release file never keeps an old digest under a new tag. |
| `promotion.enabled` | `open-release-pr` pins `<flight host>/<flight namespace or owner>/<app>`. The render fails without `pinDigest`. |

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
3. `promotion.enabled: true` only with the promoter (phase 2) and its check (phase 3) in place: before that a Flight
   release would pin an image nobody copied.

## Files

- `charts/glidepath-catalog/values.yaml` `registries`, `promotion`
- `charts/glidepath-catalog/templates/_helpers.tpl` `glidepath-catalog.releaseImagePin`
- `templates/tasks/resolve-image-ref.yaml` (`image-digest`), `open-release-pr.yaml`, `deploy-manifests.yaml`,
  `deliver-onboarding-files.yaml`; `templates/stepactions/resolve-promoted-image.yaml` (reads `source`)
- `charts/glidepath-app/templates/argocd/ephemeral-envs.yaml` (preview repository)
- `charts/glidepath-catalog/tests/registries_test.sh`
