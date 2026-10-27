#!/usr/bin/env bash
# Ground/Flight registries, phase 1 (ADR-0029): the catalog's registries/promotion values as rendered into the release
# tasks. Defaults must reproduce the old hardcoded behaviour; pinDigest writes release.image.digest/source (and removes
# them again when turned off); promotion.enabled pins the Flight repository and refuses to render without pinDigest;
# the release gates read their evidence from release.image.source. Runs the rendered bash against fixture release files
# with real yq. Needs helm, yq (mikefarah v4) and python3 (pyyaml).
# Usage: charts/glidepath-catalog/tests/registries_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"
D=sha256:53de992b7d3b3f5965a10695b5b04a02cc0558807c0ad3a8f1c10971c3bd762d
EX=charts/glidepath-catalog/tests/values.example.yaml

render() { # out-file show-only-template [helm args...]
  local out=$1 tmpl=$2; shift 2
  helm template t charts/glidepath-catalog -f "$EX" --show-only "templates/$tmpl" "$@" > "$out" 2>&1
}
script_of() { # rendered-task-file step-index -> the step's script
  python3 -c "
import sys,yaml
t=yaml.safe_load(open('$1'))
steps=t['spec']['steps']
print(steps[$2]['script'])"
}
pin_fn() { # rendered task file -> just the release_image_pin function
  script_of "$1" "${2:-0}" | sed -n '/^ *release_image_pin() {/,/^ *}$/p'
}
release_file() {
  printf 'release:\n  image:\n    repository: ghcr.io/o/app\n    tag: 1.0.0-abc\n%s' "${1:-}" > "$work/rel.yaml"
}

# --- defaults: today's behaviour --------------------------------------------------------------------------------
render "$work/onb.yaml" tasks/deliver-onboarding-files.yaml
grep -q 'image_repo="ghcr.io/${ground_ns:-${GITHUB_OWNER}}/${APP_NAME}"' "$work/onb.yaml" \
  || { echo "FAIL: default onboarding image repo is not ghcr.io/<owner>/<app>"; fail=1; }
render "$work/rir.yaml" tasks/resolve-image-ref.yaml
script_of "$work/rir.yaml" 0 | grep -q "printf '%s' none" || { echo "FAIL: default resolve-image-ref does not write image-digest=none"; fail=1; }
script_of "$work/rir.yaml" 0 | grep -q "oras resolve \"\$1\"" && { echo "FAIL: default resolve-image-ref reads the registry for a digest"; fail=1; }

render "$work/orp.yaml" tasks/open-release-pr.yaml
pin_fn "$work/orp.yaml" > "$work/pin-off.sh"
grep -q "^release_image_pin() {" "$work/pin-off.sh" || { echo "FAIL: could not extract release_image_pin (off)"; fail=1; }
release_file "    digest: $D
    source: ghcr.io/o/app
"
( source "$work/pin-off.sh"; release_image_pin "$work/rel.yaml" ghcr.io/o/app none ) \
  || { echo "FAIL: pin off errored"; fail=1; }
[ "$(yq '.release.image | has("digest") or has("source")' "$work/rel.yaml")" = false ] \
  || { echo "FAIL: pin off left a stale digest/source in the release file"; fail=1; }
[ "$(yq '.release.image.tag' "$work/rel.yaml")" = 1.0.0-abc ] || { echo "FAIL: pin off touched the tag"; fail=1; }
script_of "$work/orp.yaml" 0 | grep -q 'registries.flight\|flight_ns=' && { echo "FAIL: default open-release-pr pins a Flight repository"; fail=1; }

# --- pinDigest on -------------------------------------------------------------------------------------------------
render "$work/rir-on.yaml" tasks/resolve-image-ref.yaml --set promotion.pinDigest=true
script_of "$work/rir-on.yaml" 0 | grep -q 'oras resolve "$1"' || { echo "FAIL: pinDigest resolve-image-ref does not read the digest"; fail=1; }
for t in open-release-pr deploy-manifests; do
  render "$work/$t-on.yaml" tasks/$t.yaml --set promotion.pinDigest=true
  pin_fn "$work/$t-on.yaml" > "$work/pin-on.sh"
  grep -q "^release_image_pin() {" "$work/pin-on.sh" || { echo "FAIL: could not extract release_image_pin from $t"; fail=1; continue; }
  release_file
  ( source "$work/pin-on.sh"; release_image_pin "$work/rel.yaml" ghcr.io/ground/app "$D" ) || { echo "FAIL: $t pin on errored"; fail=1; }
  [ "$(yq '.release.image.digest' "$work/rel.yaml")" = "$D" ] || { echo "FAIL: $t did not write the digest"; fail=1; }
  [ "$(yq '.release.image.source' "$work/rel.yaml")" = ghcr.io/ground/app ] || { echo "FAIL: $t did not write the source"; fail=1; }
  release_file
  if ( source "$work/pin-on.sh"; release_image_pin "$work/rel.yaml" ghcr.io/ground/app none ) 2>"$work/err"; then
    echo "FAIL: $t pinned without a digest"; fail=1
  else grep -q "not sha256:<hex>" "$work/err" || { echo "FAIL: $t missing-digest message: $(cat "$work/err")"; fail=1; }; fi
  script_of "$work/$t-on.yaml" 0 | grep -q 'release_image_pin "${manifest}"' || { echo "FAIL: $t never calls release_image_pin"; fail=1; }
