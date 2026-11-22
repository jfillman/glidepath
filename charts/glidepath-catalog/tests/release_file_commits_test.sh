#!/usr/bin/env bash
# Platform release-file commits (docs/admin/clearance-gate.md, "Platform release-file commits").
#  1. deploy-manifests' real apply-and-wait step commits the release file through GitHub's Git Data API (a stand-in served
#     from a local bare repository with real git plumbing), never `git push`, with no author or committer in the request
#     (what GitHub needs to sign it as the App). main moving under it re-reads and commits again; a rerun commits nothing.
#  2. clearance-gate's real register step names the skipped commits on their own line and says what a DG-PROVENANCE denial
#     means for this release, not the agent case.
# The rule itself (the four conditions) is tested in clearance_gate_release_source_test.sh, section 15.
# Needs helm, git, jq, yq, python3 (pyyaml). Usage: charts/glidepath-catalog/tests/release_file_commits_test.sh
set -u
cd "$(dirname "$0")/../../.."
root="$(pwd)"
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
step() { # <template> <step name> -> the step's script
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only "templates/tasks/$1.yaml" \
    | python3 -c "import sys,yaml; d=yaml.safe_load(sys.stdin.read()); print([s for s in d['spec']['steps'] if s['name']=='$2'][0]['script'])"
}
REAL_GIT="$(command -v git)"; export REAL_GIT

# --- 1. deploy-manifests -------------------------------------------------------------------------------------------
step deploy-manifests apply-and-wait > "$d/deploy.sh" || { echo "FAIL: render deploy-manifests"; exit 1; }
expect "deploy-manifests no longer pushes" "$(grep -v '^ *#' "$d/deploy.sh" | grep -c 'git push')" 0
mkdir -p "$d/bin" "$d/lib"
cp catalog/lib/retry.sh "$d/lib/retry.sh"
echo 'github_app_installation_token() { echo tok; }' > "$d/lib/github-app.sh"
# git: a clone of github.com/<owner>/<repo> clones the local origin instead; a push is recorded (and refused).
cat > "$d/bin/git" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == clone ]]; then
  args=(); for a in "$@"; do [[ "$a" == https://* ]] && a="file://${ORIGIN}"; args+=("$a"); done
  exec "${REAL_GIT}" "${args[@]}"
fi
[[ "$1" == push ]] && { echo push >> "${LOG}/push"; exit 1; }
exec "${REAL_GIT}" "$@"
STUB
# curl: GitHub's Git Data API over ${ORIGIN}. Prints the body, then the status (the script's -w '\n%{http_code}').
cat > "$d/bin/curl" <<'STUB'
#!/usr/bin/env bash
method=GET; data=""; url=""; ct=""; outf=""
while (( $# )); do
  case "$1" in -X) method="$2"; shift ;; --data) data="${2#@}"; shift ;; --output) outf="$2"; shift ;; -H) [[ "$2" == "Content-Type: application/json" ]] && ct=json; shift ;; http*) url="$1" ;; esac; shift
