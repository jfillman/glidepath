# Commit fixtures for verify_commit_signature_test.sh

Raw commit objects (`git cat-file commit <sha>`), each named by its own object SHA so the test can
check the file still hashes to it. All but the last are real commits recorded 2026-10-20.

| SHA | What it is | Used for |
|---|---|---|
| `7455b765…` | jfillman/backstage merge commit of PR #171, GitHub web-flow key `B5690EEEBB952194` (two parents) | the GitHub-merge path passes |
| `4922bf52…` | PR #171's head, unsigned | strict mode fails on it |
| `6872b862…` | jfillman/baggage-api, gitsign-signed, pushed straight to `main` | the gitsign path (gitsign itself is stubbed) |
| `4416b513…` | jfillman/glidepath squash merge of #143, web-flow key (one parent) | a squash merge passes |
| `3bb62d47…` | cli/cli squash merge from 2023-11-29, GitHub's retired web-flow key `4AEE18F83AFDEB23` | a pre-rotation signature still passes |
| `4b0bd2bd…` | jfillman/gitops-backstage "Add clusterType to values.yaml": web editor, web-flow key, no PR | fails: not a PR's merge |
| `f07b3ee7…` | jfillman/gitops-boarding-api "Merge branch 'main' into …": GitHub's update-branch button, web-flow key | fails: not a PR's merge |
| `1338a87a…` | made for the test: committer `GitHub <noreply@github.com>`, signed by a throwaway key claiming to be GitHub | fails: unknown key |