done

# --- deploy-manifests waits for the image the chart renders (repo:tag@digest when pinned) ----------------------
want_of() { # rendered deploy-manifests -> the lines that pick the Rollout image to wait for
  script_of "$1" 0 | sed -n '/^ *want_image="\${IMAGE_REF}"$/,/want_image="\${IMAGE_REF}@/p'
}
want_of "$work/deploy-manifests-on.yaml" > "$work/want.sh"
[ "$(wc -l < "$work/want.sh")" -eq 3 ] || { echo "FAIL: could not extract the wait-image lines: $(cat "$work/want.sh")"; fail=1; }
release_file "    digest: $D
"
[ "$(manifest="$work/rel.yaml" IMAGE_REF=ghcr.io/o/app:1.0.0-abc; source "$work/want.sh"; echo "$want_image")" = "ghcr.io/o/app:1.0.0-abc@$D" ] \
  || { echo "FAIL: a pinned deploy does not wait for repo:tag@digest"; fail=1; }
release_file
[ "$(manifest="$work/rel.yaml" IMAGE_REF=ghcr.io/o/app:1.0.0-abc; source "$work/want.sh"; echo "$want_image")" = ghcr.io/o/app:1.0.0-abc ] \
  || { echo "FAIL: an unpinned deploy does not wait for repo:tag"; fail=1; }
script_of "$work/deploy-manifests-on.yaml" 0 | grep -q -- '--arg img "${want_image}"' || { echo "FAIL: the Rollout check does not use want_image"; fail=1; }

# --- promotion.enabled -------------------------------------------------------------------------------------------
render "$work/bad.yaml" tasks/open-release-pr.yaml --set promotion.enabled=true
grep -q "promotion.enabled needs promotion.pinDigest" "$work/bad.yaml" || { echo "FAIL: promotion without pinDigest rendered"; fail=1; }
render "$work/orp-fl.yaml" tasks/open-release-pr.yaml --set promotion.enabled=true --set promotion.pinDigest=true \
  --set registries.flight.namespace=flight-org
script_of "$work/orp-fl.yaml" 0 | grep -q 'image_repo="ghcr.io/${flight_ns:-${owner}}/${APP_NAME}"' \
  || { echo "FAIL: promotion does not pin the Flight repository"; fail=1; }
script_of "$work/orp-fl.yaml" 0 | grep -q "flight_ns='flight-org'" || { echo "FAIL: flight namespace not rendered"; fail=1; }
script_of "$work/orp-fl.yaml" 0 | grep -q 'source_repo="${image_repo}"' || { echo "FAIL: source is not taken from the Ground image"; fail=1; }

# --- the gates read evidence from source -------------------------------------------------------------------------
render "$work/rpi.yaml" stepactions/resolve-promoted-image.yaml
expr="$(python3 -c "
import yaml
s=yaml.safe_load(open('$work/rpi.yaml'))['spec']['script']
l=[x for x in s.splitlines() if x.strip().startswith('image_repo=')][0]
print(l.split(\"'\")[1])")"
release_file "    source: ghcr.io/ground/app
"
[ "$(yq -r "$expr" "$work/rel.yaml")" = ghcr.io/ground/app ] || { echo "FAIL: gates do not prefer release.image.source"; fail=1; }
release_file
[ "$(yq -r "$expr" "$work/rel.yaml")" = ghcr.io/o/app ] || { echo "FAIL: gates lost release.image.repository without a source"; fail=1; }

# --- glidepath-app preview image ---------------------------------------------------------------------------------
cat > "$work/app.yaml" <<'YAML'
platformIdentity: { appName: app, type: app, githubOwner: o, catalogNamespace: platform-catalog,
  gitopsRepoUrl: https://github.com/o/gitops-app, appRepoUrl: https://github.com/o/app }
platformCicdRepoUrl: https://github.com/o/glidepath.git
devClusterName: dev
defaultChart: { repoURL: https://github.com/o/airframe.git, path: charts/airframe-application, targetRevision: v0.0.0 }
ephemeralEnvironments: { pullRequest: { enabled: true } }
YAML
preview_repo() { helm template t charts/glidepath-app -f "$work/app.yaml" "$@" --show-only templates/argocd/ephemeral-envs.yaml 2>&1 \
  | grep -m1 -E '^ +repository: ' | awk '{print $2}'; }
[ "$(preview_repo)" = ghcr.io/o/app-pr ] || { echo "FAIL: default preview repository is $(preview_repo), not ghcr.io/o/app-pr"; fail=1; }
[ "$(preview_repo --set registries.ground.host=reg.example --set registries.ground.namespace=g)" = reg.example/g/app-pr ] \
  || { echo "FAIL: registries.ground does not set the preview repository"; fail=1; }

[ "$fail" = 0 ] && echo ok
exit "$fail"
