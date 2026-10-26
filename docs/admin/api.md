# glidepath-api

`glidepath-api` answers questions about an app's CI/CD for people, Tower and agents: its runs,
their logs, its release records, and Glidepath's [contract](contract.md). It is phase 1 of the
[Glidepath API spec](https://hangarplatform.dev/library/glidepath-api/): **reads only**. Re-runs,
previews and `cicd.yaml` pull requests come in phase 3, promotion and rollback in phase 4.

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
| `GET /v1/apps/{app}/releases` | `list releases` | Every `release-tracking-*` record of the app |
| `GET /v1/apps/{app}/releases/{chainId}` | `get releases` | One record |

Runs come from Tekton Results, so they cover 30 days and never go back past the creation of the
app's current `-cicd` namespace (an app deleted and recreated under the same name doesn't inherit
the old one's runs). The shapes match what Tower's backend has served since 2026-10-08, so Tower
can compare its own answers against these field for field while it moves over.

## Where it runs

`charts/glidepath-control-plane/templates/api/glidepath-api.yaml`: two replicas in
`platform-system` on the control-plane cluster, Service `glidepath-api:8080`. It renders nothing
until `apiImage` is set; `api-ci.yaml` builds a multi-arch image on every merge to main that
touches the API or the contract and sets `apiImage` to its `sha-` tag, the same loop the broker
images use. The contract is baked into the image, so `GET /v1/contract` always matches the code.

Tekton Results is read through `tekton-results-relay`, the hop Tower already uses. The relay holds
the Results read grant; the API decides who may ask before it asks.

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
serves: `glidepath.hangar.io`, resources `runs`, `runs/logs` and `releases`. RBAC only needs the
strings, so access is ordinary RBAC:

- `api.readers` binds the chart's `glidepath-api-reader` ClusterRole cluster-wide (users, groups,
  ServiceAccounts). Empty by default: a fresh install grants nobody.
- To let one identity read one app, bind the same ClusterRole with a RoleBinding in that app's
  `-cicd` namespace.

An app that doesn't exist answers 403 to a caller who couldn't read it if it did, and 404 only to
one who could, so the API doesn't tell strangers which apps exist.

**On behalf of.** A delegate, such as Tower, may name the person it acts for in
`X-Glidepath-On-Behalf-Of`. Only usernames in `api.delegates` may send it; anyone else gets
`GP-AUTH-003`. In phase 1 every call is a read and is authorized as the caller itself; the person is
recorded in the audit line. Phase 3's writes will authorize the person.

## Errors

Every refusal or failure is JSON with a stable `rule` and, where there is one, a `hint`:

| Rule | Status | Meaning |
|---|---|---|
| `GP-AUTH-001` | 401 | No bearer token |
| `GP-AUTH-002` | 401 | The cluster didn't accept the token |
| `GP-AUTH-003` | 403 | On-behalf-of from a caller that isn't a delegate |
| `GP-AUTHZ-001` | 403 | Not allowed to read this in this app's namespace |
| `GP-API-400` | 400 | A malformed app name, run id or chain id |
| `GP-API-404` | 404 | No such app, run, log or release record |
| `GP-API-502` | 502 | A source failed (the API server or Tekton Results); retry |

## Audit

Every call writes one JSON line to the pod's stdout: `op`, `tier`, `app`, `caller`, `onBehalfOf`,
`decision` (`allow` or `deny`), `status`, `ms`, and the error if there was one.

```shell
kubectl logs -n platform-system deploy/glidepath-api | grep '"msg":"audit"'
```
