#!/usr/bin/env bash
# Who proposed a release (docs/admin/clearance-gate.md): the gate registers a pipeline release PR with the author(s) and
# provenance of the SOURCE change it releases, not with the pipeline's GitHub App. Runs files/lib/release-source.sh (the
# real function) against a stub kubectl (the release records), a stub curl (GitHub and Clearance's /v1/provenance) and
# real jq and yq, then the render step's mapping into the change request. Cases: a person's PR (unchanged), the normal
# range, several authors, mixed provenance (the weakest wins; confirmed only when every source PR is), a rollback, the
# same commit again, an unresolvable source, a commit with no PR, and forgery: a PR that claims a source it does not
# carry, extra files, extra commits, a changed digest, a changed field. Needs helm, jq, yq, python3 (pyyaml).
# Usage: charts/glidepath-catalog/tests/clearance_gate_release_source_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap '[[ -n "${KEEP:-}" ]] || rm -rf "$d"' EXIT
expect() { # <name> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi
}
sha() { printf '%s%s' "$(printf '%x' "$1")" "0000000000000000000000000000000000000000" | cut -c1-40; }
P="$(sha 100)"; R="$(sha 200)"                      # the previous release's commit and this release's
C1="$(sha 1)"; M1="$(sha 2)"; C2="$(sha 3)"; M2="$(sha 4)"   # commits of the range: C* non-merge, M* merge

# --- stubs ---------------------------------------------------------------------------------------------------------
mkdir -p "$d/bin"
cat > "$d/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
[[ -f "${RECORDS_FAIL:-/nonexistent}" ]] && exit 1
cat "${FIX}/records.json"
STUB
# GitHub: https://api.github.com/repos/<path> is the fixture file with every non-alphanumeric character as _ ; a missing
# fixture is an HTTP error (curl --fail). Clearance: POST .../v1/provenance answers ${FIX}/verdicts.json[<pr>] (absent: 503).
cat > "$d/bin/curl" <<'STUB'
#!/usr/bin/env bash
url=""; data=""
while (( $# )); do
  case "$1" in
    --data) data="$2"; shift ;;
    http*) url="$1" ;;
  esac
  shift
done
if [[ "${url}" == */v1/provenance ]]; then
  echo "${data}" >> "${FIX}/provenance-requests.jsonl"
  v="$(jq -c --arg pr "$(jq -r .pr <<<"${data}")" '.[$pr] // empty' "${FIX}/verdicts.json")"
  if [[ -n "${v}" ]]; then printf '%s\n200' "${v}"; else printf '%s\n503' '{}'; fi
  exit 0
fi
f="${FIX}/gh/$(sed -E 's#^https://api.github.com/repos/##; s#[^A-Za-z0-9]#_#g' <<<"${url}")"
[[ -f "${f}" ]] || { echo "stub: no fixture for ${url}" >&2; exit 22; }
cat "${f}"
STUB
chmod +x "$d/bin/"*

# The library under test, rendered the way the Task splices it.
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/clearance-gate.yaml > "$d/task.yaml" \
  || { echo "FAIL: render"; exit 1; }
cp "${LIB:-charts/glidepath-catalog/files/lib/release-source.sh}" "$d/lib.sh"
github_app_installation_token() { echo tok; }
export -f github_app_installation_token 2>/dev/null || true

