# Image-scan exceptions (accepted findings)

The `image-scan` gate fails a release PR on any HIGH or CRITICAL vulnerability in the promoted image, fixed or not. Sometimes a
finding is real, has no fix yet, and should not hold the release. An app can accept it, deliberately, in a file in its own gitops
repo. Nothing is accepted silently, nothing is accepted for ever, and a release PR can never accept its own findings.

## What you get

- **Accepting a finding is its own reviewed change.** The file is read from the gitops repo's **default branch** at the moment the
  check runs, by commit, never from the PR's head. A release PR that adds or edits `.trivyignore.yaml` changes nothing for itself:
  the change has to be merged to the default branch first, under that repo's normal review.
- **Every exception expires.** An entry without `expired_at` is not "forever"; it is a refusal. The gate fails with the entry named.
  An `expired_at` further ahead than the maximum (90 days by default) is refused the same way. Once the date passes, Trivy stops
  applying the entry and the finding counts again.
- **Every exception has a reason.** An entry without a `statement` is refused.
- **Never silent.** The gate's result (the PR comment's findings summary and the Slack scan message) lists each accepted finding
  apart from the failing ones: id, package, statement, expiry. It also names the default-branch commit the file came from, warns about
  entries expiring within 14 days, says when an entry matches nothing in the image ("unused"), and says when one has expired.
- **No file, no exceptions, no switch.** An app with no `.trivyignore.yaml` on its default branch behaves exactly as before.

## The file

`.trivyignore.yaml` at the root of the gitops repo (`gitops-<app>`), in
[Trivy's own ignore format](https://trivy.dev/docs/latest/configuration/filtering/#by-finding-ids):

```yaml
vulnerabilities:
  - id: CVE-2026-12345
    statement: no fixed version of picomatch published; re-check before expiry
    expired_at: 2027-01-15          # an unquoted date, at most 90 days ahead
    # optional, narrow it further:
    # paths: ["usr/lib/node_modules/**"]
    # purls: ["pkg:npm/picomatch@4.0.3"]
```

Rules the gate enforces before Trivy sees the file (a failure refuses the whole file, fails the gate, and the summary says which entry):

| Rule | Why |
|---|---|
| Top-level keys are only `vulnerabilities`, `misconfigurations`, `secrets`, `licenses` | A typo such as `vulnerabilites:` would otherwise accept nothing and look fine. This scan only looks at vulnerabilities; the other sections are validated the same way. |
| Each entry has an `id` | Trivy matches on it. |
| Each entry has a non-empty `statement` | The reason has to be written down where reviewers see it. |
| Each entry has an `expired_at`, written as an unquoted YAML date | Trivy treats a missing or unparsed date as "never expires". A quoted string is refused for that reason. |
| `expired_at` is no more than `exceptions-max-days` ahead | Stops a "temporary" exception being set for years. |

Entries already past their `expired_at` are not an error; they simply accept nothing, and the summary lists them as expired so they
get removed.

## How it runs

`image-scan-check` (the gate on a release PR in the gitops repo) gives the `image-scan` Task its own `repo-url` as
`exceptions-repo-url`. The Task's `fetch-exceptions` step then:

1. Mints the read-only, single-repo GitHub App installation token the release gates already use (the same broker and
   ServiceAccount token as `extract-promoted-image`). No new credential.
2. Asks GitHub for the repo's default branch and that branch's current commit, then reads `.trivyignore.yaml` at **that commit**.
   A 404 means no exceptions. Any other failure (GitHub error, no token) makes the gate report `error` rather than guess: the gate
   cannot tell what is accepted, so it does not pass.
3. Validates the file as above and writes the verdict for the scan step.

The `scan` step passes a valid file to Trivy with `--ignorefile` and `--show-suppressed`. **Trivy drops expired entries itself**:
in Trivy 0.73.0 (the version `catalog/toolbox/Dockerfile` pins) `ParseIgnoreFile` calls `Prune`, which removes every entry whose
`expired_at` is before now (`pkg/result/ignore.go`), and the filtering runs in the Trivy client, so it applies against the shared
`trivy-server` too. `--show-suppressed` keeps what was dropped in the JSON as `ExperimentalModifiedFindings`, which is where the
accepted list comes from. The verdict counts only what remains. Bumping `TRIVY_VERSION` should re-check both behaviours; the test
(`charts/glidepath-catalog/tests/image_scan_exceptions_test.sh`) emulates them, so it will not notice a change in Trivy itself.

The build pipeline's own `image-scan` (on the app repo, after the image is pushed) is not given a repo and reads no exceptions: it
reports every finding, as before. Exceptions apply to the release gate.

## Settings

| Where | Name | Default | Meaning |
|---|---|---|---|
| `image-scan` Task param | `exceptions-max-days` | `90` | How far ahead an `expired_at` may be. Set it on the Task's param in `image-scan-check.yaml` to change it for the cluster. |
| `image-scan` Task param | `exceptions-repo-url` | empty | The repo to read the file from. Only `image-scan-check` sets it. |

The 14-day expiry warning is fixed.

## Summary format

```
1 finding(s) (HIGH/CRITICAL, vuln scanner only) after 2 accepted by .trivyignore.yaml
*CRITICAL* CVE-2026-2222 - zlib@1.3
Accepted, not counted above: .trivyignore.yaml on main a1b2c3d4e5f6 (2 finding(s))
- CVE-2026-1111 picomatch@4.0.3 (HIGH): no fixed version of picomatch published; re-check before expiry - expires 2027-01-15
Warnings:
- WARNING CVE-2026-1111 expires in 9 day(s) (2027-01-15): re-check it before then
- unused: CVE-2026-9999 matches nothing in this image (HIGH/CRITICAL); remove it
```

The summary is a Tekton result and is cut at 3000 bytes; the scan step's log has the full Trivy table.

## Failures

| Message | Meaning |
|---|---|
| `image-scan: exceptions file refused - ...` | The file on the default branch broke a rule above. Fix it on the default branch. The gate outcome is `failed`. |
| `image-scan: could not read the exceptions file ...` | GitHub or the token broker failed. The gate outcome is `error`; re-run the check. |

See also [release-guardrails.md](release-guardrails.md) (`image-scan` gate, and the per-app `required: false` override for a gate
that must not hold releases at all) and [release.md](release.md).
