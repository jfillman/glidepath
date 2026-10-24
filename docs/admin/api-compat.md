# The `api-compat` release check

Every release PR gets an `api-compat` check that compares the API the release ships with the API running in the
environment it is going to. It reports breaking changes, says which ones touch operations that are actually in use,
and lets an app owner accept a breaking change on the record. It is **advisory** today: it reports and never blocks.

## What it compares

- **The candidate**: the API spec signed onto the promoted image at build time (`cicd.yaml` `api.spec`, default
  `openapi.yaml`; see the [cicd.yaml reference](../user/cicd-yaml-reference.md), "The `api:` block").
- **The baseline**: the spec signed onto the image the target environment runs now, which is the image the PR's
  release file names on the PR's base commit.

Both are read from the images' `https://hangar.io/attestations/api-spec/v1` attestations and **verified first**: a
sigstore bundle signed by this app's own `pipeline-runner` (`app-<app>-cicd` or `infra-<app>-cicd`) under the
platform's Fulcio, with a Rekor entry. An attestation that fails verification is ignored and the comment says so; it
is never read. The diff is [oasdiff](https://github.com/oasdiff/oasdiff) (in the toolbox image): its `error` and
`warning` findings are breaking, its `info` findings are counted as non-breaking.

oasdiff follows the spec's own declarations. Removing a response property the spec marks optional is not breaking to
it; mark the fields clients rely on `required` if their removal should count.

## Verdicts

| Verdict | Meaning |
|---|---|
| `pass` | No breaking change. |
| `warn` | Breaking changes, reported only. |
| `block` | Breaking changes, at least one to an operation in use, no owner override, and blocking switched on (below). The only verdict that fails the check. |
| `overridden` | Breaking changes accepted by an app owner. |
| `no-spec` | The promoted image carries no (verified) spec. A spec is optional; nothing is wrong. |
| `no-baseline` | Nothing to compare with: the first release to this environment, or its current image has no spec. |
| `unsupported` | An AsyncAPI spec. Diffing AsyncAPI is not supported yet. |

When the check itself cannot run (no PR, GitHub or the registry unreachable, oasdiff failing) its outcome is `error`
and the check run fails like any gate's; while the gate is advisory that failure does not block either.

The PR comment shows a table of breaking changes (operation, change, level, in use) and ends with one hidden line,
`<!-- glidepath:api-compat {json} -->`, which Tower reads (and `release-log-emit` copies the verdict and counts into
the release log as `api_compat_*`). Its fields: `version`, `verdict`, `env`, `candidate` and `baseline`
(`image`, `digest`, `specSha256`, `title`, `version`, or null), `breaking[]` (`id`, `level`, `text`,
`operation {method, path}`, `inUse`, `rate` per minute), `breakingTotal`, `nonBreakingCount`,
`usage {source, window, callers}`, `override {by, at, reason}` or null, and `notes`. A very long list is cut to keep
the comment under GitHub's size limit; `breakingTotal` keeps the real count.

## "In use"

An operation is in use when the target environment's service (`<app>-<env>`) served it in the last
`apiCompat.usageWindow` (default `7d`), from the OTel collector's span metrics
(`traces_span_metrics_calls_total`, server spans). Callers come from the trace service graph. Limits:

- **Operations, not fields.** Traces show that `GET /api/flights/{flightNumber}` was called, not which response
  fields the caller read. Field-level impact needs consumer contract tests.
- **Routes only where the agent knows them.** A span named only `GET`, or a wildcard route like `GET /**`, makes
  that operation's usage unknown rather than "not in use".
- **Sparse traffic.** No calls in a quiet week is weak evidence.
- **Reachability.** The check queries `apiCompat.usage.<cluster>.url` from the control-plane cluster's pipeline pods,
  with no credentials. A cluster without an entry, or a failed query, is usage `unknown`, and **unknown never
  blocks**. In the lab, hangar-prod's Prometheus is ClusterIP-only and not reachable from hangar-dev, so releases
  to prod report usage `unknown` until a reachable endpoint exists (a query frontend exposed to the control plane).

## Blocking (off by default)

Two switches, both needed:

1. The gate is required on the cluster: `releaseGuardrailOverrides: { api-compat: { required: true } }` in that
   cluster's glidepath-catalog values ([release-guardrails.md](release-guardrails.md)); re-run the ruleset script.
2. `apiCompat.blockWhenInUse: true` in the same values.

Then a breaking change to an operation **observed in use** with no override is `block`. Breaking changes to
operations not in use, or with unknown usage, stay `warn`. An eligible rollback is always advisory.

## The owner override

A PR comment starting `/api-break-ok <reason>` accepts the breaking changes, when written:

- after the promoted commit (an older one does not carry over to a new image), and
- by a GitHub user whose role on the app's source repository (`github.com/<owner>/<app>`) is **admin** or
  **maintain**, checked live through the GitHub App on every run.

Anyone else's command is ignored and listed in the comment. Then comment `/retest api-compat-` to re-run the check.
The newest valid override wins; who, when and the reason (the rest of the comment's first line) are in the comment,
and the hidden JSON line Tower reads.