# --- scenario builder ----------------------------------------------------------------------------------------------
gh() { # <repos/ path> <json>
  mkdir -p "$FIX/gh"; printf '%s' "$2" > "$FIX/gh/$(sed -E 's#[^A-Za-z0-9]#_#g' <<<"$1")"
}
commit() { # <sha> <parents> <login or ""> -> a compare/commit entry
  jq -nc --arg s "$1" --argjson p "$2" --arg l "$3" '{sha: $s, parents: [range($p)] | map({sha: "x"}), author: (if $l == "" then null else {login: $l} end), commit: {author: {name: $l}}}'
}
prpages() { # <pr> <opener> <head sha> <commit json array> <message for coauthors or ""> [merge commit sha] [merger] [merged_at]
  gh "o/shop/pulls/$1" "$(jq -nc --arg h "$3" --arg a "$2" --arg m "${6:-}" --arg g "${7:-pat}" --arg t "${8:-2026-11-13T00:0$1:00Z}" \
    '{head: {sha: $h}, user: {login: $a}, merged_by: {login: $g}, merged_at: $t, merge_commit_sha: $m}')"
  gh "o/shop/pulls/$1/commits?per_page=100&page=1" "$(jq -nc --argjson c "$4" --arg m "$5" '[$c[] | {sha, commit: {message: ("subject\n\n" + $m)}}]')"
  gh "o/shop/pulls/$1/files?per_page=100&page=1" '[{"filename": "src/a.ts"}]'
}
cpulls() { # <commit sha> <json array of PR numbers, or []>
  gh "o/shop/commits/$1/pulls?per_page=100&page=1" "$(jq -nc --argjson n "$2" '[$n[] | {number: ., merged_at: "2026-11-13T00:00:00Z"}]')"
}
scenario() { # <name>: the normal one: previous release P (merged), this release R, PRs 281 (C1, M1) and 282 (C2, M2), all by dana
  export FIX="$d/$1"; rm -rf "$FIX"; mkdir -p "$FIX/gh" "$FIX/head/c1/prod" "$FIX/base/c1/prod"
  jq -n --arg p "$P" --arg r "$R" '{items: [
    {data: {prUrl: "https://github.com/o/gitops-shop/pull/7", releaseId: "rel2:c1/prod", env: "prod", cluster: "c1", kind: "promote",
            image: ("ghcr.io/o/shop:1.0.2-" + $r[0:7]), imageRepository: "ghcr.io/x/shop", imageSource: "", imageDigest: "", appNamespace: "app-shop-cicd",
            gitRevision: $r, gitUrl: "https://github.com/o/shop", initiator: "pat", mergedAt: ""}},
    {data: {prUrl: "https://github.com/o/gitops-shop/pull/6", releaseId: "rel1:c1/prod", env: "prod", cluster: "c1", kind: "promote",
            image: ("ghcr.io/o/shop:1.0.2-" + $p[0:7]), gitRevision: $p, gitUrl: "https://github.com/o/shop", mergedAt: "2026-11-10T00:00:00Z"}}]}' > "$FIX/records.json"
  printf 'replicaCount: 2\nrelease:\n  image:\n    repository: ghcr.io/x/shop\n    tag: 1.0.2-%s\nreleaseTracking:\n  releaseId: rel1:c1/prod\n  cluster: c1\n  appNamespace: app-shop-cicd\n' "${P:0:7}" > "$FIX/base/c1/prod/release.yaml"
  printf 'replicaCount: 2\nrelease:\n  image:\n    repository: ghcr.io/x/shop\n    tag: 1.0.2-%s\nreleaseTracking:\n  releaseId: rel2:c1/prod\n  cluster: c1\n  appNamespace: app-shop-cicd\n' "${R:0:7}" > "$FIX/head/c1/prod/release.yaml"
  echo '[{"filename":"c1/prod/release.yaml","status":"modified"}]' > "$FIX/files.json"
  gh "o/gitops-shop/pulls/7" '{"commits": 1}'
  gh "o/shop/compare/$P...$R?per_page=1" '{"status":"ahead","total_commits":4}'
  gh "o/shop/compare/$P...$R?per_page=100&page=1" "$(jq -nc --argjson a "$(commit "$C1" 1 dana)" --argjson b "$(commit "$M1" 2 dana)" --argjson c "$(commit "$C2" 1 dana)" --argjson e "$(commit "$M2" 2 dana)" '{commits: [$a, $b, $c, $e]}')"
  cpulls "$C1" '[281]'; cpulls "$M1" '[281]'; cpulls "$C2" '[282]'; cpulls "$M2" '[282]'
  prpages 281 dana "$C1" "[{\"sha\":\"$C1\"}]" "Co-Authored-By: Claude <noreply@anthropic.com>" "$M1"
  prpages 282 dana "$C2" "[{\"sha\":\"$C2\"}]" "Co-Authored-By: Claude <noreply@anthropic.com>" "$M2"
  echo '{"281":{"identity":{"status":"unattested"}},"282":{"identity":{"status":"unattested"}}}' > "$FIX/verdicts.json"
}
resolve() { # -> prints the function's JSON; the files list is $FIX/files.json
  ( source "$d/lib.sh"; github_app_installation_token() { echo tok; }
    PATH="$d/bin:$PATH" NAMESPACE=app-shop-cicd APP_NAME=shop CLEARANCE_URL=http://clearance PIPELINE_LOGINS="${PIPELINE_LOGINS:-}" \
    release_source_resolve o gitops-shop 7 "$(cat "$FIX/files.json")" "$FIX/head" "$FIX/base" "${PRAUTHOR:-cicd-pac[bot]}" ) 2> "$FIX/stderr"
}
# the clearance token the lib reads
mkdir -p "$d/secret"; echo tok > "$d/secret/clearance-token"
sed -i.bak "s#/var/run/secrets/clearance#$d/secret#" "$d/lib.sh"; rm -f "$d/lib.sh.bak"
q() { jq -r "$1" <<<"$out"; }

