# ADR-0005: Per-cluster ArgoCD instances, event-driven outcome reporting

## Context

Only one cluster needs to run the full Tekton/PaC/broker control plane (the dev
cluster); staging/prod-tier clusters just need to receive releases. The design has to
scale to *many* upper clusters over time (multi-region/multi-tenant-cluster), not just
one, all reporting back to the same dev cluster.

## Decision

Each additional cluster gets its own ArgoCD instance watching the same gitops repo,
rather than the dev cluster's ArgoCD being extended with remote-cluster credentials to
manage it. A single ArgoCD instance holding a remote-cluster credential is itself a
path from dev into prod - the same blast-radius problem this platform's whole RBAC
model exists to avoid, just relocated into ArgoCD's control plane instead of a Tekton
Task.

Outcomes flow back to dev as an *event*, not a push: on the upstream cluster, two
ArgoCD **sync hooks** (`PostSync`/`SyncFail` Jobs, rendered by `idp-service-catalog`'s
`idp-application` chart alongside the app's own manifests) build a CDEvent themselves
and POST it to dev's broker, authenticated with a shared secret per upstream cluster.
A relay service (`argocd-outcome-relay`) in front of the existing broker consumes it -
authenticating the shared secret and forwarding the CDEvent bytes on with its own
in-cluster SA token - rather than modifying the broker's own TokenReview path. Cluster
identity is a first-class field in this payload/auth scheme from the start, not
assumed to be a single implicit "prod": the relay rejects any request whose claimed
`subject.content.cluster` doesn't match the `<cluster>` its URL path authenticated
against.

ArgoCD Notifications (webhook service + `oncePer` dedup) was the first implementation
tried and was replaced after live testing found it fires on **any** completed sync
operation, including pure selfHeal drift-correction with no release involved - a
manually-scaled Deployment (bypassing git entirely) produced a real, confirmed call to
the relay for an app that had never been released. Sync hooks don't share that flaw:
they're tied to the resources a sync actually had to reapply, not to "a sync happened"
in general, and `PostSync` additionally only fires once health actually converges, not
just on manifest-apply success. See
[multi-cluster.md](../multi-cluster.md#what-feeds-the-relay-and-what-didnt-work-first)
for the full live-verified comparison.

## Consequences

- Dev never holds a credential for staging/prod, at any point - verified live, not
  assumed.
- Adding a new upper cluster is "stand up its own ArgoCD, register a relay secret,"
  not a change to the dev cluster's own credentials or trust boundary.
- The DORA exporter gained a second input path instead of replacing its original
  same-cluster CDEvents informer - both paths coexist. It's fed by a Task
  (`update-dora-metrics.yaml`) in the `release-outcome-notify` Pipeline the relay's
  forwarded CDEvent triggers, not by a direct call from the relay itself (an earlier
  version had the relay call the exporter directly; folded into the Pipeline so a
  failure there is a visible TaskRun failure instead of a silently-dropped side call).
