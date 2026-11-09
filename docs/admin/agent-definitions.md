# Agent definitions: the PR check and the release

An Autopilot agent's definition (`agents/<name>.yaml`, Clearance's schema) lives in a repo Glidepath onboards: the
app it serves, a code agent's own repo, or a team's agents repo. Glidepath runs two things for it. Clearance decides
everything about agents; Glidepath gathers, asks, and opens the PR.

| | When | What |
|---|---|---|
| **`agent-definitions-`** check | a PR to `main` that changes `agents/*.yaml` | each changed definition, with its version on the PR's base, to Clearance's `POST /v1/definitions/review`: would Clearance load it, could its agent use what it lists (`DF-*` problems fail the check), and the **tier** of the change (reported) |
| **`preflight-`** check | the same PR | each changed definition **that has Preflight cases** runs as a budgeted candidate on Autopilot's gateway and is scored against them; a failing case fails the check |
| **`agent-release`** | a push to `main` that changes `agents/*.yaml` | per definition: Clearance's review again; the **pin PR** on the control-plane tenants repo through the broker (`POST /agent-pin`); the change registered with Clearance's deployment gate (`POST /v1/clearances` with `definition`) |

## The release, step by step

1. A definition change merges on `main`. Nothing is live yet: Clearance runs the pinned commit.
2. `agent-release` asks the broker for the pin PR. The broker, not the pipeline, writes it: the Airframe `Agent` XR at
   `tenants/<app>/xr-requests/<name>.yaml` naming this commit (or its removal), on a branch
   `agent-release/<app>-<name>-<sha7>`, with the platform App's token on the tenants repo only. The caller's app comes
   from its namespace, the source repo must be that app's own, and nothing about the file is the caller's to choose
   but the agent's name, path, commit, owner group and one line of description. No app pipeline holds a token for the
   tenants repo.
3. The change is registered with Clearance's deployment gate, tiered by what the definition change lets the agent do:
   **0** no change in authority, **1** narrows only (both cleared by policy at once), **2** spends or runs more, **3**
   touches more (the owner group, and for 3 an outside group, on the Clearances page). The PR body carries the tier and
   every change.
4. Merging the pin PR is the deploy. Clearance loads the commit within a minute (`definition.load` in its evidence).
   Rollback is a pin to an earlier commit, through the same flow.

Phase 1 is soft, like the deployment gate: the tier is recorded and shown, and merging a pin PR still holding for
people is visible in the record.

## Preflight

The `preflight-` check asks Autopilot's gateway (not Clearance, and not glidepath-api: the gateway owns agent runs
and the cases) to do three things per changed `agents/<name>.yaml`, and reports what comes back:

1. `GET /v1/preflight/cases?agent=<name>`: the agent's cases (`preflight/cases/` in the autopilot repo, shipped in
   the gateway image). None: the definition is reported, not failed.
2. `POST /v1/agents/<name>/runs` with `{input, document, budget}`, once per run a case reads: the PR's definition as a
   **candidate**, run as a clamped task (tier at most T1, no GitHub calls, capped time, calls and tokens; it is never
   in the live set). The gateway refuses a candidate that would not load, or whose caller may not manage the agent.
3. `POST /v1/preflight/score` until every run has finished: each case is scored deterministically (no judge model)
   and the definition's result (`pass`/`fail`, cases) lands in the gateway's ledger and so on that definition's run
   records while it is unchanged (`docs/admin/preflight.md` in the autopilot repo).

A failing case, a refused launch, or runs that do not finish within `agentPreflight.maxWaitSeconds` fail the check.
With no `agentPreflight.gatewayUrl` the check reports "not run" and passes. The pod authenticates with a projected token
(`agentPreflight.audience`); the gateway must list its service account in `CLEARANCE_DELEGATES` and
`agentPreflight.onBehalfOf` as a platform admin (`CLEARANCE_ADMINS`) or owner of the agents. Apron's gateway ingress
NetworkPolicy must admit pods labelled `hangar.io/autopilot-gateway-client: agent-preflight`.

## Files

- `charts/glidepath-catalog/templates/tasks/agent-definitions.yaml`, `pipelines/agent-definitions-check.yaml`
- `charts/glidepath-catalog/templates/tasks/agent-preflight.yaml`, `pipelines/agent-preflight-check.yaml`
- `charts/glidepath-catalog/templates/tasks/agent-release.yaml`, `pipelines/agent-release.yaml`
- `charts/glidepath-app/files/onboarding-templates/app-repo/pull-request-agent-definitions.yaml`, `push-agent-release.yaml`
  (every app repo; each runs only when `agents/` changes)
- the broker: `/agent-pin` (`glidepath/broker/cmd/token-review-interceptor/agentpin.go`), `AGENT_PIN_TENANTS_REPO` from
  `glidepath-control-plane` `tenantsRepoUrl`
- `glidepath-catalog` values: `agentRelease.cluster`, `agentPreflight.*`; Clearance's URL and audience are `agentGates`'
- Apron's Clearance NetworkPolicy admits `hangar.io/clearance-client` `agent-definitions` and `agent-release`
