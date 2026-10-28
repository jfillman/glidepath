# AGENTS.md: Glidepath

Glidepath is Hangar's CI/CD engine: Tekton and Pipelines-as-Code, driven by one file per app,
`cicd.yaml`. This file says what is true **today**; items marked *(planned)* do not exist yet.

## Start here
1. [`llms.txt`](llms.txt): what to read first, in order.
2. `contract/glidepath-contract.json`: every `cicd.yaml` field with its type, default, description and
   `agentScope`; the release gates; the run and release states; the known failures. Generated; never
   edit it. How it's built: [docs/admin/contract.md](docs/admin/contract.md).
3. `schemas/cicd.schema.json`: the schema the pipeline validates `cicd.yaml` against.
4. [docs/user/cicd-yaml-reference.md](docs/user/cicd-yaml-reference.md) and
   [docs/user/examples/](docs/user/examples/): every field explained, and complete files.
5. The Glidepath API, `api/openapi.yaml` ([docs/admin/api.md](docs/admin/api.md)): an app's runs, step logs,
   why a run failed (`explain`: a knownFailures id, its hint and the field to change), release records, an
   image's signatures and attestations re-verified (`evidence`), and test runs with their JUnit reports, read
   only. *(planned)* re-runs, previews and `cicd.yaml` pull requests (phase 3), promotion and
   rollback (phase 4).

## Changing an app's cicd.yaml
- Open a pull request. Never push to a default branch.
- Run `tools/glidepath-validate cicd.yaml --agent --base-ref origin/main` (from this repo, pointed at
  the app's file) before you open it. Fix every error; read every warning.
- Change only fields whose `agentScope` is `open`. A `tighten` field may only move the strict way
  (turn a gate on, never off). A `human` field you leave as it is, and you ask a human.
- Never add, remove or change a Flight environment, or change any environment's tier.
- Never remove a check-ride step, or a required test step, or set `required: false` on one.
- Never touch `.tekton/`. Glidepath generates it from `cicd.yaml`.
- Read the failure, change the file, retry at most three times, then stop and report.

## Changing this repo
- `schemas/cicd.schema.json` changed: rebuild the toolbox image and bump `toolboxImage` in
  `charts/glidepath-catalog/values.yaml` (the image bakes the schema in), then regenerate the contract.
- `schemas/cicd.meta.yaml`, `releaseGuardrails` or `api/openapi.yaml` changed: run
  `python3 tools/gen_contract.py` and commit the regenerated contract. CI runs it with `--check`.
- `tools/cicd_scope.py` changed: copy it byte for byte to `autopilot/src/clearance/vendor/cicd_scope.py`.
- Run `python3 tools/test_glidepath_validate.py` for any change under `tools/` or `schemas/`.
- Render every Helm template you touch with `helm template` before you commit.
- Work in a git worktree; never commit on someone else's checkout.

## Verify
A green pipeline is not verification; the deployed service is. For a release, the release record
(`release-tracking-<chain-id>` ConfigMap in `app-<name>-cicd`) reaching `healthy` is the signal; its
states are in the contract's `states.release`.

## Do not
- Edit `contract/glidepath-contract.json` by hand.
- Turn off or loosen a gate to make a pipeline pass. If a gate is wrong, say so in the pull request.
- Put a secret value anywhere in `cicd.yaml`. `secrets:` names Infisical keys, and it is human-owned.
