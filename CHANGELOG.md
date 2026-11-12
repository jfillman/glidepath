# Changelog

Notable changes to Glidepath. Newest first.

## Unreleased

### A-11: the `propose` scope class (contract version 2)

- `agentScope` gains **`propose`**: an agent may add or change a Flight environment, the release step into it and a new
  environment's `cluster` and `production`, but only in a pull request a person must merge. Rules may set `proposeOn`
  (`added`, `changed`, `removed`): any other kind of change to the field is `human`, so a new environment's cluster is
  propose and an existing one's still is not. Secrets, signers, the gates, `deploy.target`, `deploy.chart`, the cloud blocks,
  `api`, `gitopsRepo` and `manifestPath` stay `human`. Docs: contract.md, "propose".
- `tools/cicd_scope.py`: `needs_person(base, head, scope)` lists what is propose; `scope_problems` no longer refuses a Flight
  environment added or changed (GP-SCOPE-003 is now removing one, or any tier change). `glidepath validate`, the validator
  sidecar and `POST /v1/apps/{app}/cicd/validations` answer `needsPerson` (always present); `changes[].agentScope` can read
  `propose`. The `cicd-validate-` check passes such a change and says "needs a person to merge".
- **`contractVersion` is 2.** Roll out Clearance, then Autopilot (its bundle names version 2), then this.
- Catalog: a Clearance `agent-scope` check that is neutral and titled `needs a person: ...` is not success to any reader
  (neutralOkJq), so it holds a release until Clearance posts success (a person merged) or failure (a bot did).
  `detect-bypass-merge` reads it as success only when the PR's merger is a person (account type User, not a `[bot]` login, not in
  `agentGates.agentLogins`), and reports a bypass otherwise.

### C-7: the agent gates move to Clearance's GitHub App (opt-in per cluster)

- New catalog values `agentGates.clearanceChecks` (default `false`) and `agentGates.clearanceAppId`. With
  `agentGates.clearanceUrl` set and `clearanceChecks: true`, the release guardrail reads the Clearance GitHub App's check
  runs `Clearance / agent-identity` and `Clearance / agent-scope` instead of Glidepath's `agent-gates-` check. The
  registry has two new entries (`agent-identity`, `agent-scope`, class integrity, required) and every consumer (the
  ruleset scripts, the promotion fan-in, PROM-03, the guardrail wait, `detect-bypass-merge`, the release log and the
  release PR's governance table) matches them by their exact names. A neutral conclusion (Clearance's `unattested`) counts
  as passing.
- Registry entries may now carry `check` (the exact check-run name another App posts), `integration` and
  `clearanceChecks` (present only when the Clearance checks are on, or off).
- Onboarding stops delivering `.tekton/pull-request-agent-gates.yaml` to app and gitops repos when the switch is on, and
  removes the copy it delivered earlier (the resync PR deletes it).
- The fan-in waits up to two minutes for a check another App reports, which has no finally task to ask again when it lands.
- Without Clearance (empty `clearanceUrl`), or with `clearanceChecks` off, nothing changes: `agent-gates-` stays the
  guardrail and still fails PRs that look like an agent's.
- `hack/apply-guardrail-ruleset.sh` and `hack/apply-agent-gates-ruleset.sh` build the Clearance checks under the App's id
  (`-f <values>`, `--clearance-app-id`) and refuse without it.
- Migration steps and the rollout order: docs/admin/agent-gates.md, "Migration".
