# ADR-0027: The provenance gate accepts a GitHub PR merge; the signing policy is admin-owned

*Status: Accepted (2026-10-20). Built: the GitHub-merge path in `verify-commit-signature.yaml`, the
`releaseSigning` catalog values and `release-signing-policy` ConfigMap, the tighten-only
`governance.githubMerge` in `cicd.yaml`, gpgv and GitHub's web-flow keyring in the toolbox, the weekly
key check. Not built: the Tower Release Record line naming the signature path (the Task's results carry
it). Detail: [commit-signing.md](../commit-signing.md).*

## Context

The `provenance` gate verified the promoted app commit only with `gitsign verify`. Every commit GitHub
makes when it merges a PR (merge commit, squash or rebase) is signed by GitHub's web-flow OpenPGP key
instead, so every GitHub-merged release failed with `unsupported signature type: not a PEM block`.
Measured over the last release PR of every `gitops-*` repo on 2026-10-20: 10 failures, 2 passes, both
passes gitsign-signed commits pushed straight to `main`. gitops-backstage#49 and #52 were merged past
the red gate. A gate that is red on almost every release teaches people to merge through it.

The signer list (`governance.allowedCommitSigners`) lived in the app's own `cicd.yaml`, which the team
being gated can edit. The same team could add itself to the list that gates its release.

## Decision

1. **Two paths.** gitsign stays first and is final for any non-OpenPGP signature. An OpenPGP-signed
   commit passes only on the GitHub-merge path, when the app's policy enables it and every check holds:
   GitHub's pinned web-flow key, committer GitHub, the `merge_commit_sha` of a merged PR into the
   default branch, on that branch (a merge commit's second parent is the PR head), merged by a login
   in `allowedMergers`. Optional: `mode: strict` (every PR commit gitsign-signed by an allowed signer)
   and `requiredApprovals: N` (approvals of the final head by other allowed mergers).
2. **Off by default; `merger` mode by default when on; 0 approvals by default.** Strict would fail
   every PR whose commits are written where gitsign's browser login cannot run. One approval proves
   nothing while one person holds both accounts.
3. **Admin-owned policy.** Signers, the path's switch, its mode, mergers and minimum approvals are the
   catalog chart's `releaseSigning` values, set per cluster in the cluster repo. A product owner asks;
   a platform admin merges. `cicd.yaml` keeps only tighten-only knobs (`governance.githubMerge.mode:
   strict`, a higher `requiredApprovals`); the stricter value wins. `allowedCommitSigners` in `cicd.yaml`
   is accepted and ignored.
4. **Keys are pinned, never fetched at verify time.** The keyring is checked in and baked into the
   toolbox with a fingerprint assert; the Task pins the same fingerprints. A weekly workflow warns when
   GitHub publishes a different set.
5. **Approvals are checked by the gate, not left to a ruleset.** Rulesets are not enforced on private
   repos on GitHub Free, can be bypassed by repo admins, and leave no record on the release. Keep them
   where they are enforced; the gate's check holds on every plan and records the approvers.
6. **`release-on-tag` gets the same rule** at no extra cost: its release PR names the tagged commit
   and runs the same Task.

## Consequences

- GitHub-merged releases can pass, so a red `provenance` means something again.
- The trust for those releases moves from a person's key to GitHub's key plus GitHub's record of who
  merged. Non-repudiation is weaker: a stolen GitHub session can merge. Strict mode and approvals
  narrow it. security.md lists it as an accepted gap, per app, decided by the admin who enables it.
- A GitHub key rotation fails GitHub-merged releases closed until three files change and the toolbox is
  rebuilt; the weekly check gives warning.
- The policy ConfigMap is readable by every ServiceAccount (emails and logins, not secrets), the same
  scope as the catalog's Task definitions.
- Each app's signer list now has to be in the cluster's catalog values; an app missing from them falls
  back to the cluster default, and an empty default fails gitsign closed, as before.
