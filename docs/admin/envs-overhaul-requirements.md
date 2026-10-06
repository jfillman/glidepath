# Environments overhaul: requirements and plan

**Status: plan approved, decisions recorded (2026-10-04).** Nothing here is built. It collects what the
current model gets wrong, what the overhaul must do, a proposed order of work, and the
decisions still open. The proposed decisions are recorded as a draft in
[ADR-0019](adr/0019-environments-as-one-config-model.md).

Scope, as decided with the owner: one environment model for every deploy target, gated
promotion for cloud targets, the `platform/` to `glidepath/` folder move and a
bring-your-own chart field (both already designed in
[ADR-0018](adr/0018-glidepath-owns-envs-folder-and-chart-contract.md) and
[chart-contract.md](chart-contract.md)), no Kubernetes scaffolding for cloud apps, and a
single editing experience in Tower for Ground and Flight environments alike.

Vocabulary used here: **Ground** environment = a lower environment, deployed
automatically on push. **Flight** environment = an upper environment, deployed only
through a release (a PR plus guardrails).

## 1. What an "environment" is today

There is no environment object. An environment is whatever several independent things
happen to agree on:

| Where | What it says | Read by |
|---|---|---|
| `deploy.lowerEnvironments[]` in `cicd.yaml` | the Ground env names | `deploy-rbac` (pipeline-runner access), flow validation, Tower |
| `deploy.upperEnvironments[]` (name, or `{name, cluster}`) | the Flight env names and their clusters | the AppProject, the release flow, Tower |
| `deploy.promotionOrder[]` | the order a release walks | Tower's Releases tab, release guardrails |
| `pipelines.*.steps[].env` | which env a deploy/release/test step targets | `validateFlows`, the triggers |
| `platform/envs/<env>.yaml` exists (`envName:` inside) | a Ground env exists *on Kubernetes* | the lower-envs ApplicationSet (a git-files generator) |
| `gitops-<app>/<cluster>/<env>/values.yaml` + `release.yaml` | a Flight env exists | the per-cluster ArgoCD, Tower's App Configuration tab |
| Deploy runs naming an `env` | a *cloud* env exists | Tower only (derived, added 2026-10) |

`deploy.target` (`k8s-rollout`, `aws-ecs`, `aws-lambda`, `azure-container-apps`) is one
value per app, with one config block per target.

## 2. Problems, with the evidence

