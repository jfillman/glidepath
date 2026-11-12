# The agent gates: agent-identity and agent-scope

Hangar's agents open pull requests only through Clearance, as the agents' GitHub App (`hangar-autopilot-agents`,
Hangar D4). The agent gates check every such PR against Clearance's own record of opening it, independently of
the agent and of whatever the PR says about itself.

| Gate | Passes when | Fails with |
|---|---|---|
| **agent-identity** | Clearance holds a receipt for this repo and PR, the PR's head is the commit the agent made, and any `Hangar-*` commit trailers match the receipt | AI001: trailers name a task, but Clearance never opened this PR. AI002: authored by the agents' App, but Clearance never opened it. AI003: commits were added after the agent's. AI004: the trailers were edited |
| **agent-scope** | every changed path is one the agent's commit carried, and the agent's definition still admits the repo and every path (its deny list plus Clearance's baseline denies: `.tekton/`, `CODEOWNERS`, `.github/`, AppProjects, release files) | AS001: a path the agent's commit did not carry. AS002: a path the definition denies. AS003: the repo left the definition's `repos.allow`. AS004: the definition is gone |

A person's PR (no receipt, no trailers, not by the agents' App) passes both as **not an agent PR**. Clearance
decides every verdict and records it in its evidence chain (`provenance.verify`). Clearance labels a person's PR that
carries AI co-author trailers `unattested`: a warning on the check run (neutral), never a failure, and Glidepath's
consumers read it as passing.

Field-level scope (Clearance's R020, and R021 for `cicd.yaml`) was decided on the content when the agent opened the
PR. The receipt's commit being the PR's head is what carries that decision into the gate, which is why a PR with
commits added after the agent's (AI003) is not vouched for at all.

## Two ways to run it

Glidepath supports both; a cluster is in exactly one (`glidepath-catalog.clearanceChecks` in `_helpers.tpl`).

| | **With Clearance's GitHub App** (C-7) | **Without it** (the fallback) |
|---|---|---|
| Chosen when | `agentGates.clearanceUrl` is set **and** `agentGates.clearanceChecks: true` | `clearanceUrl` is empty, or `clearanceChecks` is off (the default) |
| What posts the verdict | the Clearance GitHub App, installed on the repo: check runs **`Clearance / agent-identity`** and **`Clearance / agent-scope`** (Clearance `internal/githubapp`, `docs/provenance-app.md`) | Glidepath's `agent-gates-` PaC check: Pipeline `agent-gates-check`, Task `agent-gates` |
| Registry entries | `agent-identity`, `agent-scope` (class integrity, required) | `agent-gates` (class integrity, required) |
| Delivered to repos | nothing: no `.tekton/pull-request-agent-gates.yaml` (onboarding removes it) | `.tekton/pull-request-agent-gates.yaml` in every app and gitops repo |
| Glidepath's token for Clearance | none | a projected service-account token, audience `agentGates.audience` |
| Re-run | the check run's "Re-run" button (Clearance re-verifies) | `/retest agent-gates-` |

### With Clearance's App

The Clearance GitHub App gathers what GitHub says about the PR itself (the head commit, the changed paths with a
rename's old name, the author, the head commit's trailers) with an installation token scoped to that repo, calls the same
`provenance.verify`, and posts the two check runs on the PR's head. A person's PR passes both in seconds. Nothing of
Glidepath runs for them, so Glidepath needs no Clearance token for the agent gates and no NetworkPolicy exception.

The check names are the registry's `check` field. Every consumer matches them **exactly** (they are not
`<pipelinesAsCode.applicationName> / <gate>-`), and every ruleset requires them under the App's id:

- **the release guardrail** is the two entries above: `releaseGuardrails` in the catalog values. The promotion fan-in,
  `promote`'s PROM-03, `wait-for-release-guardrails`, `detect-bypass-merge`, the release log and the release PR's
  governance table all range over the effective registry, so they follow the new names with no edits of their own.
  The release log's flat gate keys become `gate_agent_identity` and `gate_agent_scope`; `gate_agent_gates` is no longer
  written on such a cluster (a dashboard column for it is empty from the cutover).
