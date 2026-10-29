# glidepath-api

`glidepath-api` answers questions about an app's CI/CD for people, Tower and agents: its runs,
their logs, why a run failed, its release records, the supply-chain evidence on its images, its test
runs, and Glidepath's [contract](contract.md); and it can [re-run and cancel](#writes) an app's
PipelineRuns. Phases 1 and 2 of the [Glidepath API spec](https://hangarplatform.dev/library/glidepath-api/)
are the reads (tier T0). Phase 3 has started with its first writes (tier T1): re-run and cancel. Previews
and `cicd.yaml` pull requests are still to come in phase 3, promotion and rollback in phase 4. Nothing
else is written.

The operations are [`api/openapi.yaml`](../../api/openapi.yaml). The contract lists every
operation with its tier, and [`tools/test_glidepath_validate.py`](../../tools/test_glidepath_validate.py)
fails if the server's routes and the spec ever differ.

| Call | Needs | Returns |
|---|---|---|
| `GET /v1/whoami` | a token | Who the API thinks you are |
| `GET /v1/contract` | a token | `contract/glidepath-contract.json` |
| `GET /v1/apps/{app}/runs?sinceHours=&limit=&view=` | `list runs` | Runs newest first; `view=full` adds each PipelineRun and TaskRuns |
| `GET /v1/apps/{app}/runs/{result}` | `get runs` | One run (the id is the PipelineRun's uid) |
| `GET /v1/apps/{app}/runs/{result}/logs/{taskRun}` | `get runs/logs` | One TaskRun's step logs, as text |
| `GET /v1/apps/{app}/runs/{result}/explain` | `get runs/logs` | Each failed TaskRun's failing step, its [known failure](#explain) id, hint and field, and the line that matched |
| `GET /v1/apps/{app}/releases` | `list releases` | Every `release-tracking-*` record of the app |
| `GET /v1/apps/{app}/releases/{chainId}` | `get releases` | One record |
| `GET /v1/apps/{app}/evidence?image=` | `get evidence` | Every signature and attestation on the image, each [re-verified](#evidence) |
| `GET /v1/apps/{app}/tests?limit=` | `list tests` | The app's [test runs](#tests) from Testkube, newest first |
| `GET /v1/apps/{app}/tests/{id}/report` | `get tests` | A run's JUnit report, as XML |
| `POST /v1/apps/{app}/pipelineruns/{name}/rerun` | `rerun runs` | [Re-run](#re-run) a PipelineRun, once per `Idempotency-Key` |
| `POST /v1/apps/{app}/pipelineruns/{name}/cancel` | `cancel runs` | [Cancel](#cancel) a PipelineRun, or say it already finished |

Runs come from Tekton Results, so they cover 30 days and never go back past the creation of the
app's current `-cicd` namespace (an app deleted and recreated under the same name doesn't inherit
the old one's runs). The shapes match what Tower's backend serves (runs since 2026-10-08, evidence
and tests since 2026-10-28), so Tower can compare its own answers against these field for field
while it moves over.

## Where it runs

`charts/glidepath-control-plane/templates/api/glidepath-api.yaml`: two replicas in
`platform-system` on the control-plane cluster, Service `glidepath-api:8080`. It renders nothing
until `apiImage` is set; `api-ci.yaml` builds a multi-arch image on every merge to main that
touches the API or the contract and sets `apiImage` to its `sha-` tag, the same loop the broker
images use. The contract is baked into the image, so `GET /v1/contract` always matches the code.

Tekton Results is read through `tekton-results-relay`, the hop Tower already uses. The relay holds
the Results read grant; the API decides who may ask before it asks.

Each replica caches what it reads in memory (nothing persisted; Results stays the record). A
finished run never changes, so it is kept 7 days; run lists for a minute, step logs for an hour. A
cold `view=full` read of a busy app's window is several MB from Results and can take longer than a
caller waits (Tower gives up at 10 s), so a cache-filling read is detached from the request: it
finishes within 90 s and fills the cache even when its caller has gone, and concurrent callers
asking for the same thing share one read. At start-up each replica reads every app's last 7 days of runs in the
background, one app at a time (`cmd/glidepath-api/warm.go`; it needs `list` on namespaces to find them), so its first
answers after a restart or a deploy are cached ones.

## Calling it

In the cluster, send a ServiceAccount token:

```shell
curl -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
  http://glidepath-api.platform-system.svc.cluster.local:8080/v1/apps/boarding-api/runs
```

From outside, go through the API server's service proxy. The API server authenticates you and then
drops `Authorization` before forwarding (a Kubernetes limit, the one `tekton-results-relay` exists
for), so send the token again in `X-Glidepath-Authorization`:

```shell
TOKEN=$(kubectl create token <sa> -n <ns>)
APISERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
curl --cacert <the cluster's CA file> -H "Authorization: Bearer $TOKEN" -H "X-Glidepath-Authorization: Bearer $TOKEN" \
  "$APISERVER/api/v1/namespaces/platform-system/services/glidepath-api:8080/proxy/v1/whoami"
```

The caller needs `services/proxy` on `glidepath-api` for that path, and the API then checks the
token itself, so the proxy grant alone reads nothing.

## Who may read what

**Who you are** is a TokenReview by the cluster, so anything the API server trusts works: an
in-cluster ServiceAccount, or a federated identity from another cluster (Tower's identities from
the prod cluster arrive as `hangar:<cluster>:<serviceaccount>`).

**What you may read** is a SubjectAccessReview in the app's `-cicd` namespace, on a group nothing
serves: `glidepath.hangar.io`, resources `runs`, `runs/logs`, `releases`, `evidence` and `tests`. RBAC only needs the
strings, so access is ordinary RBAC:

- `api.readers` binds the chart's `glidepath-api-reader` ClusterRole cluster-wide (users, groups,
  ServiceAccounts). Empty by default: a fresh install grants nobody.
- To let one identity read one app, bind the same ClusterRole with a RoleBinding in that app's
  `-cicd` namespace.

An app that doesn't exist answers 403 to a caller who couldn't read it if it did, and 404 only to
one who could, so the API doesn't tell strangers which apps exist.

**On behalf of.** A delegate, such as Tower, may name the person it acts for in
`X-Glidepath-On-Behalf-Of`. Only usernames in `api.delegates` may send it; anyone else gets
`GP-AUTH-003`. Every call, read or write, is authorized as the caller itself; the person is recorded in
the audit line. A delegate's write must name the person ([who may write](#who-may-write)).

## Writes

Two writes, both on a live PipelineRun in the app's `-cicd` namespace. The namespace always comes from
the app in the path (the same resolver the reads use), never from the request. They replace Tower's
own Re-run and Cancel (backstage `packages/backend/src/tektonActions.ts`) and build the same objects.

### Re-run

`POST /v1/apps/{app}/pipelineruns/{name}/rerun` with an `Idempotency-Key` header. It reads the live
run `name` and creates a copy, exactly as Tower's `rerunBody` does: the same `spec` without
`spec.status` (re-running a cancelled run must not start it cancelled), and only the
`hangar.io/app`/`hangar.io/flow` labels (or the pre-rebrand `platform.io/` pair), so the Pipelines-as-Code
annotations and trigger label are dropped and PaC never reports a commit status for it. Tekton has no
restart; this is `tkn pipelinerun start --use-pipelinerun`.

Two things differ from Tower's body, both for idempotency: the run has a `name` instead of a
`generateName`, and two annotations.

```json
{"metadata": {"name": "ci-0-build-abc-rerun-3f9a1",
  "annotations": {"glidepath.hangar.io/rerun-of": "ci-0-build-abc",
                  "glidepath.hangar.io/idempotency-key-sha256": "3f9a1c..."}}}
```

**Idempotency.** "A re-run creates exactly one PipelineRun" holds across retries and both replicas
with no shared memory: the API server is the lock. The name is `<name>-rerun-` (cut at 58 characters,
as the API server cuts a `generateName` prefix) plus the first 5 hex characters of
`sha256(app + "/" + name + "/" + key)`, so it is as long as the name Tower's `generateName` got. A
second create with the same key fails with AlreadyExists; the API reads the run that holds the name and
answers it as a replay when its annotations say it is the re-run of `name` with this key's full hash,
and `GP-API-409` otherwise (another key that shares the 5 characters, one in a million, or any other
object). A retry after the source was pruned (finished runs go about an hour after completion) still
answers with the run it made. The key is the caller's: 1 to 128 characters of `[A-Za-z0-9._:-]`, the
same on every retry of one re-run and new for the next. A missing or malformed key is `GP-API-400`.

| Answer | Status | Body |
|---|---|---|
| Created | 201 | `{"name": "<new>", "replayed": false}` |
| The same key again | 200 | `{"name": "<new>", "replayed": true}` |
| Name taken by something else | 409 | `GP-API-409` |
| No such run (and no re-run for this key) | 404 | `GP-API-404` |

### Cancel

`POST /v1/apps/{app}/pipelineruns/{name}/cancel` sends the JSON merge patch
`{"spec":{"status":"CancelledRunFinally"}}`, the status Tower's Cancel sends: the pipeline's `finally`
tasks still run (notify-backstage, notify-slack), so a cancel is announced like a failure. It reads the
run first, and a run that needs nothing is answered, not refused, so a retry is always safe:

| Run | Status | Body |
|---|---|---|
| Running or pending | 200 | `{"name": "...", "status": "cancelled"}` (patched) |
| `spec.status` already `Cancelled` or `CancelledRunFinally`, or its `Succeeded` condition reason is a cancel | 200 | `"status": "already-cancelled"` (not patched) |
| `Succeeded` condition `True` or `False` | 200 | `"status": "already-finished"` (not patched) |
| No such run | 404 | `GP-API-404` |

A run stopping gracefully (`StoppedRunFinally`) is still cancelled. A run that finishes between the
read and the patch answers `cancelled`; Tekton ignores the status on a finished run.

### Dry run

`?dryRun=true` on either does every read and check, then the create or patch as a Kubernetes
server-side dry run (`dryRun: All`): admission runs, including the policy below, and nothing is
stored. The answer is 200 and carries what a real call would write, so Tower can compare its own body
with the API's before any real write goes through the API:

```json
{"name": "ci-0-build-abc-rerun-3f9a1", "replayed": false, "dryRun": true, "object": {"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "metadata": {...}, "spec": {...}}}
{"name": "ci-0-build-abc", "status": "cancelled", "dryRun": true, "patch": {"spec": {"status": "CancelledRunFinally"}}}
```

A dry run of a key already used answers the replay, with the run it found as `object`. A dry run is
audited with `"dryRun": true`.

Through the API server's service proxy (how Tower calls), ask with the header `X-Glidepath-Dry-Run: true`
instead: the API server refuses a proxied request with `dryRun` in its query ("dryRun is not supported").
Query and header may both be given only if they agree.

### Who may write

**Authorization is a SubjectAccessReview on the caller**, as for reads: `rerun` or `cancel` on
`runs.glidepath.hangar.io` in the app's namespace. The chart's `glidepath-api-operator` ClusterRole holds
both verbs; `api.operators` binds it cluster-wide (users, groups, ServiceAccounts, empty by default), and
a RoleBinding to it in one app's `-cicd` namespace grants that app alone. It holds no read, and read
access alone is `GP-AUTHZ-001` on a write.

**Both identities are on every write.** When the caller is a delegate (Tower), the write must carry
`X-Glidepath-On-Behalf-Of`, or it is refused with `GP-AUTH-004` before anything is read. The person is
**not** authorized by the API: they are a Backstage identity, not a Kubernetes one, so no
SubjectAccessReview can be asked about them. The delegate is trusted to have checked the person, which
Tower does with its owner permission before it calls. The API authorizes the delegate, and records the
person. So `api.operators` should list a delegate only when that delegate does its own per-person
check.

**The cluster's shape lock.** glidepath-api's own ServiceAccount holds `get`, `create` and `patch` on
`tekton.dev` `pipelineruns` cluster-wide (apps are onboarded after install), which RBAC can't narrow to
Glidepath's shape. The admission policy that holds Tower's writes to that shape
(`tower-pipelinerun-shape`, gitops `30-policy/kyverno-policies/tower-identity-writes.yaml`: an app
`-cicd` namespace, a cluster-resolver `pipelineRef` into `platform-catalog`, SA `pipeline-runner`, no
inline spec, no PaC annotations, and an update may change only `spec.status`) must also match
`system:serviceaccount:platform-system:glidepath-api`. A write the cluster refuses, there or in RBAC, is
`GP-API-422` and is not worth retrying. That policy today admits only `app-` namespaces; an
InfraService's `infra-<name>-cicd` run is refused for Tower and would be for the API until it allows
them.

## Explain

`explain` reads the run and each failed TaskRun's archived log. For each one, it takes the first step
that exited non-zero and tests that step's lines, plus the TaskRun's condition message, against the
contract's `knownFailures` (`schemas/cicd.meta.yaml`), in contract order. The first match wins:

```json
{"taskRun": "itsm-bh5bn-verify-change", "task": "verify-change", "step": "verify",
 "knownFailure": {"id": "ItsmChangeNotApproved", "hint": "..."},
 "evidence": "error: change CHG0010035 for ... is in state '-5' with approval 'not requested' - not approved or scheduled. ...",
 "log": "archived"}
```

`knownFailure` is `null` when nothing matched, and `evidence` is then the step's last 15 lines. `log`
is `missing` when Results stored no log (see [tekton-results.md](tekton-results.md#when-a-runs-logs-are-missing));
the condition message is still classified, which catches timeouts. A log Results can't serve fails
the call with 502, so "nothing matched" always means the log was read. The patterns compile when
the API starts; one that doesn't compile stops it. A new failure gets an id by adding it to
`knownFailures` and regenerating the contract.

## Evidence

`evidence` is Tower's supply-chain panel as an API: everything signed about one image, and whether
each signature still verifies against this cluster's own trust roots. It reads the image from GHCR
in the three places an attestation can live, in this order, and returns all of them:

1. the legacy tag `<repo>:sha256-<digest>.att`: Tekton Chains' SLSA provenance, one DSSE envelope
   per layer, with the certificate and the Rekor receipt in the layer's annotations;
2. OCI 1.1 referrers (GHCR's referrers-tag fallback, `<repo>:sha256-<digest>`): cosign 3's
   attestations (the SBOM, the API spec), each a complete sigstore bundle with an inclusion proof;
3. the signature tag `<repo>:sha256-<digest>.sig`: the image signature itself.

Each one is verified the way Tower's `@sigstore/verify` check does, with the same checks in the same
order: the statement names this image's digest, the signing certificate chains to the Fulcio root
and was valid when Rekor logged the entry, the log entry's receipt is signed by Rekor's key (or, for
a bundle checked on its own, its Merkle inclusion proof reaches a checkpoint Rekor signed), the
entry records this signature over this content, and the signature verifies with the certificate's
key. Each receipt is re-read from Rekor by its log index first, so it is signed by Rekor's current
key; a different body at that index is `log-entry-mismatch` and never verifies. The signature tag
carries no receipt (Chains logs it without attaching one), so its entry is found among the entries
next to the attestations' and kept only if it records exactly this payload hash and signature.

One attestation is the exception: the Flight promotion attestation (predicate
`https://hangar.io/attestations/promotion/v1`, [registries.md](registries.md)) on an image the
promoter copied to Flight. The promoter workflow signs it keyless on **public Sigstore**, not on
this cluster's Fulcio, so it is verified the way an upper cluster's admission does:

- against the Sigstore public-good trusted root built into glidepath-api (TUF target
  `trusted_root.json`, sha256 `6494e21e…`, the same bytes Tower embeds), so the check is offline;
- with everything that root requires: the certificate chains to public Fulcio, carries an SCT from a
  trusted CT log, is dated by Rekor's receipt or the RFC 3161 timestamp, and the entry's inclusion
  proof reaches a checkpoint public Rekor signed;
- and only for the exact promoter identity: SAN
  `https://github.com/<promoterRepo>/.github/workflows/promote.yaml@refs/heads/main` (anchored, so
  no other branch, repository or workflow) and Fulcio issuer
  `https://token.actions.githubusercontent.com`.

Its log index belongs to public Rekor, so it is never looked up in this cluster's Rekor, and its
`transparencyLog` carries no `logId`; `logSource` is `embedded`. The promoter repository is
`api.promoterRepo` (default: `promoter.dispatchRepo`, the repository the promote pipeline already
dispatches). With neither set, promotion attestations come back unverified with
`no promoter configured`.

Act on `verified` and `verificationCode` (`verified`, `no-log-entry`, `log-entry-mismatch`,
`stale-receipt`, `digest-mismatch`, `signature-invalid`, `trust-root-unavailable`, `incomplete`).
`verificationError` is the verifier's own message, kept identical to Tower's for the shadow
comparison; display it, don't parse it. The certificate's identity is shown, not checked against the
release gate's identity policy: that is the gate's job (`verify-image-provenance`).

Where the inputs come from:

| Input | Source | Kept |
|---|---|---|
| The image, its manifests and blobs | GHCR, with a pull token for that one repository | per request |
| The GHCR credential | the app's own `registry-credentials` Secret (dockerconfigjson, `auths["ghcr.io"]`) in its `-cicd` namespace | per request |
| Fulcio root | ConfigMap `fulcio-root-ca` (`cert.pem`) in `platform-catalog` | 10 minutes |
| Rekor's public key | `rekor-server.rekor-system` `/api/v1/log/publicKey` | 10 minutes |
| Rekor entries | `rekor-server.rekor-system` `/api/v1/log/entries?logIndex=` (GET only) | per request |
| Public Sigstore root (promotion attestations only) | built into the binary; refresh it with Tower's copy when Sigstore rotates a key | for the build |

The registry credential is read for each request and used only for GHCR's token exchange
(`/token?scope=repository:<repo>:pull`). It is never cached, logged, returned, or put in an error;
a refused exchange reports GHCR's status code, never its body. The pull token GHCR returns is not
cached either. A missing Secret, or one without a `ghcr.io` entry, is an anonymous exchange, which
only public packages allow. The chart grants glidepath-api `get` on Secrets named
`registry-credentials`, and nothing else among Secrets; the Fulcio ConfigMap is readable by every
ServiceAccount already (`templates/sigstore/fulcio-root-configmap.yaml`).

A Fulcio root that can't be read doesn't fail the call: every attestation comes back unverified with
`trust-root-unavailable`. A Rekor key that can't be read is softer still: verification runs without
the log and fails with `no-log-entry`, since nothing then shows the ten-minute certificate was valid
when it was used. Only `ghcr.io/...` images are accepted (`GP-API-400`). Tower's identity carries
`get evidence` through `glidepath-api-reader` like every other read; whoever can read an app's
evidence can ask about any image that app's credential can pull.

## Tests

`tests` lists the app's Testkube executions (every run Glidepath tagged `app=<app>`, newest first, up
to `limit`, default 50, at most 200) from `testkube-api-server.testkube:8088` in the cluster. Testkube's
list returns summaries only, so each run is read in full, four at a time; a run whose full read fails
is shown from its summary. A run the agent reported `aborted` while every step passed is counted as
`passed` and says so in `statusNote` (a known race on short runs). `tests/{id}/report` returns the
JUnit report the check-ride templates leave (`data/artifacts/junit.xml`), and only for a run tagged
with this app; Tower's own route doesn't check that.

## Errors

Every refusal or failure is JSON with a stable `rule` and, where there is one, a `hint`:

| Rule | Status | Meaning |
|---|---|---|
| `GP-AUTH-001` | 401 | No bearer token |
| `GP-AUTH-002` | 401 | The cluster didn't accept the token |
| `GP-AUTH-003` | 403 | On-behalf-of from a caller that isn't a delegate |
| `GP-AUTH-004` | 400 | A delegate's write must name the person (`X-Glidepath-On-Behalf-Of`) |
| `GP-AUTHZ-001` | 403 | Not allowed to read or write this in this app's namespace |
| `GP-API-400` | 400 | A malformed app name, run id, chain id, test run id, PipelineRun name, `Idempotency-Key` or `dryRun`, or an image outside `ghcr.io` |
| `GP-API-404` | 404 | No such app, run, PipelineRun, release record, or test report |
| `GP-API-409` | 409 | The name a re-run's key derives is held by something that isn't that re-run; send a new key |
| `GP-API-422` | 422 | The cluster refused the write (an admission policy or glidepath-api's RBAC); don't retry |
| `GP-LOG-404` | 404 | The run exists, but Results archived no log for that TaskRun ([tekton-results.md](tekton-results.md#when-a-runs-logs-are-missing)) |
| `GP-API-502` | 502 | A source failed (the API server, Tekton Results, GHCR, Rekor or Testkube); retry |

## Audit

Every call writes one JSON line to the pod's stdout: `op`, `tier`, `app`, `caller`, `onBehalfOf`,
`decision` (`allow` or `deny`), `status`, `ms`, and the error if there was one. A write adds `name` (the
run in the path) and `dryRun`; a re-run adds `idempotencyKeySha256` (the first 12 hex characters of
the hash in the run's annotation, never the key), `created` and `replayed`; a cancel adds `result`
(`cancelled`, `already-cancelled`, `already-finished`).

```shell
kubectl logs -n platform-system deploy/glidepath-api | grep '"msg":"audit"'
```
