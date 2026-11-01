# Ground and Flight registries

Builds push to the **Ground** registry. Flight environments run images from the **Flight** registry, which only the
promoter writes. A release file pins the image by digest and records where it was built. Decision record:
[ADR-0029](adr/0029-ground-and-flight-registries.md).

## What it guarantees

- **An upper cluster runs only promoted images** (admission, phase 4; enforced on hangar-prod since 2026-10-30; see
  [Admission](#admission-phase-4)): a Flight-prefix rule plus a promotion attestation signed by the promoter
  workflow, verified offline by Kyverno.
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
- **Flight to Flight is still promoted from Ground.** A release from one Flight environment to another (staging to
  prod) starts from the Flight copy the source environment runs; `open-release-pr` records the app's Ground repository
  as its `release.image.source`, since the digest is the same, so PROM-06 holds and the gates read the evidence where
  it was made. The copy into Flight finds the digest already there.
- **Promotion starts itself** (phase 3): the moment the last required gate turns green on the release PR's head
  commit, with no polling pod, exactly once per commit and attempt. The PR gets the `image-promotion-` check and a
  comment like every other gate's, and with promotion on `image-promotion` is a required gate: a merge before it is
  green is reported as a bypass.

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
| `promote` Pipeline + Task | glidepath-catalog, run in `platform-catalog` as `glidepath-promoter` | reads the PR and release file, applies the rules, opens and concludes the `Glidepath / image-promotion-` check run |
| `promote-image` StepAction | glidepath-catalog | starts the workflow, finds its run by request id, waits, verifies Flight (digest, `.sig`, `.att`, and a promotion attestation for this request signed by the workflow identity) |
| `/promoter-token` | the broker (`cdevents-broker-auth`, `promoter.go`) | for `glidepath-promoter` only: `dispatch` = the promoter App, `actions: write` on the promoter repo; `release-pr` = the platform App, `checks: write` + `pull_requests: write` (the PR comment) + `contents: read` on a `gitops-<app>` repo |
| `promote.yaml` workflow | the promoter repo (lab: `riley-roooo/flight-promoter`) | checks the source is signed and attested, copies by digest with every referrer, keeps Flight's own referrers, signs the promotion attestation |

The check run carries the same name and App as a Pipelines as Code gate, so the registry, `detect-bypass-merge` and
the PR's gate table read it unchanged. The report step also leaves the gate comment (marker
`<!-- glidepath:gate:image-promotion -->`, edited in place on the next attempt): what was promoted, or why not and how
to retry.

## The fan-in (phase 3)

```
gate PipelineRun (app -cicd ns)            broker                      platform-catalog
  finally: comment-pr-check-result
    comment         -> PR comment, verdict
    promotion-ready -> gate ledger in the release record
                    -> all required green?  --promotion-ready-->  Trigger `promote`  -->  PipelineRun promote-<id>
```

| Piece | Where | Does |
|---|---|---|
| `promotion-ready` step | `comment-pr-check-result` (every gate's `finally`), rendered only with `promotion.enabled` | records this gate's verdict for the head commit in the release record (`gate.<sha12>.<gate>`, written with the record's `resourceVersion`); when every required gate is green, sends `dev.hangar.release.promotion-ready.0.1.0` |
| `promote` Trigger | glidepath-catalog `rbac/promote-trigger.yaml`, `platform-catalog` | accepts the event only from `<type>-<app>-cicd` for `gitops-<app>`, and creates `promote-<event id>` as `glidepath-promoter` |
| `glidepath-promote-trigger` | ServiceAccount, `platform-catalog` | creates PipelineRuns there and nothing else; the broker may impersonate exactly it |
| `image-promotion` registry entry | `glidepath-catalog.guardrails`, only with `promotion.enabled` | required, `class: integrity`, `targets: [k8s-rollout]`; read by `detect-bypass-merge`, `release-log-emit` and the PR body's gate table. Nothing delivers a `.tekton` file for it |

Why it works without a polling pod or a lock:

- **This gate's own verdict counts.** A gate's check run is still in progress while its `finally` runs, so the step
  uses its own verdict for itself, and `promote` waits (up to 5 minutes, `GATE_WAIT_SECONDS`) for a required gate that
  is queued or in progress. A red or missing gate refuses at once.
- **Two gates finishing together.** Each would see the other's check run in progress. The ledger write carries the
  release record's `resourceVersion`, so the two writes are serialized and the second sees both verdicts. A verdict in
  the ledger outranks the check run (a `/retest` that went red is red). With no release record (a PR opened by hand)
  the check runs alone decide.
- **A green run that will end timed out is not green.** Tekton counts `finally` against `timeouts.tasks`
  ([release-guardrails.md](release-guardrails.md#timeouts-always-split-tasksfinally-never-a-single-pipeline-budget-alone)),
  so a run whose tasks succeeded can still end `PipelineRunTimeout` with a red check run. The step reads its own
  PipelineRun: past `startTime + timeouts.tasks` it records a failure; within 60 seconds of it, it records nothing
  and does not ask (the other gates then read this gate's check run, and `/retest <gate>-` asks again). Before
  2026-10-29 such a run recorded success and started a promotion that PROM-03 refused (gitops-backstage#159).
- **Exactly once.** The event id is a hash of repository, PR, head commit and the number of earlier
  `image-promotion-` check runs on it; the PipelineRun is named after it, so two gates that both see the set complete
  start one run. After a refused or failed attempt, the next gate to finish asks again under a new id.
- **Skipped:** a stale commit (the PR moved on), a merged or closed PR, a branch that is not `release-*`, a PR that
  changes no `<cluster>/<env>/release.yaml` (cloud pin PRs copy at deploy), a commit already promoted or promoting.
- **A gate advisory for this app only** (catalog `releaseGuardrailOverridesByApp`,
  [release-guardrails.md](release-guardrails.md#one-app-only-releaseguardrailoverridesbyapp-2026-10-28)) is not part
  of "every required gate" for that app, here or in `promote`'s PROM-03: its check still runs and can be red, and the
  release is promoted anyway. When it is the last gate to finish, red, it still asks. Every other app still waits for
  it.
- **A lost event** is recovered by the next gate to finish: comment `/retest <gate>-` on any gate. `promote` re-checks
  every rule itself, so an event sent by hand gets nothing the rules forbid.

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
`flight-registry-read-dockerconfigjson` (a `read:packages` credential for Flight; synced into `platform-catalog` as
`flight-registry-read`, which the `flight-prune` CronJob and glidepath-api's evidence reads of a Flight image use:
an app's `registry-credentials` holds the Ground owner's credential, which Flight refuses; [api.md](api.md#evidence)).

### Promote by hand

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
4. `promotion.enabled: true` (glidepath with the phase 3 fan-in). From then on a release PR pins the Flight
   repository, `image-promotion` is a required gate, and the fan-in promotes when the last required gate turns green.
   A release PR opened before the switch still pins Ground and is refused (PROM-01): open it again (Tower Promote).
5. Admission on each upper cluster, in Audit (Apron `admission.flight.enabled`, with `registries.flight` and
   `promotion.promoter.repo` in that cluster's `cluster.yaml`; see below). Check that a new pod of a promoted app
   gets no warning and a Ground image does.
6. Once every app's prod release runs from Flight: `admission.flight.enforce` (Deny, fail closed) and
   `admission.flight.pullFromFlight` (the tenant pull secret reads Flight; the Ground credential leaves the cluster).
   Before that, a restart of an app still on Ground would be refused, or could not pull.
7. Remove every write path to Flight but the workflow's own `GITHUB_TOKEN`: no other collaborator with write access
   on the promoter repository, no `write:packages` token of the Flight owner anywhere. (Lab: the Ground owner's
   write access to the promoter repository was removed 2026-10-30.)

## Admission (phase 4)

Apron renders two Kyverno policies on an upper cluster (`30-policy/kyverno-policies/flight-images.yaml`), both on
Pod CREATE in tenant namespaces (`hangar.io/managed-secrets`):

- `flight-only-images` (ValidatingPolicy): every container and init container image starts with the Flight prefix or
  one of `admission.flight.allowedPrefixes`, third-party images no Glidepath build produces (CloudNativePG, the OTel
  auto-instrumentation init image, bitnamilegacy Redis). The message names the images that are not promoted.
- `flight-images` (ImageValidatingPolicy, Flight images only): the `https://hangar.io/attestations/promotion/v1`
  attestation verifies keyless against the promoter workflow (`<promoter repo>/.github/workflows/promote.yaml` on
  `main`, GitHub Actions OIDC, public Sigstore), offline from the bundle's inclusion proof. It reads Flight with
  `kyverno/flight-registry-read` (Infisical `flight-registry-read-dockerconfigjson`, read-only).

They are two policies because an ImageValidatingPolicy fetches every image it matches with its own credential: a
Ground image read with the Flight credential is an evaluation error, not a violation. Chains' build provenance is
not checked at admission: it carries no inclusion proof and lives in the hub's Rekor, which an upper cluster cannot
reach; the gates verified it before the promotion the attestation records. Audit mode is `[Audit, Warn]` with
`failurePolicy: Ignore`; the upper clusters run no Kyverno reports controller, so the evidence is the warning
`kubectl` prints (a server-side dry run of a pod shows it without creating anything). Enforce is `[Deny]` with
`failurePolicy: Fail`: a refused pod's error names the images, and while Kyverno's admission controller is down no new
tenant pod starts.

In the lab: hangar-prod in Audit 2026-10-29, `pullFromFlight` 2026-10-30 (gitops-hangar-prod#20: its tenant
`registry-credentials` is the Flight read credential) and enforce the same day (gitops-hangar-prod#21). Before
enforce, a server-side dry run of every running tenant pod raised no warning; after it, a Ground image is denied and
the promoted Flight image admitted.

## Cloud targets (phase 6)

A cloud deploy (`deploy.target` `aws-ecs`, `aws-lambda`, `azure-container-apps`) copies the build from Ground into
the target's own registry at deploy time; nothing promotes it. With `promotion.enabled`, `resolve-deploy-target`
therefore refuses a cloud target in a **Flight** environment (PROM-10) before any deploy Task runs, with the reason.
Ground environments of the same app deploy as before. Cloud promotion (pin PRs through the fan-in, ECR copied from
Flight) is a follow-up for when a real cloud Flight environment exists; no app has one today.

## Retention (phase 6)

Flight only grows unless `promotion.retention.enabled`. The platform plans, the promoter deletes:

- The `flight-prune` CronJob (catalog namespace, runs as `glidepath-promoter`) lists the Flight packages with the
  read-only `flight-registry-read` credential and, for each package with a `<gitopsOwner>/gitops-<package>` repo,
  builds a keep list: every tag and digest a `<cluster>/<env>/release.yaml` on the default branch pins, every
  Release Record (`releases/*.yaml`) names, and every open release PR pins (PROM-09). Any failed read skips the
  package; a package with no gitops repo or an empty list is skipped.
- It dispatches the `prune` workflow (`promotion.retention.workflow`) in `promotion.promoter.repo` once per package
  and waits for it. The workflow's `GITHUB_TOKEN` deletes; nothing in a cluster can.
- The workflow keeps the `keepNewest` newest releases and anything younger than `minAgeDays` whatever the list says,
  deletes at most `maxDeletes` releases per run, refuses an empty keep list, and with `dryRun` (the default) only
  writes the plan to the run summary. Deleting a release removes, by digest, its index, platform manifests,
  `.sig`/`.att`/`.sbom` tags, referrers fallback index and the referrers it lists, unless a kept release reaches the
  same digest.

Not yet verified live: the GitHub packages API with the read credential, and version deletion with the workflow's
`GITHUB_TOKEN` (GitHub allows it for packages linked to the workflow's repository). Run it in `dryRun` first.

## Files

- `charts/glidepath-catalog/values.yaml` `registries`, `promotion`
- `charts/glidepath-catalog/templates/_helpers.tpl` `glidepath-catalog.releaseImagePin`
- `templates/tasks/resolve-image-ref.yaml` (`image-digest`), `open-release-pr.yaml`, `deploy-manifests.yaml`,
  `deliver-onboarding-files.yaml`; `templates/stepactions/resolve-promoted-image.yaml` (reads `source`)
- `charts/glidepath-app/templates/argocd/ephemeral-envs.yaml` (preview repository)
- `charts/glidepath-catalog/tests/registries_test.sh`
- `templates/pipelines/promote.yaml`, `templates/tasks/promote.yaml`, `templates/stepactions/promote-image.yaml`,
  `templates/rbac/promoter.yaml`, `_helpers.tpl` `glidepath-catalog.promoterGitHub`; `tests/promote_test.sh`
- `templates/tasks/comment-pr-check-result.yaml` (`promotion-ready`), `templates/rbac/promote-trigger.yaml`,
  `_helpers.tpl` `glidepath-catalog.guardrails` (the `image-promotion` entry); `tests/promotion_fan_in_test.sh`
- `glidepath/broker/cmd/token-review-interceptor/promoter.go`;
  `charts/glidepath-control-plane/templates/secretstore/flight-promoter-external-secrets.yaml`
