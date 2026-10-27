# Governance gates

`sast`, `imageScan`, `policyCheck`, and `sbom` are real, enforcing gates today - see
[features.md](../user/features.md#governance-gates---real-not-stubs) for what each one
actually verifies (Semgrep, Trivy, gitsign commit-signature verification, cosign SBOM
attestation), and so is `qa` since 2026-10-19 (a check ride read from the promoted
image's test provenance, [testing.md](testing.md)). `itsm` is real once the catalog names a change tool
([change-management.md](change-management.md), 2026-10-19) and this stub until then;
`policy-validation` is real since 2026-10-26 (the release rendered and evaluated offline against the target
cluster's admission policies, [policy-validation.md](policy-validation.md), ADR-0028), so `itsm` without a change
tool is the only stub left (`image-promotion` was one until ADR-0025 retired it, 2026-10-09; `qa` and
`policy-validation` were until they graduated) - see
[release-guardrails.md](release-guardrails.md) for what each is intended to verify and the
registry (`.Values.releaseGuardrails`) that drives the full current gate list. This doc covers the mechanism that got the first four there,
and that any future gate should reuse: **a stub is never reported with the same
confidence as a real check** - see ADR-0003 for why that matters enough to be a
standing design rule, not just a one-time migration detail.

## How a stub stays honestly a stub

Before a gate has real logic behind it, its Task
(`charts/glidepath-catalog/templates/tasks/governance-gate-stub.yaml`) calls
`charts/glidepath-catalog/templates/stepactions/governance-stub.yaml`, which:

1. Logs, loudly, to the step's own output: `no real check implemented yet`.
2. Emits its own child span, named `governance:<gate>` and parented to the current
   stage's span, carrying `governance.stub=true` and `governance.gate=<name>` as span
   attributes - a structural, machine-readable fact on the trace itself, not a comment
   someone can miss.
3. Sets its Task result to the literal string `"stub"` - never `"passed"` or
   `"failed"`, which would imply a real judgment was made.

A stub ignores the registry's `required` field ([release-guardrails.md](release-guardrails.md),
"Required and advisory gates"): advisory means "a real check that reports without blocking",
and a stub has nothing to report. It stays in every required set, so a stub check that never
ran is still noticed by `detect-bypass-merge`.

`charts/glidepath-control-plane/files/dashboards/pipeline-detail.json`'s
"Governance gates in this run" panel renders any `governance.stub=true` row with a
distinct grey background and the label "STUB - not a real check", so a stub can never
be visually confused with a real pass/fail result.

## Landing a real implementation

Swapping a stub for a real check is a one-Task change: delete the `governance-stub`
call, add the real logic, keep the Task's name/params/results contract the same.
Nothing about the Pipeline shape, the `cicd.yaml` schema, or the dashboard panel needs
to change - this is what let all four gates land independently as each was actually
built, rather than needing a coordinated cutover.