# 1. The normal release: previous -> this release, two source PRs by one person, both with an AI co-author trailer.
scenario normal; out="$(resolve)"
expect "range: resolved" "$(q .resolved)" true
expect "range: mode and size" "$(q '[.mode, .commits] | join(",")')" "range,4"
expect "range: the proposer is the source author, not the PR author" "$(q '[.proposer, .proposerKind, (.agent // "none")] | join(",")')" "user:dana,person,none"
expect "range: unattested (AI co-author outside Clearance)" "$(q .provenance)" unattested
expect "range: both source PRs were asked about" "$(q '[.prs[].number] | join(",")')" "281,282"
expect "range: openers and mergers of the source PRs, counted by PR" "$(q '[(.authors[] | "\(.login):\(.prs)"), (.mergers[] | "m-\(.login):\(.prs)")] | join(",")')" "dana:2,m-pat:2"
expect "range: the promoter is read from the record" "$(q .promoter)" pat
expect "range: Clearance got the source PR's trailers, repo and head, not the gitops PR" \
  "$(jq -sr '[.[] | "\(.repo)#\(.pr)@\(.head[0:2]) \(.coauthors | length)"] | join(";")' "$FIX/provenance-requests.jsonl")" "o/shop#281@10 1;o/shop#282@30 1"

# 2. A person's PR (no release record names it): not applicable, the PR author stays the proposer.
scenario person; jq '.items[0].data.prUrl = "https://github.com/o/gitops-shop/pull/99"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(resolve)"; expect "person's PR: not applicable" "$(q .applicable)" false

# 3. Several openers: the proposer is the opener of the NEWEST merged source PR (merge time, then number), not a count of
#    commits; every opener and merger is kept. A commit's author login is never identity: GitHub derives it from the
#    commit's author email, which whoever pushes sets, so here every commit claims another person and none of it counts.
scenario many
gh "o/shop/compare/$P...$R?per_page=100&page=1" "$(jq -nc --argjson a "$(commit "$C1" 1 victim)" --argjson b "$(commit "$M1" 2 victim)" --argjson c "$(commit "$C2" 1 victim)" --argjson e "$(commit "$M2" 2 victim)" '{commits: [$a, $b, $c, $e]}')"
prpages 281 zed "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" mia 2026-11-13T01:00:00Z
prpages 282 amy "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" lee 2026-11-13T02:00:00Z
out="$(resolve)"
expect "several openers: the newest merged PR's opener proposes; the commits' author login (victim) is ignored" "$(q '[.proposer, (.authors | map(.login) | join("+")), (.mergers | map(.login) | join("+"))] | join(",")')" "user:amy,amy+zed,lee+mia"
prpages 281 zed "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" mia 2026-11-13T03:00:00Z
out="$(resolve)"; expect "newest by merge time, not by PR number or commit order" "$(q .proposer)" "user:zed"
expect "no commit author is ever in the result" "$(grep -c victim <<<"$out")" 0

