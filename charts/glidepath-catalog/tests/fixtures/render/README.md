A small chart and gitops tree for the digest-equality and hostile-render tests (tests/tokenless_render_test.py).

`golden.json` was produced by the clearance-gate `render` step as it was BEFORE rendering moved to the token-less render-manifests
task (glidepath main at 3a7e099), with real `helm template` on `chart/` (release `x-prod`, namespace `app-x-prod`, values
`gitops/hangar-prod/prod/{values,release}.yaml`, the base tree with `replicas: 1`): the tree digest and every object digest the
Clearance request carried. The test renders the same inputs through the new path and requires the same bytes. Do not regenerate it
from the current code: a changed digest breaks every clearance Clearance matches against its Argo CD plugin's digest of the same objects.
