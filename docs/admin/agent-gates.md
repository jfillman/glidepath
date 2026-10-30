# The agent gates: agent-identity and agent-scope

Hangar's agents open pull requests only through Clearance, as the agents' GitHub App (`hangar-autopilot-agents`,
Hangar D4). The agent gates check every such PR against Clearance's own record of opening it, independently of
the agent and of whatever the PR says about itself.

| Gate | Passes when | Fails with |
|---|---|---|
| **agent-identity** | Clearance holds a receipt for this repo and PR, the PR's head is the commit the agent made, and any `Hangar-*` commit trailers match the receipt | AI001: trailers name a task, but Clearance never opened this PR. AI002: authored by the agents' App, but Clearance never opened it. AI003: commits were added after the agent's. AI004: the trailers were edited |
| **agent-scope** | every changed path is one the agent's commit carried, and the agent's definition still admits the repo and every path (its deny list plus Clearance's baseline denies: `.tekton/`, `CODEOWNERS`, `.github/`, AppProjects, release files) | AS001: a path the agent's commit did not carry. AS002: a path the definition denies. AS003: the repo left the definition's `repos.allow`. AS004: the definition is gone |

A person's PR (no receipt, no trailers, not by the agents' App) passes both as **not an agent PR**. Clearance
decides every verdict and records it in its evidence chain (`provenance.verify`). The gate only gathers what GitHub
says about the PR: the head commit, the changed paths (with a rename's old name), the author, and the head commit's
trailers.

Field-level scope (Clearance's R020, and R021 for `cicd.yaml`) was decided on the content when the agent opened the
PR. The receipt's commit being the PR's head is what carries that decision into the gate, which is why a PR with
commits added after the agent's (AI003) is not vouched for at all.

## How it runs

- One PaC check, **`agent-gates-`**, on every PR to `main` of every onboarded app repo and gitops repo
  (`.tekton/pull-request-agent-gates.yaml`, delivered by onboarding whatever `cicd.yaml` says). It has to run on a
  person's PR too: a required check that ran only for agents would block every other PR.
- Pipeline `agent-gates-check`, Task `agent-gates` (`platform-catalog`). The verify step reads the PR through the
  platform's GitHub App token (the broker, as every gate does) and posts to Clearance's `POST /v1/provenance` with a
  projected service-account token for audience `clearance` (`agentGates.audience`). The report step prints both
  verdicts, with each rule, violation and fix hint, and exits 1 if either fails.
- The PR comment (`comment-pr-check-result`) is posted only on an agent's PR. The finally task runs on every PR
  with `post-comment` set from the `agent-pr` result, so the verdict still reaches the promotion fan-in.
- **A release guardrail** (2026-10-30). `agent-gates` is a `releaseGuardrails` entry (class integrity, required), so
  every consumer of the registry includes it: the ruleset script, the promotion fan-in and PROM-03, the guardrail
  wait, the bypass detectors, the release log and the release PR's governance table. A release PR is `not-agent`
  and passes; an agent's propose-only upper PR (`repo.pr.open_upper`) is judged like any agent PR. Because it reads
  Clearance, a Clearance outage holds releases until Clearance is back, unless a cluster makes it advisory
  (`releaseGuardrailOverrides: {agent-gates: {required: false}}`).
- **Without Clearance.** If `agentGates.clearanceUrl` is empty (a cluster with no Clearance) or Clearance doesn't
  answer, a PR that looks like an agent's (authored by an `agentGates.agentLogins` login, or carrying `Hangar-*`
  trailers) fails. Any other PR passes without a verdict, and nothing is recorded.

## Configuration

`agentGates` in the glidepath-catalog values, set per cluster in the cluster repo's `50-glidepath/glidepath-catalog`:

```yaml
agentGates:
  clearanceUrl: http://clearance.autopilot-system.svc.cluster.local:8080
  audience: clearance
  agentLogins: ["hangar-autopilot-agents[bot]"]
```

Clearance has to accept the gate as a verifier. The Autopilot gateway's `CLEARANCE_VERIFIERS` must match the
pipeline's service account (`app-*-cicd/pipeline-runner`), and Clearance's ingress NetworkPolicy must admit pods
labelled `hangar.io/clearance-client: agent-gates` (apron `55-autopilot/clearance`).

## Making it block

A check stops a merge only as a required status check: `Pipelines as Code CI / agent-gates-` (integration `4461953`,
the PaC App). For gitops repos it is in the [guardrail ruleset](github-ruleset.md), which
`hack/apply-guardrail-ruleset.sh` builds from `releaseGuardrails`, so re-applying it adds the check. App repos get
their own ruleset with this one check: `hack/apply-agent-gates-ruleset.sh <owner>/<repo>`.

Rulesets on private repos need a paid GitHub plan; on GitHub Free the API answers 403 (every Hangar lab repo,
2026-10-30). There a red `agent-gates-` does not stop the merge button. On gitops repos the detective control still
applies: `detect-bypass-merge` reads the same registry, so a release merged past a red `agent-gates-` is recorded as a
bypass (the release log's `bypass` flag and the break-glass alert). App repos have no detective check for it.

## Verify it

1. An agent PR (e.g. from the triage agent) shows `agent-gates-` green, and the log shows `agent-identity: PASS
   [confirmed]` and `agent-scope: PASS [in-scope]`.
2. A person's PR shows `agent-gates-` green, with `not-agent` in the log.
3. A person's PR whose commit carries a made-up `Hangar-Task:` trailer fails with AI001.
4. A commit pushed onto an agent's branch fails with AI003. If the push adds a path, that path shows as AS001.
5. Clearance's evidence has one `provenance.verify` record per check, naming the gate's service account.