# 4. Mixed provenance: the weakest wins; confirmed only when every source PR is.
scenario mix; echo '{"281":{"identity":{"status":"confirmed"},"record":{"agent":"scribe"}},"282":{"identity":{"status":"not-agent"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "confirmed + not-agent: not-agent, a person (an agent is never claimed)" "$(q '[.provenance, .proposerKind, .proposer] | join(",")')" "not-agent,person,user:dana"
echo '{"281":{"identity":{"status":"confirmed"},"record":{"agent":"scribe"}},"282":{"identity":{"status":"unattested"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "confirmed + unattested: unattested" "$(q .provenance)" unattested
echo '{"281":{"identity":{"status":"not-agent"}},"282":{"identity":{"status":"unattested"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "not-agent + unattested: unattested" "$(q .provenance)" unattested
echo '{"281":{"identity":{"status":"confirmed"},"record":{"agent":"scribe"}},"282":{"identity":{"status":"confirmed"},"record":{"agent":"coder"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "all confirmed: an agent, every agent kept, the first by name proposes" "$(q '[.provenance, .proposerKind, .proposer, .agent, (.agents | join("+"))] | join(",")')" "confirmed,agent,autopilot-runs/coder,coder,coder+scribe"
echo '{"281":{"identity":{"status":"confirmed"},"record":{"agent":"scribe"}},"282":{"identity":{"status":"modified"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "a modified agent PR (AI003): unknown" "$(q '[.provenance, .proposerKind] | join(",")')" "unknown,unknown"
echo '{"281":{"identity":{"status":"unrecorded"}},"282":{"identity":{"status":"not-agent"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "forged trailers (AI001) in one PR: unknown" "$(q .provenance)" unknown
echo '{"282":{"identity":{"status":"not-agent"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "Clearance cannot answer for one PR: unknown, never claimed" "$(q .provenance)" unknown

# 5. A commit no merged PR contains (a direct push) cannot be vouched for: unknown, named in full. Merge commits count too.
scenario nopr; cpulls "$C2" '[]'; prpages 282 dana "$C2" "[{\"sha\":\"$(sha 777)\"}]" "" "$M2"
out="$(resolve)"; expect "a direct push: unknown, and names the commit" "$(q '[.provenance, .proposerKind, (.noPr | join(","))] | join("|")')" "unknown|unknown|$C2"
scenario nopr-merge; cpulls "$M1" '[]'; prpages 281 dana "$C1" "[{\"sha\":\"$C1\"}]" "" "$(sha 778)"
out="$(resolve)"; expect "a merge commit made outside any PR is no-PR too (not skipped)" "$(q '[.provenance, (.noPr | join(","))] | join("|")')" "unknown|$M1"
scenario nopr-all; for c in "$C1" "$M1" "$C2" "$M2"; do cpulls "$c" '[]'; done
out="$(resolve)"; expect "no commit maps to a merged PR: unresolved" "$(q '[.resolved, (.reason | test("no commit of the range"))] | join(",")')" "false,true"
scenario pulls-many; gh "o/shop/commits/$C1/pulls?per_page=100&page=1" "$(jq -nc '[range(100) | {number: (1000 + .), merged_at: "2026-11-13T00:00:00Z"}]')"
gh "o/shop/commits/$C1/pulls?per_page=100&page=2" "$(jq -nc '[range(100) | {number: (2000 + .), merged_at: "2026-11-13T00:00:00Z"}]')"
gh "o/shop/commits/$C1/pulls?per_page=100&page=3" "$(jq -nc '[range(100) | {number: (3000 + .), merged_at: "2026-11-13T00:00:00Z"}]')"
out="$(resolve)"; expect "a commit with 300+ associated PRs is not read in full: unresolved" "$(q '[.resolved, (.reason | test("300"))] | join(",")')" "false,true"
scenario principal; prpages 281 "dependabot[bot]" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T00:00:00Z
out="$(resolve)"; expect "a bot opener fits Clearance's principal pattern" "$(q '[.resolved, (.authors | map(.login) | join("+"))] | join(",")')" "true,dana+dependabot[bot]"
prpages 281 "j.doe-x" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T00:00:00Z
out="$(resolve)"; expect "a dotted login fits" "$(q .resolved)" true
prpages 281 "bad name" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T00:00:00Z
out="$(resolve)"; expect "a login Clearance cannot record: unresolved, never mangled" "$(q '[.resolved, (.reason | test("principal"))] | join(",")')" "false,true"

