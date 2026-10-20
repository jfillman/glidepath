# Commit signing (Phase 3 item 2, sub-item 1 of SLSA/Sigstore/Tekton Chains)

The release stage's `provenance` gate proves who is behind the app commit a release
promotes. It accepts that commit in one of two ways:

| Path | What it proves | Default |
|---|---|---|
| **gitsign** | An allowed person signed this exact commit with a short-lived Sigstore certificate bound to their GitHub identity, logged in the public Rekor. | always on |
| **GitHub merge** (2026-10-20) | GitHub signed this commit when it merged a pull request into the default branch, and GitHub's record says an allowed person pressed merge. Optional **strict** mode also requires every commit in that PR to be gitsign-signed. | off, per app |

Both read the **release-signing policy**, which platform admins own, never the app's own
`cicd.yaml` (see [Who sets the policy](#who-sets-the-policy)). The PR comment and the
Task's results say which path verified the commit.

It was the first of this platform's governance gates to do real work instead of being a
structurally-loud stub (see `docs/governance-stubs.md`).

## At a glance: the guardrails

- **gitsign first, and final.** A commit with a gitsign signature is judged by gitsign
  alone; the GitHub-merge path is never a fallback for a failed gitsign check.
- **Only a PR's own merge.** The web editor, the contents API, GitHub Apps and the
  "Update branch" button make commits signed with the same GitHub key. They fail: nothing
  records them as a PR's `merge_commit_sha`.
- **Pinned keys, no network trust.** GitHub's web-flow keys are baked into the toolbox
  image with a fingerprint assert, and pinned again in the Task. An unknown key fails
  closed; the gate never fetches a key while verifying.
- **People, by login, from a list admins keep.** The merger must be in
  `allowedMergers`. An empty list fails closed.
- **The team can tighten, never loosen.** `cicd.yaml` can ask for strict mode or more
  approvals. It cannot turn the GitHub-merge path on, name mergers or add signers.
- **Every failure names its check**, in the log and the PR comment.

## Why this is public Sigstore, not self-hosted

Two different signing use cases in this platform's SLSA/Sigstore work end up needing
different infrastructure, decided deliberately rather than defaulting to one answer for
both:

- **`gitsign` (this doc)**: a human developer, interactively authenticating - the public
  Sigstore instances (`fulcio.sigstore.dev`/`rekor.sigstore.dev`) plus a real OIDC
  identity provider (**GitHub**, confirmed as the choice for this platform) is exactly
  how most real orgs already do this. Zero new infrastructure.
- **Tekton Chains (a later Phase 3 item)**: a cluster workload identity, no human in the
  loop - the public Fulcio doesn't trust an arbitrary Kubernetes cluster's own OIDC
  issuer (its trusted identity providers are a fixed set: `google`/`spiffe`/`github`/
  `filesystem`), so that needs a **self-hosted** Fulcio+Rekor instead. Not this doc's
  concern - covered when that item is built.

## Developer setup

1. **Install `gitsign`** (and its optional credential cache):
   ```
   brew install sigstore/tap/gitsign          # macOS
   # or download the linux_amd64/linux_arm64 binary from
   # https://github.com/sigstore/gitsign/releases
   ```

2. **Configure git** (per-repo or `--global`):
   ```
   git config gpg.x509.program gitsign
   git config gpg.format x509
   git config commit.gpgsign true
   ```

3. **Commit as normal**. The first commit (or first every ~10 minutes, without the
   credential cache) opens a browser to `https://oauth2.sigstore.dev/auth` - Sigstore's
   own Dex-based OIDC broker, which then redirects to **GitHub** to authenticate. gitsign
   gets a short-lived (~10 minute) certificate from Fulcio bound to your GitHub identity,
   signs the commit with it, and logs the signature to Rekor. The cert itself is
   discarded after signing - nothing long-lived to leak or rotate.

   **Which email lands in the cert depends on your GitHub privacy setting.** If "Keep my
   email addresses private" is enabled on your GitHub account (common default), the
   identity asserted is your **noreply** address,
   `<numeric-id>+<username>@users.noreply.github.com` - not your real email, even if
   `git config user.email` is set to something else (that field is never read by
   gitsign/Fulcio; it's purely local git metadata). Confirmed live: a real signed commit
   here came back with `7268337+jfillman@users.noreply.github.com`, not
   `crossfader@gmail.com`, despite the local git config. Either allowlist the noreply
   address (see below) or disable that GitHub privacy setting - both are supported.

4. **Optional but recommended - avoid re-authenticating every commit**: run
   `gitsign-credential-cache` (the same release asset as `gitsign`) as a background
   process; it holds the cert/key in memory over a local socket for its ~10-minute
   lifetime so a burst of commits doesn't mean a burst of browser prompts.

5. **Verify locally, if you want to check your own work**:
   ```
   gitsign verify --certificate-identity=<your-email-or-noreply-address> \
     --certificate-oidc-issuer=https://github.com/login/oauth HEAD
   ```

## How `provenance` verifies it

`provenance` runs as a Check on the **gitops** repo's release PR, but the commit that
needs a verified human behind it is the **app**-repo commit being promoted - the gitops
commit itself is generated by `open-release-pr.yaml`'s own automation identity and is
never hand-signed. `open-release-pr.yaml` embeds `X-App-Repo`/`X-App-Commit` trailers in
its own commit message specifically so `charts/glidepath-catalog/templates/tasks/verify-commit-signature.yaml` (run
via `charts/glidepath-catalog/templates/pipelines/provenance-check.yaml`) can find its way back to the right commit: it
reads those trailers and does a narrow single-commit `git fetch` of just that one app-repo
commit (no second PaC `Repository` CR/webhook needed on the app repo for this). Then:

1. It reads the app's [release-signing policy](#the-release-signing-policy) and the app's
   tighten-only knobs.
2. It reads the commit's `gpgsig` header (headers only, never the message).
   - No signature: fail.
   - Anything but OpenPGP (gitsign writes `BEGIN SIGNED MESSAGE`): the **gitsign path**.
     Pass or fail is final.
   - OpenPGP (`BEGIN PGP SIGNATURE`): the **GitHub-merge path**, only if the policy
     enables it for this app.
3. On a pass it writes the results `signature-path` (`gitsign`, `github-merge` or
   `github-merge-strict`), `signature-detail` (compact JSON: repo, commit, and for a
   merge `pr`, `mergedBy`, `approvers`, `key`) and `findings-summary`, the line the PR
   comment shows, for example:
   `provenance: verified via GitHub merge of jfillman/backstage#171 by jfillman (merge commit, web-flow key B5690EEEBB952194)`.

The same Task runs for every release, including a git-rooted `release-on-tag` flow
(example 05): the tag event opens a release PR whose `X-App-Commit` is the tagged
commit, so a tag on a GitHub merge commit on the default branch gets the GitHub-merge
path, and a tag on anything else does not. Neither path checks who pushed the tag.

### The gitsign path

```
gitsign verify \
  --certificate-identity-regexp "^(email1|email2|...)$" \
  --certificate-oidc-issuer "https://github.com/login/oauth" \
  <commit>
```

The regexp is built at run time from the policy's `allowedCommitSigners`, escaping `.`
and `+` (both real regex metacharacters in real addresses: GitHub's noreply format is
`<id>+<username>@users.noreply.github.com`). An empty list fails closed: "refusing to
verify against an empty allowlist".

**The `--certificate-oidc-issuer` value is `https://github.com/login/oauth`, not
`https://oauth2.sigstore.dev/auth`** - confirmed by inspecting a real signed commit's
cert, after the first version of this doc/Task got it wrong by trusting `gitsign
version`'s default-config output instead. `oauth2.sigstore.dev` is the Dex-based broker
gitsign *talks to* during login, but it federates to an upstream IdP (GitHub here), and
it's the upstream connector's own issuer URL that ends up in the cert's issuer claim -
verifying against the broker URL fails against every real cert.

### The GitHub-merge path

Every check must hold, in this order. The first that fails ends the run with a `FAIL:`
line naming it.

| # | Check | How | Rejects |
|---|---|---|---|
| 1 | **Signature** | `git verify-commit --raw` with `gpg.program` pointed at `gpgv` and a keyring holding only GitHub's web-flow keys (`/etc/glidepath/github-web-flow.gpg`). Exactly one `VALIDSIG`, no `BADSIG`/`ERRSIG`/`NO_PUBKEY`/`REVKEYSIG`; the primary fingerprint must be pinned: `968479A1AFF927E37D1A566BB5690EEEBB952194` (current, from 2024-01-16), or `5DE3E0509C47EA3CF04A42D34AEE18F83AFDEB23` (retired) for a signature made before 2024-01-16T20:00Z. | an altered commit, any other key, a new GitHub key not yet pinned |
| 2 | **Committer** | `committer GitHub <noreply@github.com>` | a person's commit signed with some OpenPGP key |
| 3 | **A PR's own merge** | `GET /repos/{app}/commits/{sha}/pulls` has a PR in the same repo, merged, `merge_commit_sha == sha`, `base.ref` = the repo's default branch; `GET /pulls/{n}` agrees. | web editor, contents API, GitHub Apps, "Update branch", a PR into another branch |
| 4 | **Lineage** | `GET /compare/{sha}...{default}` is `ahead` or `identical` (the commit is on the default branch). Two parents: the second must be the PR's `head.sha`. One parent (squash or rebase): being on the branch is the check. | a merge that never reached the default branch, a merge commit whose content is not the PR |
| 5 | **Merger** | `merged_by.login` is in `allowedMergers` (case-insensitive) | anyone not listed, a bot unless listed |
| 6 | **Strict mode** (`mode: strict`) | every commit of `GET /pulls/{n}/commits` (up to GitHub's 250) is fetched by SHA and passes the gitsign check above | an unsigned or wrongly signed commit anywhere in the PR |
| 7 | **Approvals** (`requiredApprovals: N`) | each reviewer's latest decisive review (COMMENTED doesn't count either way) is `APPROVED` on the PR's final head, by a login in `allowedMergers` other than the merger; at least N such reviewers | no review, an approval of an earlier push, self-approval, an approval later replaced by "changes requested" |

Why `git verify-commit` instead of splitting the commit by hand: git separates the
signature from the signed payload exactly as it signs. A hand-rolled split got the
trailing newline wrong on GitHub's newline-less merge messages, and every real merge came
back `BADSIG`. git's own exit code is ignored: it reads the retired key's valid
signature (`EXPKEYSIG`) as a failure, so the Task parses gpgv's status lines instead.

API cost: 4 reads per release (repo, commit's PRs, PR, compare), plus one for reviews when
approvals are required, plus a page per 100 PR commits in strict mode, all with the
app-repo installation token the Task already mints.

### What a web-flow signature does and doesn't prove

| Claim | gitsign path | GitHub-merge path |
|---|---|---|
| This exact commit object is unmodified since signing | yes (Sigstore cert, Rekor) | yes (GitHub's OpenPGP key) |
| Who signed | an allowed person, by OIDC identity | GitHub's servers |
| Who caused it | the signer | `merged_by` of the PR whose `merge_commit_sha` it is (GitHub's record) |
| Who wrote the code | the signer | the PR's commit authors; in strict mode each commit is gitsign-signed by an allowed signer |

The trust moves from "a person's key" to "GitHub's key plus GitHub's record of who pressed
merge." The platform already trusts GitHub for source hosting, PR review and the webhook
that starts every build, so this adds no new party. **It does weaken non-repudiation: a
stolen GitHub session can merge.** Strict mode closes most of that gap for the code
itself (each commit still needs its author's own short-lived Sigstore login); required
approvals add a second person. Neither closes it fully for the merge click itself.

### Modes and approvals

- **`merger`** (the default when an app turns the path on): the merge is the evidence.
  Use it while commits are written somewhere that cannot run gitsign's browser login
  (agents, this platform's own automation) - strict would fail every PR authored there.
- **`strict`**: the merge plus every PR commit gitsign-signed. Use it when every author
  can sign.
- **`requiredApprovals`**: 0 by default. Raise it to 1 once a second real person merges
  for the app; with one person and a stand-in account it proves nothing.

**Approvals in the gate vs. a GitHub ruleset.** The rulesets in
[github-ruleset.md](github-ruleset.md) stop a merge *before* it happens and give
stale-review dismissal and code-owner review for free, so keep them wherever GitHub
enforces them. They cannot carry this alone:

| | Ruleset (prevent at merge) | Gate check (verify at release) |
|---|---|---|
| Works on private repos on GitHub Free | no, shown but not enforced | yes |
| Repo admins / bypass list can skip it | yes | no |
| Leaves evidence on the release | no; whether it was on at merge time isn't recorded | yes: approvers in `signature-detail` |
| Stops unreviewed code reaching `main` and dev | yes | no, only the release |
| Who sets the number | repo admins | platform admins (policy), team may raise it |

Use both: the ruleset prevents where it is enforced; the gate proves it happened, on every
plan.

## Who sets the policy

Platform admins. The policy decides whose signature and whose merge release a team's
code, so the team being gated cannot be the one that writes it.

| Setting | Owner | Where |
|---|---|---|
| Defaults for a cluster: signers, whether the GitHub-merge path exists, its mode and minimum approvals | platform admins | `releaseSigning.defaults` in `charts/glidepath-catalog/values.yaml` (a glidepath PR), overridden per cluster in the cluster repo's `50-glidepath/glidepath-catalog` values |
| Turning the path on for one app; that app's signers and allowed mergers | platform admins merge; the product owner asks for it or approves it | `releaseSigning.apps.<app>` in the cluster repo's `50-glidepath/glidepath-catalog` values |
| Making it stricter for one app: `mode: strict`, more approvals | the team | `governance.githubMerge` in the app's `cicd.yaml`; the stricter of the two applies |

Never Tower, never `cicd.yaml` for anything that loosens. Until 2026-10-20 the signer list
was `governance.allowedCommitSigners` in the app's own `cicd.yaml`, so anyone who could
merge to the app repo could add themselves to the list that gates their own release.
That key is still accepted by the schema (so existing files validate) and is **ignored**.

## The release-signing policy

Catalog values, rendered into one ConfigMap, `release-signing-policy` in the catalog
namespace (`templates/release-signing-policy.yaml`), as a single JSON key `policy.json`:

```yaml
# the cluster repo's 50-glidepath/glidepath-catalog application.yaml, helm.valuesObject
releaseSigning:
  defaults:
    allowedCommitSigners:            # plain emails, no regex
      - 7268337+jfillman@users.noreply.github.com
      - crossfader@gmail.com
    githubMerge:
      enabled: false                 # the path is off unless an app turns it on
      mode: merger                   # merger | strict
      allowedMergers: []             # GitHub logins, case-insensitive
      requiredApprovals: 0
  apps:
    backstage:
      githubMerge:
        enabled: true
        allowedMergers: [jfillman]
```

The Task merges `apps.<app>` over `defaults` with jq's `*`: objects merge key by key, a
list or a scalar replaces. So an app's own `allowedCommitSigners` replaces the default
list rather than adding to it, and `enabled: false` turns a default `true` off for one app.
The merge happens in the Task, not in Helm, because Sprig's merge drops a `false` that
should override a `true`. Nothing is validated at render time: one bad app entry failing
the catalog render would stop every app's gates, so the Task fails that one app closed
instead (an unknown `mode`, a non-numeric `requiredApprovals`, an empty list).

The ConfigMap is readable by every ServiceAccount (`rbac.readOnlyGroup`, the same group
that reads the catalog's Tasks), because each app's `pipeline-runner` runs the gate in its
own namespace. It holds commit emails and GitHub logins, which are on every commit and PR
anyway. Nothing binds write access to it; Argo CD syncing the catalog chart is the only
writer.

The team's tighten-only knobs live in `cicd.yaml` and reach the Task through the app's own
`<app>-policy-config` ConfigMap (`github-merge-mode`, `github-merge-required-approvals`):

```yaml
governance:
  githubMerge:
    mode: strict            # "" | merger | strict; strict wins over the admin's merger
    requiredApprovals: 1    # the higher of this and the admin's number applies
```

The schema refuses `githubMerge.enabled` or `allowedMergers` in `cicd.yaml`, and the Task
never reads them from there.

The same merged `allowedCommitSigners` is handed to Conforma as `ruleData` by
`verify-image-provenance.yaml` (no current rule reads it).

## Merge strategy: what each way of landing code gives you

`gitsign` signs one exact commit *object* - the signature is computed over that commit's
own serialized content (tree, parent(s), author, committer, message). Change any of
those fields and you get a different object with a different SHA; the old signature
doesn't carry over. All three of GitHub's PR merge-button strategies make a new commit,
signed by GitHub's web-flow key instead of by you:

- **Create a merge commit** - a brand-new two-parent commit, committed by GitHub.
- **Squash and merge** - a brand-new single commit combining the diff. Your original
  signed commits don't land on the target branch.
- **Rebase and merge** - GitHub replays each commit itself, producing new commit objects
  (new SHAs) distinct from the ones `gitsign` signed locally.

Before 2026-10-20 that made every GitHub-merged release fail `provenance` with `Error:
unsupported signature type: not a PEM block` (first diagnosed 2026-08-11). Measured over
the last release PR of every `gitops-*` repo on 2026-10-20: **10 failures, 2 passes**, and
both passes were gitsign-signed commits pushed straight to `main`. A gate that is red on
almost every release teaches people to merge through it (gitops-backstage#49 and #52 both
were), so a real failure gets ignored the same way. That is why the GitHub-merge path
exists.

| How the commit lands | Path | Passes when |
|---|---|---|
| gitsign-signed commit fast-forwarded or pushed to `main` | gitsign | the signer is in `allowedCommitSigners` |
| Any of GitHub's three merge buttons | GitHub merge | the app enables the path and every check above holds |
| GitHub web editor, contents API, a GitHub App, "Update branch" | GitHub merge | never: not a PR's merge |
| Unsigned | none | never |

**Still the strongest:** push a gitsign-signed commit by fast-forward
(`git merge --ff-only <branch> && git push`), or tag the exact commit you signed for
`release-on-tag`. `release-on-tag`'s trigger only needs an image already built for that
SHA and a valid signature on it, whichever path verifies it.

## Rotation and failure modes

- **GitHub rotates its web-flow key** (last done 2024-01-16). Every GitHub-merged release
  then fails closed with "signed by unknown GitHub key `<fpr>`". The fix is three edits
  and a toolbox rebuild: `catalog/toolbox/github-web-flow.asc` (from
  `https://github.com/web-flow.gpg`), the fingerprint assert in `catalog/toolbox/Dockerfile`,
  and the pinned `case` in `verify-commit-signature.yaml` (give the old key a not-after
  time). `.github/workflows/web-flow-key-check.yaml` diffs GitHub's published keys
  against the checked-in file every Monday and fails, which mails the repo owner, before
  that bites. Confirm a new key from GitHub's own announcement; never copy it blindly.
- **Admin merge past a ruleset, or a force-push to `main`.** Not a PR's merge, so check 3
  fails. `detect-bypass-merge.yaml` stays the break-glass signal on the gitops side.
- **A bot merged the PR** (Renovate auto-merge, Glidepath automation). `merged_by` is the
  bot's login; it passes only if listed. Don't list bots, so a person still presses merge.
- **`/retest provenance-`.** Unchanged: the gitops PR number is resolved from the
  revision commit (gitops-boarding-api#70); the app-side lookups go by commit SHA.
- **A PR with more than 250 commits in strict mode.** GitHub lists only 250, so strict
  fails closed rather than verifying part of the PR.

## Verification

Unit level, in CI (`catalog-ci` job `commit-signature`):
`charts/glidepath-catalog/tests/verify_commit_signature_test.sh` runs the rendered
Task script against recorded real commits (`tests/fixtures/commits/`, see its README:
backstage's merge of #171 and its unsigned head, a gitsign-signed baggage-api commit, a
squash, a 2023 commit signed by the retired key, a web-editor commit, an update-branch
merge) plus a commit signed by a throwaway key posing as GitHub and an edited copy of a
real merge, with GitHub's real keyring and real `gpgv`. The GitHub API, `kubectl`,
`git fetch` and `gitsign` are stubbed.

Live, on the release cluster, for each change to the Task:

- A GitHub-merged release PR of an app with the path on, `mode: merger`: passes, and the
  comment names the PR, merger and key.
- The same app flipped to `strict`: fails on an unsigned PR commit.
- A PR merged by a login not in `allowedMergers`: fails, naming the merger.
- `/retest provenance-`: the same result.
- A gitsign-signed commit pushed to `main`: passes on the gitsign path.
- `kubectl auth can-i get configmaps/release-signing-policy -n <catalog-namespace> --as=system:serviceaccount:<app-namespace>:pipeline-runner`
  is `yes`; `create`/`update` are `no`.
