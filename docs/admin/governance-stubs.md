# Governance gates

`sast`, `imageScan`, `policyCheck`, and `sbom` are real, enforcing gates today - see
[features.md](../user/features.md#governance-gates---real-not-stubs) for what each one
actually verifies (Semgrep, Trivy, gitsign commit-signature verification, cosign SBOM
attestation). `itsm`, `qa`, `policy-validation`, and `image-promotion` are newer stub
gates, not yet real - see [release-guardrails.md](release-guardrails.md) for what each
is intended to verify and the registry (`.Values.releaseGuardrails`) that drives the
full current gate list. This doc covers the mechanism that got the first four there,
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