**P1. Five places must agree, and nothing checks all of them.** A deploy step's `env` must
be in `lowerEnvironments` or `upperEnvironments` (`validateFlows`), but nothing ties either
list to the env files, to `promotionOrder`, or to what Tower shows. Real consequences:
a declared env rendered as a second, never-deployed card next to the working one on
smoke-fn's Overview (fixed as a symptom in tower#8); the Glidepath tab rebuilt `deploy`
from its own fields and silently dropped `target` and the `lambda` block (tower#8); a
`cicd.yaml` with `lowerEnvironments: []` and a deploy step for `dev` stops the app's chart
from rendering at all.

**P2. Cloud apps get Kubernetes artifacts they cannot use.** smoke-fn is a Lambda
function, yet onboarding scaffolded `platform/envs/dev.yaml` for it and the lower-envs
ApplicationSet generated the Application `smoke-fn-dev`, which renders
`charts/airframe-application` into `app-smoke-fn-dev` (a Namespace, ServiceAccount and
NetworkPolicy). The validator also forces a cloud app to declare its env in
`lowerEnvironments`, which is what creates the namespace RBAC. chart-contract.md already
says this must not happen ("proposed: gate the generator on `deploy.target`").

**P3. A cloud app can deploy to exactly one resource.** `deploy.lambda` has one
`functionName`/`region`, `deploy.ecs` one cluster/service, `deploy.azureContainerApps` one
app. The `deploy-lambda` task never reads the step's `env`. Declaring `test` and `prod`
just deploys to the same function twice, and Tower shows the same image in both.

**P4. Cloud environments cannot be gated.** Flight means "release PR in `gitops-<app>`".
Functions have no gitops repo (airframe#47 removed the URL it never had), so there is no
way to put a cloud `prod` behind an approval. Every cloud env is effectively Ground.

**P5. Two different editing experiences.**
- Flight env: App Configuration tab. A real form, schema-validated against the chart's
  `values.schema.json`, an env banner, one PR per save.
- Ground env: Glidepath tab. A comma-separated text field for the env list, a raw-YAML
  editor for `platform/envs/<env>.yaml`, and a text box to name a new env. I found no
  delete flow for a Ground env there.
- Adding an environment is therefore several separate edits (the list, the order, the
  file, the pipeline step) in different places, none of which knows about the others.

**P6. The folder and the chart are fixed.** `platform/` is still the folder name (rename
decided in ADR-0018, not done) and both ApplicationSets hardcode `charts/airframe-application`
at a pinned tag, so Glidepath cannot yet deploy any other chart.

**P7. Config-only pushes are not validated** (known-gaps #5): a hand edit of an env file
on `main` is checked by nothing until the next build.

## 3. Requirements

### Model
- **R1.** Environments are defined once, in `cicd.yaml` under `deploy.environments[]`.
  Each entry is an object: `name`, `tier` (`ground` or `flight`), and where relevant
  `cluster`, `target` and target config, and `approval`.
- **R2.** The order of the list is the promotion order. `promotionOrder` goes away in the
  new shape.
- **R3.** Pipeline steps reference an environment by name. A step naming an undefined
  environment fails validation with a message that says which list to add it to.
- **R4.** `deploy.target` stays the app default; an environment may override it. The
  per-target config (`lambda`, `ecs`, `azureContainerApps`) stays as the app default and an
  environment may override fields (function name, region, ECS service, Container App).
- **R5.** Everything an environment needs on a cluster (folder file, namespace, RBAC,
  Application, gitops directory) is derived from its definition and exists only for the
  `k8s-rollout` target. A cloud environment creates none of it.
- **R6.** The chart is per app (and optionally per environment) through `deploy.chart`,
  per chart-contract.md. Unset means the reference chart at today's pin.
- **R7.** Ephemeral PR-preview environments stay outside `environments[]` (they are
  generated per pull request from `pr-env.yaml`, ADR-0012).

### Promotion
- **R8.** A Ground environment deploys automatically on push, as today.
- **R9.** A Flight environment never deploys without an explicit approval. For
  Kubernetes this is the existing release PR and guardrails, unchanged.
- **R10.** Cloud Flight environments get an approval path that does not need a gitops
  repo (see Q1).
- **R11.** The same release artifact (one image digest) is what moves from env to env;
  a cloud env records what it last deployed, the way a release file does for Kubernetes.

### Compatibility and migration
- **R12.** Both shapes are read during a window. If `deploy.environments` is present it
  wins and the old fields are ignored with a warning; if absent, the old fields are
  mapped to the new model in memory. No app changes behavior until it opts in.
- **R13.** Glidepath provides the mapping and opens a migration PR per app (it can be
  generated mechanically: lower to ground, upper to flight, order from `promotionOrder`).
- **R14.** The folder move follows ADR-0018's dual-path window (both ApplicationSet
  generators list `platform/` and `glidepath/`; an `envName` never appears in both; the
  old path is dropped last).
- **R15.** The old shape is removed only after every onboarded app has migrated, verified
  by listing every `cicd.yaml`, not assumed.

### Safety
- **R16.** Every change to environment config goes through a PR; no direct commits (the
  current posture of both Tower tabs).
- **R17.** The schema is validated in the toolbox image, which bakes `cicd.schema.json`
  in: any schema change needs a toolbox rebuild and bump, and a check that proves it.
- **R18.** Deleting an environment shows its impact first (files, namespace, gitops
  directory, Applications, pipeline steps that reference it) and never deletes data
  silently. A past delete-policy mistake removed `cicd.yaml` in five repos, so deletion
  is always an explicit, previewed PR.
- **R19.** Tower editing must round-trip keys it does not know. Both the Glidepath tab
  (tower#8) and `build` have already lost fields to a form that rebuilt a section.
- **R20.** Config-only pushes to environment files get validated (closes P7).

### Tower (editing experience)
- **R21.** Adding, editing and deleting an environment is one flow, and Ground and Flight
  look and behave the same: the same picker, the same top-shelf form with schema
  validation, the same PR result dialog.
- **R22.** Adding an environment is one action that makes all the required changes
  (cicd.yaml entry, env file or gitops directory, pipeline step if wanted) and shows them
  before submitting.
- **R23.** The environment list shows tier, target (Kubernetes / Lambda / ECS / Container
  Apps), cluster, current health and the live image for each environment, in promotion order.
- **R24.** A cloud environment's form edits its target config (function name, region and
  so on); a Kubernetes environment's form edits chart values. Tower decides which from the
  environment's target.
- **R25.** Tower must never show an environment that is not in the model, and must show
  every environment that is (closes the ghost-env class of bug from P1).

## 4. Tower UI: options and recommendation

**Decision (2026-10-04): the table layout (option C below) with the pending-changes panel
from the stack layout.** The mockups are on the design canvas "Tower Environments Tab
Mockups" (boards C and D). Concretely:

- One **Environments** tab. A dense table, one row per environment in promotion order, with
  tier, target, where it runs, health, live image and last deploy. A row expands in place
  into the environment's form (Settings, Values or Target config, Promotion, Danger zone).
- Edits **stage** instead of submitting. A **Pending changes** panel beside the table lists
  every staged change (added, edited, removed environments), the files each touches, and
  exactly which pull requests will open and in what order, then one "Review and open PRs".
  This is also what resolves Q2: the panel shows the change set as a whole.
- Add and delete are the shared dialogs (board E): a file preview for add, an impact list
  and typed confirmation for delete.
- App Configuration stays as its own tab for non-environment config (Q7).

The three options below are kept as the record of what was considered.

The existing assets: ConfigTab already has the form, env banner, schema validation and PR
dialog, but only for Flight envs. The Glidepath tab has the cicd.yaml form and a raw-YAML
editor for Ground files.

**Option A: a new "Environments" tab (recommended).** One tab that owns environment
management; ConfigTab and the Glidepath tab stop editing env lists and env files.

```
Environments                                                [ + Add environment ]
┌────────────────────┬──────────────────────────────────────────────────────────┐
│ dev      Ground    │  prod   Flight · kind-prod · Kubernetes      ● Healthy   │
│  Kubernetes  ●     │  Image 1.4.0-ab12cd3   promoted 2d ago                   │
│ test     Ground    │ ┌────────┬──────────┬──────────┬───────────────────────┐  │
│  Kubernetes  ●     │ │Settings│ Values   │ Promotion│ Danger zone           │  │
│ prod     Flight    │ └────────┴──────────┴──────────┴───────────────────────┘  │
│  Kubernetes  ●     │  Values: the same top-shelf form for Ground and Flight,   │
│ staging  Flight    │  with "inherits base.yaml" shown per field for Ground.    │
│  Lambda      ●     │                                                          │
└────────────────────┴──────────────────────────────────────────────────────────┘
```
- Left rail: every environment in promotion order, with tier, target and health. Drag to
  reorder (edits list order).
- **Add environment** is a short dialog: name, tier, target (defaults to the app's),
  cluster (Kubernetes) or the target's fields (cloud), approval (Flight), position. It
  produces one change set and previews every file it will touch before opening the PR.
- **Settings** edits the environment's definition. **Values** is the shared top-shelf form
  (Kubernetes) or the target-config form (cloud). **Promotion** shows approval and
  guardrails. **Danger zone** is delete with the impact preview (R18).
- Pros: one mental model, one place, and it makes the "no environment is invisible"
  guarantee (R25) easy to test. Cons: a new tab, and App Configuration then has to either
  disappear or become a thin link, which is a navigation change users will notice.

**Option B: keep two tabs, share one environment picker and one form.** Smaller change.
Ground envs would get the form inside the Glidepath tab. Pros: no navigation change.
Cons: environment management stays split across two tabs, so adding an env still means
visiting both, which is the complaint.

**Option C: manage environments from the Overview rail** (a "+" ghost card, a menu per
card). Good for discoverability but a poor place for a form, so best as a *shortcut into*
Option A rather than a replacement.

**Recommendation: A, with C as an entry point.** Build it read-only first (list, health,
tier, target) so the model is visible before anything is editable.

**Where the values come from, so the form can serve both tiers.** A Ground env's values
are `glidepath/envs/<env>.yaml` layered over `base.yaml`, in the *source* repo. A Flight
env's are `gitops-<app>/<cluster>/<env>/values.yaml` plus `release.yaml`, in the *gitops*
repo. Both are values for the same chart and validate against the same
`values.schema.json`, so one form can edit either; only the target file and repo differ.
The consequence is that adding a Flight env is two PRs (source repo for `cicd.yaml`,
gitops repo for the directory) unless we decide otherwise (Q2).

## 5. Proposed order of work

Each phase is independently shippable and leaves existing apps unchanged.

| # | Phase | Notes and exit criterion |
|---|---|---|
| 0 | This doc and ADR-0019 | Reviewed and the open questions answered |
| 1 | Stop Kubernetes artifacts for cloud apps (R5 for existing shape) | Gate onboarding's env scaffold and the lower-envs ApplicationSet on `deploy.target`; relax `validateFlows` for cloud targets. Verify: a new function gets no `smoke-fn-dev`-style Application or namespace; an existing function's leftovers are listed for cleanup |
| 2 | `deploy.environments[]` in the schema, validator and in-memory mapping (R1-R4, R12) | Needs a toolbox rebuild and bump (R17). Verify with every real `cicd.yaml` in the org mapped and diffed against today's behavior |
| 3 | Tower: read-only Environments tab (R23, R25) | Built on the mapping; no editing yet |
| 4 | Per-env cloud config in the deploy tasks (R4) | `deploy-lambda`, `deploy-ecs`, `deploy-azure-container-apps` read the step's env. Verify with two Lambda functions |
| 5 | Tower: add/edit/delete flow and shared form (R21, R22, R24, R18, R19) | Includes the delete impact preview. Ground add = one `cicd.yaml` PR (resync scaffolds the file); Flight add = `cicd.yaml` PR plus the ApplicationEnvironment template launched through the scaffolder API. No backend change: the existing cicd.yaml change route takes a `deploy` patch |
| 6 | Cloud gated promotion (R9-R11) | Designed in [ADR-0020](adr/0020-cloud-gated-promotion-by-release-pin.md) (proposed); five slices, none built |
| 7 | Folder rename `platform/` to `glidepath/` (R14) | Per ADR-0018's dual-path window. Readers and writers are dual-path (rule in [chart-contract.md](chart-contract.md#dual-path-window-platform-to-glidepath)); app repos move one by one; dropping `platform/` is a later cleanup |
| 8 | `deploy.chart` (R6) | [ADR-0023](adr/0023-per-app-chart-deploy-chart.md): Ground ApplicationSet moves into `glidepath-app`; Flight chart recorded in `identity.yaml` |
| 9 | Migration PRs, then drop the old shape (R13, R15) | Only after every app is on the new shape |

Phases 1 and 3 are low risk and visible; 2 is the one that could surprise existing apps,
which is why it is read-only until the mapping has been checked against real repos.

## 6. Decisions and what is still open

Answered by the owner on 2026-10-04:

- **Q1 (cloud Flight approval): decided.** A PR on the *source* repo that changes a small
  per-env release pin (`glidepath/releases/<env>.yaml`), reusing the PR and guardrails
  model. No gitops repo.
- **Q4 (cloud credentials): decided.** One set per app for the first version. Per-env
  accounts are a later extension (they need per-env secrets).
- **Q5 (target change): decided.** An environment cannot change target. Delete and re-add,
  with the impact preview.
- **Q6 (naming): decided.** Keep "Ground" and "Flight".
- **Q7 (App Configuration): decided.** It stays as its own tab for non-environment config.
- **UI layout: decided.** Table with in-place expansion plus the pending-changes panel (§4).

- **Q2 (a Flight env touches two repos): decided, revised 2026-10-05.** The first answer
  (Glidepath's resync opens the gitops PR) rested on a wrong premise: the resync delivers only
  `.tekton/` governance files to the gitops repo and never creates an environment directory. A
  Flight environment is created by Airframe's `ApplicationEnvironment` XR, requested through the
  existing Backstage template (a request PR on the tenants repo; Crossplane then commits
  `<cluster>/<env>/values.yaml` into the gitops repo and the identity file the ApplicationSet reads).
  **Decision: Tower opens both PRs.** It opens the `cicd.yaml` PR on the source repo and launches that
  template (`applicationenvironments.catalog.hangar.io-v1alpha1`: `appName`, `env`, `cluster`,
  `appType`, `pushToGit`) through the scaffolder API, and the pending-changes panel shows both PRs
  and the order to merge them (the XR request first, so the environment exists before `cicd.yaml`
  names it). No new Glidepath or Airframe machinery.
  **Ground environments are different and the first answer holds for them:** adding one is a single
  Tower PR on `cicd.yaml`; the onboarding resync then opens a PR on the source repo that scaffolds
  `platform/envs/<env>.yaml`.
  Deleting a Flight environment is not designed yet. Removing the XR must not delete data (a past
  delete-policy mistake removed `cicd.yaml` in five repos), so the first version only edits
  `cicd.yaml` and tells the owner the XR is still there.

Proposed, awaiting confirmation (the owner asked how much complexity it adds, said they like
the idea, and has not confirmed the approach):

- **Q3 (Ground env on another cluster).** Proposed: the schema allows `cluster` on any
  environment from day one (no later breaking change); the validator rejects a Ground
  `cluster` that differs from the app's dev cluster until multi-cluster Ground is built as
  its own phase. That phase needs the cluster's own ArgoCD to generate the Application, the
  per-cluster RBAC, registry credentials and secrets store, health via the existing
  outcome-relay path, and Tower reading each cluster. The owner likes multiple dev clusters
  managed by one Glidepath, so this is wanted, just later.

## 7. Out of scope

Ephemeral PR environments (R7), `cicd.yaml`'s `apiVersion: platform/v1` (a separate
decision per ADR-0018), the Autopilot/AI workload path, and cloud-target health signals in
Tower (a separate item).