# 6. A pipeline App's PR no record names is not a person's: unresolved. A person's own PR is unchanged (case 2).
scenario app-norecord; jq '.items[0].data.prUrl = "https://github.com/o/gitops-shop/pull/99"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
PIPELINE_LOGINS="other[bot],cicd-pac[bot]" PRAUTHOR="Cicd-Pac[bot]" out="$(PIPELINE_LOGINS="other[bot],cicd-pac[bot]" PRAUTHOR="Cicd-Pac[bot]" resolve)"
expect "the pipeline App's PR with no record: unresolved (kind unknown), not a person's" "$(q '[.applicable, .resolved, (.reason | test("no release record names it"))] | join(",")')" "true,false,true"
out="$(PIPELINE_LOGINS="" resolve)"; expect "without the setting a no-record PR is a person's (unchanged)" "$(q .applicable)" false
out="$(PIPELINE_LOGINS="cicd-pac[bot]" PRAUTHOR=dana resolve)"; expect "a person's PR with the setting on: unchanged" "$(q .applicable)" false
scenario dup; jq '.items += [.items[0]]' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(resolve)"; expect "two records for one PR: none trusted" "$(q '[.resolved, (.reason | test("2 release records"))] | join(",")')" "false,true"
scenario dotted-repo; jq '.items[0].data.gitUrl = "https://github.com/o/shop.js" | .items[1].data.gitUrl = "https://github.com/o/shop.js"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
for f in "$FIX"/gh/o_shop_*; do cp "$f" "$FIX/gh/o_shop_js_${f##*/o_shop_}"; done
out="$(resolve)"; expect "a repository name with a dot is valid" "$(q '[.resolved, .sourceRepo] | join(",")')" "true,o/shop.js"

