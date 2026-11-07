The recorded gate-api `hangar-prod/prod` render used by Clearance's Argo CD plugin parity test
(jfillman/clearance providers/argocd/cmp/testdata/parity-gate-api, copied verbatim): the gitops files, `head.yaml`
(`helm template gate-api-prod` of airframe v0.3.168 with them and `injected.json`), `injected.json` (the platform
values the Argo CD Application injects) and `tekton.json` (the tree and per-object digests the clearance-gate
task's Python computes for that render). `../clearance_gate_parity_test.sh` holds the task to all of it.