done
# like curl --output F -w '%{http_code}': the body to F, the status to stdout
reply() { [[ -n "${outf}" ]] || { echo "stub: no --output" >&2; exit 2; }; printf '%s' "$1" > "${outf}"; printf '%s' "$2"; }
g() { "${REAL_GIT}" -C "${ORIGIN}" "$@"; }
path="${url#https://api.github.com/repos/o/shop/}"
echo "${method} ${path} ${ct:-none} ${data:+body}" >> "${LOG}/calls"
case "${method} ${path}" in
  "GET git/commits/"*)
    s="${path#git/commits/}"; t="$(g rev-parse "${s}^{tree}" 2>/dev/null)" || { reply '{}' 404; exit 0; }
    reply "$(jq -nc --arg s "$s" --arg t "$t" '{sha: $s, tree: {sha: $t}}')" 200 ;;
  "POST git/trees")
    cp "${data}" "${LOG}/tree.json"
    export GIT_INDEX_FILE="${LOG}/index.$$"; rm -f "${GIT_INDEX_FILE}"
    g read-tree "$(jq -r .base_tree "${data}")"
    n="$(jq '.tree | length' "${data}")"
    for ((i = 0; i < n; i++)); do
      p="$(jq -r ".tree[$i].path" "${data}")"; m="$(jq -r ".tree[$i].mode" "${data}")"
      if [[ "$(jq -r ".tree[$i].sha" "${data}")" == null && "$(jq -r ".tree[$i] | has(\"content\")" "${data}")" == false ]]; then
        g update-index --force-remove "$p"
      else
        b="$(jq -j ".tree[$i].content" "${data}" | g hash-object -w --stdin)"; g update-index --add --cacheinfo "$m,$b,$p"
      fi
    done
    reply "$(jq -nc --arg t "$(g write-tree)" '{sha: $t}')" 201 ;;
  "POST git/commits")
    cp "${data}" "${LOG}/commit.json"
    c="$(GIT_AUTHOR_NAME='cicd-pac[bot]' GIT_AUTHOR_EMAIL=bot@x GIT_COMMITTER_NAME='cicd-pac[bot]' GIT_COMMITTER_EMAIL=bot@x \
         g commit-tree "$(jq -r .tree "${data}")" -p "$(jq -r '.parents[0]' "${data}")" -m "$(jq -r .message "${data}")")"
    reply "$(jq -nc --arg c "$c" '{sha: $c}')" 201 ;;
  "PATCH git/refs/heads/main")
    cp "${data}" "${LOG}/ref.json"
    if [[ -f "${RACE:-/nonexistent}" ]]; then   # someone else's commit lands on main first
      [[ -n "${RACE_ALWAYS:-}" ]] || rm -f "${RACE}"
      w="$(mktemp -d)"; "${REAL_GIT}" clone -q "${ORIGIN}" "$w/c"
      echo "x$RANDOM" >> "$w/c/README.md"; "${REAL_GIT}" -C "$w/c" -c user.name=dana -c user.email=d@x commit -qam "dana's change"
      "${REAL_GIT}" -C "$w/c" push -q origin HEAD:main
    fi
    [[ -n "${REF422:-}" ]] && { reply '{"message":"Reference update failed: validation"}' 422; exit 0; }
    new="$(jq -r .sha "${data}")"
    if g merge-base --is-ancestor "$(g rev-parse main)" "${new}"; then g update-ref refs/heads/main "${new}"; reply '{}' 200
    else reply '{"message":"Update is not a fast forward"}' 422; fi ;;
  *) reply '{}' 404 ;;
esac
STUB
cat > "$d/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
[[ "$*" == *annotate* ]] && exit 0
jq -nc --arg i "${IMAGE_REF}" '{status: {phase: "Healthy"}, spec: {template: {spec: {containers: [{image: $i}]}}}}'
STUB
printf '#!/usr/bin/env bash\nexit 0\n' > "$d/bin/sleep"
chmod +x "$d/bin/"*

origin() { # <dev.yaml content>: a fresh origin with cicd.yaml and the human env file
  export ORIGIN="$d/origin-$RANDOM.git" LOG="$d/log-$RANDOM"; mkdir -p "${LOG}"
  local w; w="$(mktemp -d)"; "${REAL_GIT}" init -q -b main "$w"
  mkdir -p "$w/glidepath/envs"; printf 'deploy:\n  releaseFile: glidepath/envs/{env}.release.yaml\n' > "$w/cicd.yaml"
  printf '%s' "$1" > "$w/glidepath/envs/dev.yaml"; echo readme > "$w/README.md"
  "${REAL_GIT}" -C "$w" add -A; "${REAL_GIT}" -C "$w" -c user.name=dana -c user.email=d@x commit -qm init
  "${REAL_GIT}" clone -q --bare "$w" "${ORIGIN}"
}
deploy() { # <image ref>
  PATH="$d/bin:$PATH" PLATFORM_LIB="$d/lib" GITHUB_TOKEN_BROKER_URL=http://broker IMAGE_REF="$1" IMAGE_DIGEST=none ENV=dev \
    APP_NAME=shop APP_TYPE=app GIT_URL=https://github.com/o/shop bash "$d/deploy.sh" > "${LOG}/out" 2>&1
}
o() { "${REAL_GIT}" -C "${ORIGIN}" "$@"; }

