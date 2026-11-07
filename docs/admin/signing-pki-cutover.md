# Signing PKI cutover

Status: **Built, not live** (C-2a, 2026-11-07). Everything below is off until a cluster's flags are flipped; with the flags
off the platform behaves exactly as [image-signing.md](image-signing.md) describes. The cutover has not been run on any
cluster. Statements about how External Secrets or Argo CD behave during it are marked Unverified where no cluster has
shown them.

## What changes

Today each cluster mints its own signing material inside the cluster: `hooks/fulcio-bootstrap-job.yaml` makes an Ed25519
root (`<cluster>-fulcio-root`, 100 years) and `hooks/rekor-signer-bootstrap-job.yaml` makes Rekor's key. After the cutover
the signing material is the owner's, from a key ceremony:

| Piece | Today | After |
|---|---|---|
| Root | the cluster's own Ed25519 certificate, in `fulcio-secret` | **Hangar Root CA**: ECDSA P-384, 10 years, pathlen 1. Offline: never in a cluster, Infisical or git |
| Fulcio CA | the same root signs leaves | **Hangar Fulcio `<cluster>`**: ECDSA P-256, 2 years, pathlen 0, code signing. Its key is PKCS#8 encrypted; key and passphrase sit in Infisical and reach Fulcio through ExternalSecrets. Fulcio serves the chain (intermediate, then root) as `cert.pem` |
| Rekor key | generated in-cluster (`rekor-signer`) | the owner's P-256 PKCS#8 key, through Infisical and an ExternalSecret |
| What verifiers trust | `fulcio-root-ca` (one cert) and Rekor's live `/api/v1/log/publicKey` | `hangar-trusted-root`: a Sigstore TrustedRoot listing every Fulcio chain and Rekor key with its validity window. The legacy root and the old Rekor key stay in it with windows that close at the cutover, so old signatures keep verifying |

