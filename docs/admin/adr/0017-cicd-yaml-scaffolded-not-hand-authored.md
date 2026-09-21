# ADR-0017: `cicd.yaml` is scaffolded at onboarding, not hand-authored first

## Context

Airframe's Bootstrap-tier app stacks (`NodeJSApplication`/`SpringBootApplication`/
`GoApplication`/`PythonApplication`) scaffold a real src repo - `package.json`/
`go.mod`/`pom.xml`, a Dockerfile, a README - but never committed a `cicd.yaml`.
Every other onboarding step in this platform is self-service through Tower; this
one wasn't: a developer had to hand-author `cicd.yaml` from scratch (copying an
example from `docs/user/examples/`) before any pipeline could run at all. Worse,
`glidepath-control-plane`'s tenant-onboarding `ApplicationSet` always declares a
source reading `$appsrc/cicd.yaml` as a Helm `valueFiles` entry - see
`xrds/infraservice.yaml`'s own header in `airframe` for why that source can't be
made conditional - so a freshly onboarded app's ArgoCD `Application` had nothing
to resolve until that manual step happened. Found live 2026-09-21, following up
on `airframe/docs/user/quickstart.md`'s own worked walkthrough.

Fixing it surfaced a second, unrelated gap: `schemas/cicd.schema.json`'s
`build.agent` enum (`go-1.22`, `python-3.11`) was narrower than Airframe's own
`GoApplication`/`PythonApplication` XRDs (`goVersion: 1.22|1.23|1.24`,
`pythonVersion: 3.11|3.12|3.13`) - the schema was never updated when those two
stacks were added. A version-matched scaffold for those two would have committed
a `cicd.yaml` that failed this platform's own schema validation on its first run.

## Decision

Every app-stack Composition in `airframe` now commits a minimal, real,
build-only `cicd.yaml` alongside its other src-repo boilerplate - same
create-once-then-hands-off `managementPolicies` as `package.json`/`Dockerfile`/
etc., so a developer's first real edit isn't fought by drift correction.
`build.script` is omitted (every scaffolded Dockerfile does its own build,
single-stage or multi-stage; kaniko builds it directly) and `unitTest.enabled`
starts `false` (no test script exists yet). `build.agent` maps directly onto
the stack's own version field (`nodeVersion`/`javaVersion`/`goVersion`/
`pythonVersion`).

`schemas/cicd.schema.json`'s `build.agent` enum is widened to
`go-1.23`/`go-1.24`/`python-3.12`/`python-3.13` to make that mapping valid,
with matching entries added to `catalog/lib/build-agents.env`.
`platform-cicd-toolbox` rebuilt and tag-bumped to
`2026-09-21-go-python-agents` for this, per this repo's own standing rule
(`charts/glidepath-catalog/values.yaml`'s own comment on `toolboxImage`) -
unconditional even though the schema edit itself is a two-line array append,
because the schema is baked into the image, not mounted.

`InfraService`'s own `cicd-yaml-stub.yaml` (a deliberately zero-stage,
zero-source-code placeholder) is unaffected - a different, correct use case,
not superseded by this change.

## Consequences

- A freshly onboarded app is pipeline-ready immediately after its `GoApplication`/
  etc. XR reaches `Ready` - no manual `cicd.yaml` authoring step, no broken
  first ArgoCD sync.
- The scaffolded pipeline is intentionally minimal (build only, no test, no
  deploy stages) - a developer still owns adding real stages, same as they
  already own editing the scaffolded source itself.
- The `build.agent` enum now tracks every version each app stack actually
  offers. Future stack additions (or version bumps) need to keep
  `schemas/cicd.schema.json` and `catalog/lib/build-agents.env` in sync with
  whatever Airframe XRDs offer - this drifted silently once already.