origin $'envName: dev\n'
deploy ghcr.io/o/shop:1.2-abc; rc=$?
expect "a deploy succeeds" "$rc" 0
expect "the commit is on main, made through the API" "$(o log -1 --format='%s|%cn' main)" "Deploy shop to dev: ghcr.io/o/shop:1.2-abc|cicd-pac[bot]"
expect "the release file carries the image" "$(o show main:glidepath/envs/dev.release.yaml | yq -r '.release.image.repository + ":" + .release.image.tag')" "ghcr.io/o/shop:1.2-abc"
expect "one commit on top of the clone's tip" "$(o rev-list --count main)" 2
expect "the commit request names no author, committer or signature (GitHub signs it as the App)" "$(jq -c 'keys' "${LOG}/commit.json")" '["message","parents","tree"]'
expect "the tree holds only the release file, over the parent's tree" "$(jq -r '[.tree[].path] | join(",")' "${LOG}/tree.json"),$(jq -r '.base_tree | length' "${LOG}/tree.json")" "glidepath/envs/dev.release.yaml,40"
expect "every write sends its JSON body; reads send none" "$(awk '{print $1, $3, $4}' "${LOG}/calls" | sort -u | paste -sd ';' -)" "GET none ;PATCH json body;POST json body"
expect "the ref moves without force" "$(jq -c .force "${LOG}/ref.json")" false
expect "git push is never run" "$([[ -f "${LOG}/push" ]] && echo yes || echo no)" no
expect "the other files are untouched" "$(o show main:README.md)" readme
before="$(o rev-parse main)"
deploy ghcr.io/o/shop:1.2-abc; rc=$?
expect "a rerun of the same deploy: nothing to commit, succeeds" "$rc,$(o rev-parse main),$(grep -c 'nothing to commit' "${LOG}/out")" "0,${before},1"

# main moves between the clone and the ref update: re-read and commit again on top of it, keeping the other change.
origin $'envName: dev\n'; export RACE="${LOG}/race"; : > "${RACE}"
deploy ghcr.io/o/shop:1.3-def; rc=$?
expect "main moved once: retried and committed" "$rc,$(grep -c 'main moved since the clone' "${LOG}/out")" "0,1"
expect "the deploy lands on top of the other change, which is kept" "$(o log --format=%s main | head -2 | paste -sd '|' -)" "Deploy shop to dev: ghcr.io/o/shop:1.3-def|dana's change"
expect "the other change is not overwritten" "$(o show main:README.md | wc -l | tr -d ' ')" 2
origin $'envName: dev\n'; export RACE="${LOG}/race" RACE_ALWAYS=1; : > "${RACE}"
deploy ghcr.io/o/shop:1.4-aaa; rc=$?
expect "main keeps moving: fails after 3 attempts, the deploy is not written" "$rc,$(grep -c 'gave up after 3 attempts' "${LOG}/out"),$(o log --format=%s main | grep -c '^Deploy')" "1,1,0"
unset RACE RACE_ALWAYS
origin $'envName: dev\n'
REF422=1 deploy ghcr.io/o/shop:1.4-ccc; rc=$?
expect "a 422 that is not 'not a fast forward': no retry, the error is reported" "$rc,$(grep -c 'main moved since the clone' "${LOG}/out"),$(grep -c 'GitHub refused to move main' "${LOG}/out")" "1,0,1"

# the one-time move onto the split: the env file's image goes in the same commit, both paths in the one tree
origin $'envName: dev\nrelease:\n  image:\n    tag: old\n'
deploy ghcr.io/o/shop:1.5-bbb; rc=$?
expect "moving onto the split: one commit with both files" "$rc,$(jq -r '[.tree[].path] | sort | join(",")' "${LOG}/tree.json"),$(o show main:glidepath/envs/dev.yaml | yq '.release.image // "gone"')" "0,glidepath/envs/dev.release.yaml,glidepath/envs/dev.yaml,gone"