The switch is `sigstore.managedKeys.enabled` in `glidepath-control-plane` (default false). Apron sets it from
`signing.managedKeys` ([apron docs/signing-pki.md](https://github.com/<owner>/apron/blob/main/docs/signing-pki.md)).

### What each consumer does

| Consumer | Behaviour |
|---|---|
| Tasks `verify-image-provenance`, `verify-sast-attestation`, `qa-gate`, `api-compat-gate`, `generate-sbom`, `attest-api-spec` | still build a single-root trust from `fulcio-root-ca` and Rekor's live key, then **replace it with `hangar-trusted-root`'s `trusted-root.json` when that ConfigMap exists**. With it, a legacy signature verifies against the legacy root and key, a new one against the new chain and key, each only while its window was open at Rekor's signed time |
| `glidepath-api` evidence (image attestations in Tower) | the same: a `hangar-trusted-root` that exists is the whole trust material. One that exists but does not parse is an error on every attestation, never a quiet fall back |
| Tekton Chains | signs through Fulcio at `http://fulcio-server.fulcio-system.svc`: no change; the new CA signs the leaves |
| Fulcio's `config.yaml` (issuers) | unchanged: still written by the `fulcio-bootstrap` hook, which with managed keys only creates the namespaces and keeps `fulcio-config` current |
| Clearance's keyless attestations | Fulcio issues the core a certificate; `clearance attest verify --trust-root trusted-root.json` checks it ([clearance docs/core.md](https://github.com/<owner>/clearance/blob/main/docs/core.md#trusted-root-c-2a)) |
| gitsign (`verify-commit-signature`) | not affected: it verifies commit signatures against its own identity rules, not this cluster's Fulcio |
| Kyverno policies | none read the Fulcio root or Rekor key in this chart; Flight's image-signature rules name subjects and issuers, not roots |
| Tower's own provenance view | reads the same `fulcio-root-ca` and Rekor's live key (Tower repo): until Tower reads `hangar-trusted-root`, a pre-cutover signature shows unverified there. Not changed here |

## The procedure

### 1. The owner's ceremony output lands in Infisical

The owner creates these in project `glidepath-hangar-dev`, environment `shared`, path `/`, readable by
`platform-secret-store`. Names derive from `clusterName`; set `sigstore.managedKeys.fulcioChainKey`, `fulcioKeyKey`,
`fulcioKeyPasswordKey` and `rekorSignerKey` to use others.

| Infisical key | Holds |
|---|---|
| `hangar-fulcio-hangar-dev-chain` | the chain PEM: "Hangar Fulcio hangar-dev", then "Hangar Root CA" |
| `hangar-fulcio-hangar-dev-key` | the intermediate's encrypted private key (PKCS#8 PEM) |
| `hangar-fulcio-hangar-dev-key-password` | its passphrase |
| `hangar-rekor-hangar-dev-signer` | Rekor's private key (P-256, PKCS#8 PEM) |

Nobody but the owner creates these, and no key passes through git, a chat or a terminal log. The root's private key is not
among them.

Before the flip, while the old Fulcio and Rekor still run, collect the two legacy **public** items:

```sh
kubectl --context hangar-dev -n platform-catalog get cm fulcio-root-ca -o jsonpath='{.data.cert\.pem}' > legacy-fulcio-root.pem
kubectl --context hangar-dev -n rekor-system port-forward svc/rekor-server 3000:80 &   # then:
curl -s http://localhost:3000/api/v1/log/publicKey > legacy-rekor.pub
```

### 2. The public PEMs are committed

The owner commits them to the cluster's repo under `signing-pki/` (or hands them over and they are added): `root.pem`,
`intermediate.pem`, `rekor.pub` (`openssl pkey -in signer.pem -pubout`), `legacy-fulcio-root.pem`, `legacy-rekor.pub`.
Then, with `clearance` (the release that contains jfillman/clearance#77) on the PATH:

```sh
hack/build-trust-root.sh <cluster repo> --new-start 2026-11-10T00:00:00Z --legacy-end 2026-11-10T04:00:00Z
```

`--new-start`: when the new chain and key start being trusted (the publication time is fine). `--legacy-end`: **after the
last signature the old Fulcio and Rekor will make**, so the flip time plus a margin; a legacy signature made after it is
refused. The script writes `signing-pki/fulcio-chain.pem` and `signing-pki/trusted-root.json`. Read
`trusted-root.json` before committing: two Fulcio chains and two Rekor keys, the legacy ones with an `end`, every `baseUrl`
containing `rekor-server.rekor-system.svc` (Rekor names its checkpoint signer by that host).

### 3. Flip the flags

In the cluster's `cluster.yaml`, in this order, as separate commits so each can be reverted alone:

```yaml
signing:
  trustedRoot: true      # a: publish hangar-trusted-root. Safe before anything else changes
  managedKeys: true      # b: Fulcio and Rekor take the owner's keys
clearance:
  attestation:
    keyless: true        # c: Clearance's core signs attestations through Fulcio and Rekor
```

Also bump `versions.glidepath` to a tag that contains this chart, and `versions.clearance` to a release that accepts
in-cluster `http` Fulcio/Rekor URLs (the clearance release that contains jfillman/clearance#77) before (c).

### 4. Render, diff and sync

1. `apron render` (`python3 hack/render.py --cluster cluster.yaml`), then diff the cluster repo. (a) adds
   `sigstore.managedKeys.trustedRootJson`; (b) adds `enabled` and `rootCaPem`; (c) adds the Clearance `attestation:` block
   and two egress rules. Nothing else may change. (An Apron render with all three flags off is byte-identical to before. The chart's own render with the flag off differs from the previous chart only by the added `hangar-trusted-root` name in the read grant `allow-fulcio-root-ca-read`, and comments.)
2. Sync `glidepath-control-plane`. After (b) expect: ExternalSecrets `fulcio-secret`, `fulcio-server-config` (fulcio-system)
   and `rekor-signer` (rekor-system) Ready; `fulcio-root-ca` and `hangar-trusted-root` in platform-catalog; no
   `rekor-signer-bootstrap` Job. Check `kubectl get externalsecret -A` is `SecretSynced` before restarting anything.
3. Restart both servers so they read the new keys (the Secrets are read at start): `kubectl -n fulcio-system rollout restart
   deploy/fulcio-server` and `kubectl -n rekor-system rollout restart deploy/rekor-server`.
4. Sync `clearance` for (c). The core restarts with `/readyz` reporting `attestation: {enabled: true, backend: keyless}`.

An existing `fulcio-secret` and `rekor-signer` made by the old hooks are replaced by the ExternalSecrets' output
(Unverified: confirm the ExternalSecret reports `SecretSynced` and the old Secret's `cert.pem` is now the chain). The old
private keys are then gone from the cluster; their public halves live on in `trusted-root.json`.

### 5. Verify

Do all three. None is optional.

**a. A new Chains signature verifies against the new chain.** Run a build of a scratch app, then:

```sh
kubectl -n platform-catalog get cm hangar-trusted-root -o jsonpath='{.data.trusted-root\.json}' > trusted-root.json
curl -s http://fulcio-server.fulcio-system.svc/api/v2/trustBundle | jq -r '.chains[0].certificates | length'   # 2: intermediate, root
cosign verify-attestation --type slsaprovenance --trusted-root trusted-root.json --insecure-ignore-sct \
  --certificate-oidc-issuer https://hangarplatform.dev/oidc/hangar-dev \
  --certificate-identity-regexp '^https://kubernetes\.io/namespaces/app-.*-cicd/serviceaccounts/.*$' \
  ghcr.io/<owner>/<app>@sha256:<new digest>
```

The leaf's issuer is `CN=Hangar Fulcio hangar-dev` (Tower's attestation view, or `openssl x509 -noout -issuer` on the
certificate in the attestation). The release gates (`verify-image-provenance`, `qa-gate`) pass on that build.

**b. An old signature still verifies through the legacy entries.** The same command against an image signed before the
flip (`<old digest>`) succeeds, and `verify-image-provenance` on a release made before the cutover still passes. If it
fails with "not enough verified log entries" or "certificate chain", the legacy root or Rekor key is missing from
`trusted-root.json`, or `--legacy-end` was earlier than the signature.

**c. A Clearance attestation verifies against the trusted root.** After a deploy reports an applied outcome:

```sh
curl -s https://clearance.<domain>/v1/clearances/<id>/attestation > bundle.json
clearance attest verify --file bundle.json --trust-root trusted-root.json \
  --identity https://kubernetes.io/namespaces/autopilot-system/serviceaccounts/clearance-core \
  --issuer https://hangarplatform.dev/oidc/hangar-dev --subject sha256:<manifest tree>
```

Exit 0 and a `rekor entry <n> verified` line. (The identity is the core's service account: Fulcio's `type: kubernetes`
issuer turns its token's subject into that URI; Fulcio's config needs no change because the cluster's issuer is already
listed with client-id `sigstore` and there is no subject rule.)

### 6. Rollback

Roll back in the reverse order; each step is its own commit.

- **Clearance keyless** (`clearance.attestation.keyless: false`): the core stops attesting; existing attestations stay
  valid. Safe at any time.
- **Trusted root** (`signing.trustedRoot: false`): verifiers go back to one root and Rekor's live key. Safe only while no
  signature that needs the other generation matters: after the flip, legacy signatures stop verifying (no legacy root in
  `fulcio-root-ca`), or the managed ones do, depending on which key set is live.
- **Managed keys** (`signing.managedKeys: false`): the hooks run again. Because the ExternalSecrets owned the Secrets,
  deleting them leaves nothing for the hooks to keep, so they mint a **third**, new self-generated root and Rekor key (the
  old ones were replaced at the flip and are gone). That is a forward fix, not a restore: add the new root's public
  certificate and Rekor's new public key to `signing-pki/` as another entry (a later `start`), rebuild
  `trusted-root.json`, and publish it; otherwise verifiers that prefer `hangar-trusted-root` refuse the new signatures.
  Also confirm `fulcio-root-ca` exists after the sync (Argo CD may prune the ConfigMap the chart owned: sync again so the
  hook recreates it). Unverified on a live cluster.

To abandon the whole thing before step 3(b), set nothing: with the flags off the chart renders as before.

## Rotation and expiry

The intermediate lives two years. Before it expires the owner issues a new one and a new chain, adds it to Infisical, adds
its certificate to `signing-pki/` with the old one's window closing after the swap, rebuilds `trusted-root.json`, and
restarts Fulcio. The same applies to a new Rekor key. The root's ten years is the outer bound for everything under it.