# 7. Unresolvable: no record for the image the environment runs now (swept), a first release, a range over the cap.
scenario swept; jq '.items |= [.[0]]' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(resolve)"; expect "previous record swept: unresolved, the reason says so" "$(q '[.resolved, (.reason | test("swept"))] | join(",")')" "false,true"
expect "unresolved still names the promoter and the release" "$(q '[.promoter, .releaseId] | join(",")')" "pat,rel2:c1/prod"
scenario first; rm "$FIX/base/c1/prod/release.yaml"
printf 'release:\n  image:\n    repository: ghcr.io/x/shop\n    tag: 1.0.2-%s\nreleaseTracking:\n  releaseId: rel2:c1/prod\n  cluster: c1\n  appNamespace: app-shop-cicd\n' "${R:0:7}" > "$FIX/head/c1/prod/release.yaml"
gh "o/shop/commits/$R" "$(commit "$R" 1 dana)"; cpulls "$R" '[282]'; prpages 282 dana "$R" "[{\"sha\":\"$R\"}]" "" "$R"
out="$(resolve)"; expect "first release to an environment: the released commit alone, resolved through its PR" "$(q '[.resolved, .mode, .commits, .proposer] | join(",")')" "true,first,1,user:dana"
scenario big; gh "o/shop/compare/$P...$R?per_page=1" '{"status":"ahead","total_commits":300}'
out="$(resolve)"; expect "more than 250 commits: unresolved" "$(q '[.resolved, (.reason | test("250"))] | join(",")')" "false,true"
scenario nokube; export RECORDS_FAIL="$FIX/records.json"
if ( source "$d/lib.sh"; PATH="$d/bin:$PATH" NAMESPACE=n APP_NAME=shop release_source_resolve o gitops-shop 7 '[]' "$FIX/head" "$FIX/base" ) >/dev/null 2>&1; then
  echo "FAIL unreadable records must fail the check"; fail=1; else echo "ok   unreadable release records fail the check (no guess)"; fi
unset RECORDS_FAIL

# 8. A rollback restores one earlier commit: that commit alone, whatever the compare would say.
scenario rollback; jq '.items[0].data.kind = "rollback" | .items[0].data.rollbackOf = "rel0"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
gh "o/shop/commits/$R" "$(commit "$R" 1 dana)"
cpulls "$R" '[282]'
prpages 282 dana "$R" "[{\"sha\":\"$R\"}]" "" "$R"
out="$(resolve)"; expect "rollback: the restored commit alone" "$(q '[.mode, .commits, .proposer] | join(",")')" "rollback,1,user:dana"
# ... and a release whose commit is older than what runs (a person re-promoting an old image): compare says behind.
scenario behind; gh "o/shop/compare/$P...$R?per_page=1" '{"status":"behind","total_commits":0}'
gh "o/shop/commits/$R" "$(commit "$R" 1 dana)"; cpulls "$R" '[282]'
prpages 282 dana "$R" "[{\"sha\":\"$R\"}]" "" "$R"
out="$(resolve)"; expect "an older image than the environment runs: handled as a rollback" "$(q .mode)" rollback
scenario same; gh "o/shop/compare/$P...$R?per_page=1" '{"status":"identical","total_commits":0}'
gh "o/shop/commits/$R" "$(commit "$R" 1 dana)"; cpulls "$R" '[282]'
prpages 282 dana "$R" "[{\"sha\":\"$R\"}]" "" "$R"
out="$(resolve)"; expect "the same commit again: that commit alone" "$(q '[.mode, .commits] | join(",")')" "single,1"