# --- 2. clearance-gate's comment -----------------------------------------------------------------------------------
PROTECTION_LIB="$(cat charts/glidepath-catalog/files/lib/protection.sh)"; export PROTECTION_LIB
r="$d/reg"; mkdir -p "$r/w/eval/hangar-prod-prod" "$r/secret" "$r/bin"; echo tok > "$r/secret/clearance-token"
step clearance-gate register \
  | sed -e "s|/work|$r/w|g" -e "s|\$(results.outcome.path)|$r/outcome|" -e "s|\$(results.findings-summary.path)|$r/summary|" \
        -e "s|\$(results.change-ids.path)|$r/ids|" -e "s|\$(results.joins.path)|$r/joins|" -e "s|/var/run/secrets/clearance|$r/secret|" > "$r/register.sh"
# Clearance: CORE=old refuses provenanceReason/unvouchedCommits as unknown fields (a core before clearance#213, strict
# decoding); otherwise unvouched-commits holds at tier 3 for two people and anything else is denied DG-PROVENANCE.
cat > "$r/bin/curl" <<'STUB'
#!/usr/bin/env bash
f=""; while (( $# )); do [[ "$1" == --data ]] && f="${2#@}"; shift; done
jq -c . "$f" >> "${REQS}"
if [[ "${CORE:-new}" == old ]] && jq -e 'has("provenanceReason") or has("unvouchedCommits")' "$f" >/dev/null; then
  printf '%s\n400' "{\"error\":\"DG-REQUEST\",\"detail\":\"unknown field 'provenanceReason'\"}"; exit 0
fi
if [[ "${CORE:-new}" == new ]] && [[ "$(jq -r '.provenanceReason // ""' "$f")" == unvouched-commits ]]; then
  printf '%s\n200' '{"id":"chg-9","state":"holding","tier":3,"protected":true,"reasons":[{"tier":3,"rule":"unvouched-commits","subject":"1 commit no merged pull request contains"}],"requirement":{"owner":1,"ownerGroup":"group:default/shop","outside":1,"outsideGroups":["group:default/admins"]},"approvals":[],"cleared_by":null,"deny_reason":null,"expires_at":null}'
  exit 0
fi
printf '%s\n200' '{"id":"chg-9","state":"denied","tier":1,"protected":true,"reasons":[],"requirement":null,"approvals":[],"cleared_by":null,"deny_reason":"DG-PROVENANCE: provenance unknown","expires_at":null}'
STUB
printf '#!/usr/bin/env bash\nexit 0\n' > "$r/bin/kubectl"; chmod +x "$r/bin/"*
export REQS="$r/reqs"
comment() { # <source.json content or ""> [<change.json extra fields>] -> the report; the requests sent are in $REQS
  echo "hangar-prod|prod|default|true" > "$r/w/plan.txt"; echo "hangar-prod|prod|true" > "$r/w/declared.txt"
  local x="${2:-}"; [[ -n "$x" ]] || x='{}'
  : > "${REQS}"
  jq -nc --argjson x "$x" '{cluster: "hangar-prod", env: "prod", pr: 7, treeDigest: "sha256:feed", proposerKind: "unknown"} + $x' > "$r/w/eval/hangar-prod-prod/change.json"
  rm -f "$r/w/source.json"; [[ -n "$1" ]] && printf '%s' "$1" > "$r/w/source.json"
  PATH="$r/bin:$PATH" APP_NAME=shop CLEARANCE_URL=http://clearance TOWER_URL= PIPELINE_RUN= REPO_URL=u NAMESPACE=n bash "$r/register.sh" >/dev/null 2>&1
  cat "$r/w/report.md"
}
src='{"applicable":true,"resolved":true,"mode":"range","sourceRepo":"o/shop","previousCommit":"1111111111111111111111111111111111111111","releasedCommit":"2222222222222222222222222222222222222222","commits":6,"authors":[{"login":"dana","prs":1}],"mergers":[{"login":"pat","prs":1}],"prs":[{"number":5,"author":"dana","merger":"pat","identity":"not-agent","agent":""}],"proposer":"user:dana","proposerKind":"unknown","proposerRule":"x","provenance":"unknown","promoter":"pat"}'
out="$(comment "$(jq -c '.noPr = ["3333333333333333333333333333333333333333"] | .platformCommits = ["4444444444444444444444444444444444444444","5555555555555555555555555555555555555555"]' <<<"$src")")"
expect "the skipped commits are on their own line" "$(grep -c '^- platform release-file commits, not counted: `4444444`, `5555555`' <<<"$out")" 1
expect "the commit no PR contains is still named" "$(grep -c 'No merged pull request contains `3333333333333333333333333333333333333333`' <<<"$out")" 1
expect "DG-PROVENANCE names the real cause: the commit no PR contains, not an agent" \
  "$(grep -c 'DG-PROVENANCE` denial here means provenance is unknown because the release carries 1 commit(s) no merged pull request contains' <<<"$out"),$(grep -c "looks like an agent" <<<"$out")" "1,0"
out="$(comment "$(jq -c '.noPr = [] | .platformCommits = []' <<<"$src")")"
expect "no platform commits: no such line" "$(grep -c 'platform release-file commits' <<<"$out")" 0
out="$(comment '{"applicable":true,"resolved":false,"reason":"no release record for the image"}')"
expect "an unresolved source: DG-PROVENANCE says so" "$(grep -c 'denial here means the source of this release could not be established' <<<"$out")" 1
out="$(comment '')"
expect "a person's PR (no source): the agent case, as before" "$(grep -c 'denial here means the PR looks like an agent' <<<"$out")" 1

# provenanceReason (clearance#213): the hold row says why; a core that refuses the field gets the change without it.
U=3333333333333333333333333333333333333333
usrc="$(jq -c --arg u "$U" '.noPr = [$u] | .provenanceReason = "unvouched-commits" | .unvouchedCommits = [$u]' <<<"$src")"
ufields="$(jq -nc --arg u "$U" '{provenanceReason: "unvouched-commits", unvouchedCommits: [$u]}')"
out="$(CORE=new comment "$usrc" "$ufields")"
expect "a core that knows it: one request, with the reason and the commits" "$(jq -sc 'map([.provenanceReason, (.unvouchedCommits | length)])' "${REQS}")" '[["unvouched-commits",1]]'
expect "the hold row says why (unvouched commits, tier 3)" "$(grep -c '^| ⏳ | hangar-prod/prod | 3 | holding |.*unvouched-commits (1 commit no merged pull request contains)' <<<"$out")" 1
expect "the source section says why it holds for two people" "$(grep -c 'Why it holds for two people:.*provenanceReason: unvouched-commits' <<<"$out")" 1
expect "no fallback note" "$(grep -c 'does not take `provenanceReason` yet' <<<"$out")" 0
out="$(CORE=old comment "$usrc" "$ufields")"
expect "a core before clearance#213: the refused request, then the same change without the two fields" \
  "$(jq -sc 'map(has("provenanceReason") or has("unvouchedCommits"))' "${REQS}")" '[true,false]'
expect "... is registered (denied, as before: fails closed), not an error" "$(grep -c '^| ❌ | hangar-prod/prod | 1 | denied |' <<<"$out"),$(cat "$r/outcome")" "1,denied"
expect "... and the comment says the field was not taken" "$(grep -c 'does not take `provenanceReason` yet' <<<"$out")" 1
out="$(CORE=old comment "$src" '{}')"
expect "nothing to send: one request, no note" "$(wc -l < "${REQS}" | tr -d ' '),$(grep -c 'does not take' <<<"$out")" "1,0"

[[ "${fail}" == 0 ]] && echo "all passed" || { echo "FAILED"; exit 1; }
