A chart and gitops tree for the digest-equality and hostile-render tests (tests/tokenless_render_test.py).

`golden.json` was produced by the clearance-gate `render` step as it was BEFORE rendering moved to the token-less render-manifests
task (glidepath main at 3a7e099), with real `helm template` on `chart/` (release `x-prod`, namespace `app-x-prod`, values
`gitops/hangar-prod/prod/{values,release}.yaml`, the base tree with `replicas: 1`): the tree digest and every object digest the
Clearance request carried. The test renders the same inputs through the new path and requires the same bytes. Do not regenerate it
from the current code: a changed digest breaks every clearance Clearance matches against its Argo CD plugin's digest of the same objects.

The chart covers what could change a digest if the gate's parse or the render's bytes changed: a pre-install hook and a test hook, a
CRD (in `crds/`, which `helm template` leaves out, and in `templates/`), a subchart, anchors, aliases and merge keys, CRLF line endings, a
`List`, timestamps and YAML 1.1 scalars (`yes`, `012`, `0o14`, `0x1F`, `1:30`, `1.10`, `~`), a binary scalar, a duplicate key (last wins),
empty and comment-only documents, documents without a kind or a metadata mapping, cluster-scoped and namespace-less objects, unicode. (Helm
itself refuses a document that is a bare scalar or a list, or whose `metadata` is not a mapping; the ingest tests cover those.) A mapping
that mixes a YAML 1.1 boolean key (`yes:`) with string keys makes the digest function raise (json.dumps sort_keys); the old gate did too,
so the fixture avoids it, and docs/admin/security.md lists it with the other parser differentials.