# 9. Forgery. The PR's text is never read, so a body or title naming a source does nothing; what could change the result
#    is the PR's content, and the PR is only the pipeline's if its content is what the record says it wrote.
scenario forged-body
# (the PR body / title / commit message a forger controls are not inputs: the function takes the files and the manifests)
out="$(resolve)"; expect "text in the PR cannot change the source: it is the record's" "$(q .releasedCommit)" "$R"
scenario forged-files; echo '[{"filename":"c1/prod/release.yaml"},{"filename":"c1/prod/secrets.yaml"}]' > "$FIX/files.json"
out="$(resolve)"; expect "an extra file: not the pipeline's PR, unresolved" "$(q '[.resolved, (.reason | test("changes c1/prod/release.yaml, c1/prod/secrets.yaml"))] | join(",")')" "false,true"
scenario forged-other-env; echo '[{"filename":"c1/dev/release.yaml"}]' > "$FIX/files.json"
out="$(resolve)"; expect "a manifest of another environment than the record's: unresolved" "$(q .resolved)" false
scenario forged-commits; gh "o/gitops-shop/pulls/7" '{"commits": 2}'
out="$(resolve)"; expect "a second commit pushed to the branch: unresolved" "$(q '[.resolved, (.reason | test("2 commits"))] | join(",")')" "false,true"
scenario forged-field; printf 'replicaCount: 9\nrelease:\n  image:\n    repository: ghcr.io/x/shop\n    tag: 1.0.2-%s\nreleaseTracking:\n  releaseId: rel2:c1/prod\n  cluster: c1\n  appNamespace: app-shop-cicd\n' "${R:0:7}" > "$FIX/head/c1/prod/release.yaml"
out="$(resolve)"; expect "a changed field beside the image: unresolved" "$(q '[.resolved, (.reason | test("more than the image"))] | join(",")')" "false,true"
scenario forged-tag; sed -i.bak "s/1.0.2-${R:0:7}/1.0.2-${P:0:7}/" "$FIX/head/c1/prod/release.yaml"
out="$(resolve)"; expect "an image that is not the record's (another version's source claimed): unresolved" "$(q '[.resolved, (.reason | test("not the record"))] | join(",")')" "false,true"
RT='releaseTracking:\n  releaseId: rel2:c1/prod\n  cluster: c1\n  appNamespace: app-shop-cicd\n'
head_with() { # <extra image lines> : the head manifest with the right tag and repository
  printf "replicaCount: 2\nrelease:\n  image:\n    repository: ghcr.io/x/shop\n    tag: 1.0.2-%s\n$1${RT}" "${R:0:7}" > "$FIX/head/c1/prod/release.yaml"; }
scenario forged-digest; jq '.items[0].data.imageDigest = "sha256:aaaa"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
head_with '    digest: sha256:bbbb\n'
out="$(resolve)"; expect "a digest that is not the recorded one (a PR pre-opened on the release branch): unresolved" "$(q '[.resolved, (.reason | test("digest"))] | join(",")')" "false,true"
scenario nodigest-record; head_with '    digest: sha256:bbbb\n'
out="$(resolve)"; expect "the record has no digest and the PR carries one: unresolved (the safe choice)" "$(q '[.resolved, (.reason | test("digest"))] | join(",")')" "false,true"
scenario good-digest; jq '.items[0].data.imageDigest = "sha256:aaaa" | .items[0].data.imageSource = "ghcr.io/o/shop"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
head_with '    digest: sha256:aaaa\n    source: ghcr.io/o/shop\n'
out="$(resolve)"; expect "the recorded digest, source and releaseTracking are the pipeline's own: resolved" "$(q .resolved)" true
scenario forged-repo; printf 'replicaCount: 2\nrelease:\n  image:\n    repository: ghcr.io/evil/shop\n    tag: 1.0.2-%s\n%s' "${R:0:7}" "$(printf "$RT")" > "$FIX/head/c1/prod/release.yaml"
out="$(resolve)"; expect "an image repository that is not the record's: unresolved" "$(q '[.resolved, (.reason | test("repository"))] | join(",")')" "false,true"
scenario forged-rollout-image; printf 'rollout:\n  image:\n    tag: evil\n' >> "$FIX/head/c1/prod/release.yaml"; printf 'rollout:\n  image:\n    tag: evil\n' > /dev/null
out="$(resolve)"; expect "a rollout.image added: unresolved" "$(q .resolved)" false
scenario forged-tracking; sed -i.bak 's/app-shop-cicd/app-evil-cicd/' "$FIX/head/c1/prod/release.yaml"
out="$(resolve)"; expect "a releaseTracking field that is not the record's: unresolved" "$(q '[.resolved, (.reason | test("releaseTracking"))] | join(",")')" "false,true"
scenario old-record; jq 'del(.items[0].data.imageRepository)' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(resolve)"; expect "a record that predates the repository field: unresolved, says so" "$(q '[.resolved, (.reason | test("predates"))] | join(",")')" "false,true"
scenario forged-sha; jq --arg x "$(sha 999)" '.items[0].data.gitRevision = $x' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(resolve)"; expect "an image tag that does not end in the record's commit: unresolved" "$(q '[.resolved, (.reason | test("does not end in"))] | join(",")')" "false,true"

