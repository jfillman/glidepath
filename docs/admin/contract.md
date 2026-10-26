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

The `GP-ENV` and `GP-PIPE` rules are constraints the pipeline already enforces when it runs
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
Autopilot's Clearance vendors that file byte for byte and runs it before an agent opens a pull
request, so the gate the agent meets and the check in CI are the same code. When you change it,
copy it to `autopilot/src/clearance/vendor/cicd_scope.py`; a drift test there fails until you do.

### Not enforced in app pull requests yet

`glidepath validate --agent` is not wired into the pipeline's own pull-request checks yet. That check
has to know which pull requests an agent opened, and the GitHub identity agents will use isn't
decided (the spec's open decision). Until then the scope is enforced by Clearance before the pull
request exists, and a human merges every pull request either way.