- **a check another App reports has no finally task here**, so nothing asks again when it lands: the promotion fan-in
  waits up to two minutes for a missing or running one before giving up on that commit, and PROM-03 waits on a running one.
  A check the App never posts (it is not installed on that repo) reads as `missing` and holds the release, so **install
  the App on every onboarded app and gitops repo before turning the switch on**.
- **advisory per cluster** keeps its semantics: `releaseGuardrailOverrides: {agent-scope: {required: false}}` (and per
  app, `releaseGuardrailOverridesByApp`) are the keys now. An override of the old name `agent-gates` names an entry that
  is not in the registry on such a cluster and does nothing.
- **a conclusion of `neutral`** on these checks (Clearance's `unattested` warning) counts as success, as GitHub's own
  required-check evaluation treats it.
- **the old check's names.** On commits from before the cutover the old check is `Glidepath / agent-gates-` or, from before
  the PaC application rename, `Pipelines as Code CI / agent-gates-` (`legacyApplicationNames`). Those readers keep
  matching either name for the `agent-gates` entry on a cluster still in the fallback; with Clearance's App on, the old
  check is no longer in the registry and an old red one on an open PR is ignored.

### Without Clearance's App (the without-Clearance guardrail)

One PaC check, **`agent-gates-`**, on every PR to `main` of every onboarded app repo and gitops repo
(`.tekton/pull-request-agent-gates.yaml`, delivered by onboarding whatever `cicd.yaml` says). It has to run on a person's
PR too: a required check that ran only for agents would block every other PR.

- Pipeline `agent-gates-check`, Task `agent-gates` (`platform-catalog`). The verify step reads the PR through the
  platform's GitHub App token (the broker, as every gate does) and posts to Clearance's `POST /v1/provenance` with a
  projected service-account token for audience `agentGates.audience`. The report step prints both verdicts, with each
  rule, violation and fix hint, and exits 1 if either fails.
- The PR comment (`comment-pr-check-result`) is posted only on an agent's PR. The finally task runs on every PR with
  `post-comment` set from the `agent-pr` result, so the verdict still reaches the promotion fan-in.
- **If `agentGates.clearanceUrl` is empty** (a cluster with no Clearance) or Clearance doesn't answer, a PR that looks
  like an agent's (authored by an `agentGates.agentLogins` login, or carrying `Hangar-*` trailers) fails. Any other PR
  passes without a verdict, and nothing is recorded. This is the guardrail a cluster without Clearance keeps.
- It is a release guardrail like the others (`agent-gates`, class integrity, required). Because it reads Clearance, a
  Clearance outage holds releases until Clearance is back, unless a cluster makes it advisory
  (`releaseGuardrailOverrides: {agent-gates: {required: false}}`).

## Configuration

`agentGates` in the glidepath-catalog values, set per cluster in the cluster repo's `50-glidepath/glidepath-catalog`:

```yaml
agentGates:
  clearanceUrl: http://clearance-core.<clearance namespace>.svc.cluster.local:8080   # the cluster's Clearance core (apron 55-clearance)
  audience: clearance
  agentLogins: ["hangar-autopilot-agents[bot]"]
  clearanceChecks: true        # C-7: the Clearance App reports the agent checks; false keeps agent-gates-
  clearanceAppId: "<id>"       # the Clearance GitHub App's id, for the rulesets' integration_id
```

Verifiers are Clearance's own: its core decides who may call `POST /v1/provenance` (`clearance.verifiers` in apron's
`55-clearance`; R-10 moved that from the Autopilot gateway's `CLEARANCE_VERIFIERS` to the core). Clearance's ingress
NetworkPolicy admits the Glidepath gate pods by the label `hangar.io/clearance-client`, one value per gate (`agent-gates`,
`clearance-gate`, `agent-definitions`, `agent-release`). `clearanceUrl` is still needed with the App on: the deployment gate,
the agent-definitions check and the agent release use it too.

## Making it block

A check stops a merge only as a required status check. For gitops repos the checks are in the [guardrail
ruleset](github-ruleset.md), which `hack/apply-guardrail-ruleset.sh -f <cluster values>` builds from `releaseGuardrails`
(the Clearance entries under the App's id, the Glidepath ones under the PaC App's `4461953`), so re-applying it swaps the
check. App repos get their own ruleset, "Glidepath App Checks", with the agent checks and `cicd-validate-`
([contract.md](contract.md)): `hack/apply-agent-gates-ruleset.sh -f <cluster values> <owner>/<repo>`.

Rulesets on private repos need a paid GitHub plan; on GitHub Free the API answers 403 (every Hangar lab repo,
2026-10-30). There a red agent check does not stop the merge button. On gitops repos the detective control still
applies: `detect-bypass-merge` reads the same registry, so a release merged past a red agent check is recorded as a
bypass (the release log's `bypass` flag and the break-glass alert). App repos have no detective check for it.

## Migration

Turning `clearanceChecks` on for a cluster that runs `agent-gates-` today. Order matters: a required check nothing
reports blocks every PR.

1. **Install the Clearance GitHub App** on every onboarded app repo and gitops repo, and confirm Clearance posts
   `Clearance / agent-identity` and `Clearance / agent-scope` on a recent PR beside `agent-gates-` (both report during the
   overlap, so verdicts can be compared).
2. **Set `agentGates.clearanceAppId`** and, in the same cluster-values change, `agentGates.clearanceChecks: true`
   (`clearanceUrl` is already set). The catalog then renders the new registry; onboarding starts opening resync PRs.
   Do not merge the cluster values until step 3's ruleset is ready to apply.
3. **Swap the rulesets first**: `hack/apply-guardrail-ruleset.sh -f <values> <owner>/gitops-<app>...` and
   `hack/apply-agent-gates-ruleset.sh -f <values> <owner>/<app>...`. They require the Clearance checks and drop
   `Glidepath / agent-gates-`. Applying them before the file is removed means the old check is never required while it
   disappears; applying them while the old check still runs is harmless.
4. **Merge the onboarding resync PRs** (app repos: `.tekton/pull-request-agent-gates.yaml` deleted; gitops repos the
   same). The Task deletes only a file carrying its own `# BOILERPLATE` header, so a tenant's own copy is left alone.
   For a repo that has not resynced yet, delete `.tekton/pull-request-agent-gates.yaml` by hand; there is no separate
   script, because the resync is this repo's mechanism for removing onboarded files (the same one that retired the
   `image-promotion` stub, ADR-0025).
5. **Apron** (rollout, a separate PR): `templates/50-glidepath/glidepath-catalog/application.yaml` gains
   `clearanceChecks: true` and `clearanceAppId` under `agentGates` (the same change as step 2). Once no repo runs the
   `agent-gates-` check, `templates/55-clearance/clearance/application.yaml` drops `agent-gates` from the
   `hangar.io/clearance-client` values of the core's ingress rule. The pipeline account stays in `clearance.verifiers`
   (the deployment gate and agent release still call Clearance as that account); only the `agent-gates` label value goes.
   Keep it while any cluster or repo still runs the check.
6. Rolling back is `clearanceChecks: false` plus re-applying the rulesets; the next resync redelivers the files.

A cluster that has no Clearance changes nothing: it keeps `agent-gates-` and its rulesets as before.

## Verify it

1. An agent PR (e.g. from the triage agent) shows `Clearance / agent-identity` and `Clearance / agent-scope` green
   (`agent-gates-` green on a fallback cluster, the log showing `agent-identity: PASS [confirmed]` and
   `agent-scope: PASS [in-scope]`).
2. A person's PR shows both green, `not-agent`.
3. A person's PR whose commit carries a made-up `Hangar-Task:` trailer fails identity with AI001.
4. A commit pushed onto an agent's branch fails identity with AI003. If the push adds a path, that path shows as AS001.
5. Clearance's evidence has one `provenance.verify` record per verification (principal `app:<installation>` for the
   App's checks, the pipeline's service account for the fallback).
6. A release PR's governance table lists `agent-identity` and `agent-scope` (or `agent-gates`), and
   `detect-bypass-merge` reports no bypass when both are green.
