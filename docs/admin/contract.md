# The contract, glidepath validate, and what an agent may change

Glidepath's contract is one generated file, [`contract/glidepath-contract.json`](../../contract/glidepath-contract.json),
that says everything a client needs to use Glidepath without reading its source: every `cicd.yaml`
field and who may change it, the release gates, the fixed state vocabularies, and the failures
Glidepath knows by name. People, Tower and agents read the same file. It is the first phase of the
[Glidepath API spec](https://hangarplatform.dev/library/glidepath-api/), and it follows the pattern
Airframe's contract set.

## What's in it

| Section | What it says | Source |
|---|---|---|
| `cicd.fields` | Every `cicd.yaml` field: type, enum, default, description, plus `owner`, `risk` and `agentScope` | [`schemas/cicd.schema.json`](../../schemas/cicd.schema.json) + [`schemas/cicd.meta.yaml`](../../schemas/cicd.meta.yaml) |
| `scope` | The rules an agent's change is checked against (below) | `schemas/cicd.meta.yaml` |
| `gates` | Each release gate: what it checks, `required` or advisory, `real` or `stub` | `releaseGuardrails` in [`charts/glidepath-catalog/values.yaml`](../../charts/glidepath-catalog/values.yaml) ([release-guardrails.md](release-guardrails.md)) |
| `states` | The run states and the release states, with their meanings | `schemas/cicd.meta.yaml` ([release-state.md](release-state.md)) |
| `knownFailures` | Failures with a stable id, the log line they match, a hint and, when there is one, the field to change | `schemas/cicd.meta.yaml` |
| `api` | Every API operation and its tier, once the API exists | `api/openapi.yaml` |

Regenerate it after changing any source, and commit the result:

```shell
python3 tools/gen_contract.py          # write it
python3 tools/gen_contract.py --check  # what CI runs (contract-ci.yaml)
```

The generator fails if a schema field matches no pattern in `cicd.meta.yaml`, so a new field can't
ship until someone has decided whether an agent may touch it.

`cicd.meta.yaml` is not read by any pipeline. The pipeline still validates `cicd.yaml` against the
schema baked into the toolbox image, so a change to the sidecar needs no toolbox rebuild. A change
to `cicd.schema.json` still does.

## glidepath validate

`tools/glidepath-validate` checks a `cicd.yaml` before it reaches a pipeline, in one pass:

```shell
tools/glidepath-validate cicd.yaml
tools/glidepath-validate cicd.yaml --format json
tools/glidepath-validate cicd.yaml --agent --base-ref origin/main   # also the GP-SCOPE rules
```

| Rule | Severity | What it catches |
|---|---|---|
| `GP-YAML-001` | error | Not YAML, or not a mapping |
| `GP-SCHEMA-001` | error | An unknown key, with the closest real one |
| `GP-SCHEMA-002` | error | A wrong type or value |
| `GP-ENV-001` | warning | An environment named like a pipeline stage |
| `GP-ENV-002` | error | Two environments with the same name |
| `GP-ENV-003` | error | A Ground environment sets `cluster` |
| `GP-ENV-004` | error | `production: true` on a Ground environment |
| `GP-ENV-005` | error | `deploy.lowerEnvironments`, `upperEnvironments` or `promotionOrder`, retired by `deploy.environments` |
| `GP-PIPE-001` | error | A deploy or release step names an environment `deploy.environments` doesn't list |
| `GP-PIPE-002` | error | A release step names a Ground environment |
| `GP-PIPE-003` | error | A flow has more than one check-ride step |
| `GP-PIPE-004` | error | A check-ride step sets `testName` |
| `GP-CHECKS-001` | error | A checks-only file (no `build`) sets `pipelines`, `test`, `deploy`, `api` or `ephemeralEnvironments` |

The `GP-ENV`, `GP-PIPE` and `GP-CHECKS` rules are constraints the pipeline already enforces when it runs
([cicd-yaml-reference.md](../user/cicd-yaml-reference.md)). Here they fail in seconds instead of
partway through a flow. Exit 0 means no errors, 1 means at least one, 2 means bad usage. It needs
python3, pyyaml and jsonschema.

## What an agent may change

An agent changes `cicd.yaml` only through a pull request that a human merges. On top of that,
every field has an `agentScope`:

| Scope | Meaning | Fields |
|---|---|---|
| `open` | An agent may change it | `build`, `test`, `ephemeralEnvironments`, `notifications`, `deploy.strategy`, Ground entries in `deploy.environments`, `pipelines` |
| `tighten` | Only in the stricter direction | `governance.sast`, `imageScan`, `policyCheck`, `sbom` (off to on), `governance.githubMerge.mode` (`""` < `merger` < `strict`), `githubMerge.requiredApprovals` (up), a test step's `required` (false to true) |
| `human` | Never in an agent's PR | `secrets`, `governance.allowedCommitSigners`, `deploy.target`, `deploy.releaseFile`, `deploy.chart`, the cloud blocks (`ecs`, `lambda`, `azureContainerApps`, globally or per environment), an environment's `cluster` and `production`, `api`, a step's `cluster`, `gitopsRepo` and `manifestPath` |

The rule behind the split is that an agent never edits a control that constrains it. Secrets,
signers, cloud deploy identity and the gates are those controls. A build flag isn't.

Three rules check an agent's change against the file it started from:

| Rule | What it refuses |
|---|---|
| `GP-SCOPE-001` | A `human` field added, removed or changed |
| `GP-SCOPE-002` | A `tighten` field moved the loose way (an unset field counts as its default), or a flow that lost its check-ride step or one of its required test steps |
| `GP-SCOPE-003` | A Flight environment added, removed or changed, or any environment's tier changed |

Environments are compared by name, so reordering them is not a change. Pipeline steps are matched
by content first, so inserting a step doesn't make every later step look changed.

The rules are [`tools/cicd_scope.py`](../../tools/cicd_scope.py), standard library only.
Clearance vendors that file byte for byte and runs it before an agent opens a pull request, so the gate
the agent meets and the check in CI are the same code. When you change it, copy it to
`clearance/src/clearance/packs/hangar/vendor/cicd_scope.py` (with the contract); a drift test there fails
until you do.

The same rules answer `POST /v1/apps/{app}/cicd/validations` on glidepath-api
([api.md](api.md#validate-a-cicdyaml-change)): a caller sends a patch or the whole file and gets the
verdict the check will give, plus every field the change touches, before opening the pull request.

### Enforced in app pull requests: the `cicd-validate` check

Since 2026-10-31 every onboarded app repo runs `glidepath validate` as a pull-request check,
**`Pipelines as Code CI / cicd-validate-`** (`.tekton/pull-request-cicd-validate.yaml`, delivered by
onboarding like the agent gates' file; Pipeline `cicd-validate-check`, Task `cicd-validate`). Before it,
a broken `cicd.yaml` was only caught by `stage-preflight`, on the first build after the merge.

- It runs on every PR to `main`, a person's too, because a required check that only ran when
  `cicd.yaml` changed would block every other PR. A PR that leaves `cicd.yaml` alone passes in seconds
  as "unchanged", with no comment.
- When the PR changes `cicd.yaml`, the check reads the file at the PR head through the platform's
  GitHub App token and runs the schema, `GP-ENV` and `GP-PIPE` rules. It fails on any error and posts
  the findings as a PR comment, each with its rule, field and fix.
- An agent's PR (authored by an `agentGates.agentLogins` login, or carrying `Hangar-*` commit
  trailers) also meets the `GP-SCOPE` rules, against the file at the PR's merge base (not the base
  branch's tip, so `main` moving on is not read as the agent's change). A PR that adds the first
  `cicd.yaml` is compared with an empty file, so every field counts as added. Whether the PR really is
  the agent's is the [agent gates'](agent-gates.md) question; this check applies the stricter rules to
  either claim.
- A PR that removes `cicd.yaml` fails: every app repo needs one at its root.

The validator runs from the `glidepath-validate` image (`tools/validate.Containerfile`: the validator,
`cicd_scope.py`, the schema and the contract of one commit; `validate-image-ci` publishes
`ghcr.io/<owner>/glidepath-validate:sha-<short>` on every change and sets `cicdValidatorImage` in the
catalog values). The toolbox image stays bash-only. The check blocks a merge only as a required status
check: `hack/apply-agent-gates-ruleset.sh <owner>/<repo>` adds it to the app repo's ruleset, once the
repo has the `.tekton` file (a required check nothing reports would block every PR).

Clearance still runs the scope rules before an agent opens the PR (R021), so the agent learns of a
refusal before anything exists on GitHub; the check is what holds whoever opened the PR, however they
opened it.
