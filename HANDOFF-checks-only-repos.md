# Handoff: checks-only repos in Glidepath

Written 2026-10-31 for an agent picking this up cold. Read this file, then `AGENTS.md`, then the files it names.

## The goal

Let Glidepath onboard a repository that **builds nothing and deploys nothing, but runs PR checks**. Today every
onboarded repo must declare `build` in `cicd.yaml`, so a repo that only holds configuration cannot be onboarded at
all. The first user is Airframe's coming `AgentRepository` kind: a team's repo of prompt-only Autopilot agent
definitions (`agents/<name>.yaml`). Those repos need Glidepath's PR checks and the agent release, and nothing else.

Done means: a repo whose `cicd.yaml` declares no build is onboarded end to end on hangar-dev, receives exactly the
checks every repo gets (listed below), and a definition change on it runs `agent-definitions-` on the PR and
`agent-release` on the merge.

## Decided (by the owner, 2026-10-31). Do not reopen.

- **Airframe's `AgentRepository` composes Airframe's `TektonCICD` child.** TektonCICD is Glidepath's *registration*
  of a repo (it writes `tenants/<app>/identity.yaml` in the control-plane tenants repo; that creates the repo's
  `app-<name>-cicd` namespace, its Pipelines-as-Code Repository and RBAC). It is not a build. Without registration no
  check can run.
- **Product independence.** Glidepath knows nothing about agents, Autopilot or Clearance beyond its existing optional
  integrations (`agentGates.clearanceUrl`). "Checks-only" is a generic Glidepath concept: any configuration-only repo
  can use it. Do not name anything after agents.
- **Glidepath decides what runs on a repo; Airframe only writes `cicd.yaml` and the registration.**

## The contract Airframe will write (build against this)

`AgentRepository` (being built in parallel in the airframe repo) scaffolds this `cicd.yaml` and nothing else
Glidepath reads:

```yaml
apiVersion: platform/v1
kind: PipelineConfig
# No build: a checks-only repository. Glidepath runs its PR checks and nothing else.
```

If you need an explicit marker instead of "no `build` key", choose it, document it in the contract, and say so in
your PR. The Airframe side will follow the contract you publish.

## What exists, and what to change

The good news: Glidepath already builds nothing by default.
`charts/glidepath-catalog/templates/tasks/deliver-onboarding-files.yaml` (around line 386) delivers only
`onboarding-resync.yaml` unconditionally, plus the checks every repo gets, and generates `.tekton/flow-*.yaml` from
`cicd.yaml`'s `pipelines:` section. No `pipelines:` means no build flow. So the work is mostly making `build` optional
and making everything that reads it tolerate its absence.

Start with these:

| File | Today | What to do |
|---|---|---|
| `schemas/cicd.schema.json` | `"required": ["apiVersion", "kind", "build"]` | make `build` optional; when it is absent, refuse `pipelines`, `deploy`, `test`, `api`, `ephemeralEnvironments` (a checks-only repo has nothing for them). JSON Schema `if/then` or a cross-field rule in the validator, whichever this repo already prefers |
| `schemas/cicd.meta.yaml` | per-field owner and risk | add whatever new field you introduce, if any |
| `tools/glidepath-validate`, `tools/test_glidepath_validate.py` | validator and its tests | a checks-only fixture passes; a checks-only file with `pipelines:` fails with a clear rule and hint |
| `charts/glidepath-app/templates/env/build-cache-pvc.yaml` | reads `.Values.build.cache.size` | render nothing without `build` |
| `charts/glidepath-app/templates/triggers/flow-triggers.yaml` | reads `.Values.build.sourceVolume.size` | render nothing (no flows) without `build` |
| `charts/glidepath-catalog/templates/tasks/deliver-onboarding-files.yaml` | reads `.build.sourceVolume.size` (around line 142) and computes flows | tolerate no `build`: deliver the always-on files only |
| other readers of the schema | `build-image.yaml`, `stage-preflight.yaml`, `gitops-image-bump.yaml`, `charts/glidepath-app/tests/*`, `charts/glidepath-catalog/tests/validate_config_envs_test.py` | check each; most only run when a build exists |
| `docs/user/cicd-yaml-reference.md`, `docs/admin/onboarding-mechanics.md` | | document checks-only repos: what a repo gets, the `cicd.yaml` above |

Everything that reads the schema: `grep -rln cicd.schema.json --include=*.py --include=*.go --include=*.yaml .`

## What a checks-only repo must receive

The same always-on files every app repo gets from `deliver-onboarding-files.yaml`:

- `.tekton/onboarding-resync.yaml` (re-delivers on every `cicd.yaml` change)
- `.tekton/pull-request-agent-gates.yaml`
- `.tekton/pull-request-cicd-validate.yaml`
- `.tekton/pull-request-agent-definitions.yaml` and `.tekton/push-agent-release.yaml`: added by
  [glidepath#209](https://github.com/jfillman/glidepath/pull/209); see `docs/admin/agent-definitions.md` once it
  merges. Each runs only when `agents/` changes.

No `flow-*.yaml`, no release, no gitops repo, no environments, no build cache.

## Follow the repo's rules (`AGENTS.md`)

- A schema change means rebuilding the toolbox image and bumping `toolboxImage` in
  `charts/glidepath-catalog/values.yaml` (the image bakes the schema in), then `python3 tools/gen_contract.py`.
- `python3 tools/test_glidepath_validate.py` for any change under `tools/` or `schemas/`; `helm template` every chart
  you touch; the `catalog-ci` and `app-chart-ci` workflows must pass.
- If `tools/cicd_scope.py` changes, the Clearance repo vendors it byte for byte
  (`src/clearance/packs/hangar/vendor/cicd_scope.py`): open the matching Clearance PR. The same is true of
  `contract/glidepath-contract.json` (Clearance's `test_cicd_scope.py` fails on drift until it is re-vendored).
- Work in a git worktree. Use `git -C <path>` and `gh --repo jfillman/glidepath` in every command.
- You may merge your own PR once every check passes; never merge with a failing or pending check, and never chain a
  merge onto a check wait in one command.

## Verify live (hangar-dev)

A green pipeline is not verification. On hangar-dev:

1. Onboard a scratch checks-only repo. The Airframe `AgentRepository` may not exist yet; register by hand the way
   TektonCICD does: commit `tenants/<name>/identity.yaml` to `jfillman/gitops-hangar-dev-tenants` (copy the shape
   from `tenants/gate-api/identity.yaml`) and give the repo the checks-only `cicd.yaml`.
2. Confirm the `app-<name>-cicd` namespace and the PaC Repository exist, the resync PR delivers exactly the always-on
   files, and no build or release Applications or flows appear.
3. Open a PR adding `agents/<some-agent>.yaml` (copy `agents/gate-reader.yaml` from `jfillman/gate-api`, renaming it).
   `agent-definitions-` runs and reports a tier. Merge it: `agent-release` runs. Whether the broker can open the pin
   PR depends on the platform App's access to the tenants repo; report what you see either way.
4. Delete the scratch repo's registration afterwards. Never delete anything else.

## Coordinate

- The Airframe side (`AgentRepository` XRD and composition, composing TektonCICD, scaffolding `agents/`,
  `preflight/cases/`, a README and the `cicd.yaml` above) is being built separately. Do not edit the airframe repo.
- Publish the contract in your PR description: exactly what a checks-only `cicd.yaml` looks like, and the validator
  rule ids it can fail with.
- Open questions to answer in the PR rather than guess silently:
  - Does a checks-only repo need `governance` or `notifications`?
  - Should `cicd-validate-` treat "adding `build` later" as a normal upgrade path?