# 10. The render step's mapping into the change request (the real step, a stand-in helm, the parity fixture).
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/clearance-parity"
cat > "$d/bin/helm" <<'EOF'
#!/usr/bin/env bash
cat "${FIXTURE}/head.yaml"
EOF
chmod +x "$d/bin/helm"
yq '.spec.steps[1].script' "$d/task.yaml" | sed -e "s#/opt/airframe/charts/airframe-application#/chart#g" > "$d/render.sh"
change_for() { # <name> <source.json content or ""> [provenance verdict json]: runs the render step, prints change.json
  local w="$d/render-$1"; mkdir -p "$w/repo" "$w/base" "$w/observability" "$w/policies"
  cp -R "$fx/hangar-prod" "$w/repo/"; cp -R "$fx/hangar-prod" "$w/base/"
  : > "$w/policies/unreadable"; : > "$w/observability/hangar-prod.yaml"
  jq -n --argjson prov "${3:-null}" '{owner: "o", repo: "gitops-shop", pr: 7, head: ("a" * 40), base: ("b" * 40), author: "cicd-pac[bot]",
    files: [{filename: "hangar-prod/prod/release.yaml", status: "modified", additions: 1, deletions: 1}], looksAgent: false,
    provenance: $prov, checkRuns: [], policyCommit: ""}' > "$w/meta.json"
  [[ -n "$2" ]] && printf '%s' "$2" > "$w/source.json"
  echo "hangar-prod|prod|default|true" > "$w/plan.txt"
  sed "s#/work#$w#g" "$d/render.sh" > "$w/step.sh"
  ( cd "$w" && FIXTURE="$fx" PATH="$d/bin:$PATH" APP_NAME=shop NAMESPACE=app-shop-cicd bash "$w/step.sh" ) > "$w/log" 2>&1 || { echo "-- render log:"; cat "$w/log"; }
  jq -c '[.proposer, .proposerKind, (.agent // "none"), .checks.provenance, (.checks.sourceAuthors // "-"), (.checks.sourceMergers // "-"), ((.checks.sourceUnresolved // "-") | .[0:20])] | join("|")' "$w/eval/hangar-prod__prod/change.json" | tr -d '"'
}
good="$(scenario map-good; resolve)"
expect "register: a person's source (several authors in the record)" \
  "$(change_for good "$(jq -c '.authors = [{login: "amy", prs: 1}, {login: "zed", prs: 1}] | .mergers = [{login: "lee", prs: 2}] | .proposer = "user:amy"' <<<"$good")")" "user:amy|person|none|unattested|amy,zed|lee|-"
expect "register: a confirmed source is an agent" \
  "$(change_for agent "$(jq -c '.provenance = "confirmed" | .proposerKind = "agent" | .agent = "scribe" | .proposer = "autopilot-runs/scribe"' <<<"$good")")" "autopilot-runs/scribe|agent|scribe|confirmed|dana|pat|-"
expect "register: unresolved source: the PR author, kind unknown (never person), the reason in the change" \
  "$(change_for unres '{"applicable":true,"resolved":false,"reason":"no release record for the image the environment runs now"}')" "user:cicd-pac[bot]|unknown|none|unknown|-|-|no release record fo"
expect "register: not a release PR: unchanged (the PR author, per the agent gates' verdict)" \
  "$(change_for plain '{"applicable":false}' '{"identity":{"status":"not-agent"}}')" "user:cicd-pac[bot]|person|none|not-agent|-|-|-"
expect "register: no source.json at all (an older fetch): unchanged" \
  "$(change_for none '' '{"identity":{"status":"not-agent"}}')" "user:cicd-pac[bot]|person|none|not-agent|-|-|-"

[[ "${fail}" == 0 ]] && echo "all passed" || { echo "FAILED"; exit 1; }
