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
url=""; data=""; hdrf=""
while (( $# )); do
  case "$1" in
    --data) data="$2"; [[ "${data}" == "@-" ]] && data="$(cat)"; shift ;;
    -D) hdrf="$2"; shift ;;
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
# -D: the response headers (a Link header for paging) from <fixture>.headers, when there is one
[[ -n "${hdrf}" ]] && { if [[ -f "${f}.headers" ]]; then cat "${f}.headers" > "${hdrf}"; else printf 'HTTP/2 200\r\n' > "${hdrf}"; fi; }
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
prpages() { # <pr> <opener> <head sha> <commit json array> <message for coauthors or ""> [merge commit sha] [merger] [merged_at] [json merged over the PR]
  local x="${9:-}"; [[ -n "$x" ]] || x='{}'
  gh "o/shop/pulls/$1" "$(jq -nc --arg h "$3" --arg a "$2" --arg m "${6:-}" --arg g "${7:-pat}" --arg t "${8:-2026-11-13T00:0$1:00Z}" --argjson x "$x" \
    '{head: {sha: $h}, user: {login: $a, type: "User"}, merged_by: {login: $g}, merged_at: $t, merge_commit_sha: $m,
      base: {ref: "main", repo: {full_name: "o/shop"}}, additions: 10, deletions: 0} * $x')"
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
  gh "o/shop" '{"default_branch": "main"}'
  gh "o/shop/compare/$P...$R?per_page=1" '{"status":"ahead","total_commits":4}'
  gh "o/shop/compare/$P...$R?per_page=100&page=1" "$(jq -nc --argjson a "$(commit "$C1" 1 dana)" --argjson b "$(commit "$M1" 2 dana)" --argjson c "$(commit "$C2" 1 dana)" --argjson e "$(commit "$M2" 2 dana)" '{commits: [$a, $b, $c, $e]}')"
  cpulls "$C1" '[281]'; cpulls "$M1" '[281]'; cpulls "$C2" '[282]'; cpulls "$M2" '[282]'
  prpages 281 dana "$C1" "[{\"sha\":\"$C1\"}]" "Co-Authored-By: Claude <noreply@anthropic.com>" "$M1"
  prpages 282 dana "$C2" "[{\"sha\":\"$C2\"}]" "Co-Authored-By: Claude <noreply@anthropic.com>" "$M2"
  echo '{"281":{"identity":{"status":"unattested"}},"282":{"identity":{"status":"unattested"}}}' > "$FIX/verdicts.json"
}
resolve() { # (PRAUTHORTYPE: the PR author's account type) -> prints the function's JSON; the files list is $FIX/files.json
  ( source "$d/lib.sh"; github_app_installation_token() { echo tok; }
    PATH="$d/bin:$PATH" NAMESPACE=app-shop-cicd APP_NAME=shop CLEARANCE_URL=http://clearance PIPELINE_LOGINS="${PIPELINE_LOGINS:-}" \
    release_source_resolve o gitops-shop 7 "$(cat "$FIX/files.json")" "$FIX/head" "$FIX/base" "${PRAUTHOR:-cicd-pac[bot]}" "${PRAUTHORTYPE:-Bot}" "${RCFG:-}" ) 2> "$FIX/stderr"
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
for f in "$FIX"/gh/o_shop_*; do cp "$f" "$FIX/gh/o_shop_js_${f##*/o_shop_}"; done; cp "$FIX/gh/o_shop" "$FIX/gh/o_shop_js"; sed -i.bak 's#"full_name":"o/shop"#"full_name":"o/shop.js"#' "$FIX"/gh/o_shop_js_pulls_28[12]; rm -f "$FIX"/gh/*.bak
out="$(resolve)"; expect "a repository name with a dot is valid" "$(q '[.resolved, (.sourceRepo // .reason)] | join(",")')" "true,o/shop.js"

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
scenario trunc; gh "o/shop/compare/$P...$R?per_page=100&page=1" "$(jq -nc --argjson a "$(commit "$C1" 1 dana)" '{commits: [$a]}')"
out="$(resolve)"; expect "a compare listing fewer commits than the range's total_commits (4): unresolved" "$(q '[.resolved, (.reason | test("not read in full"))] | join(",")')" "false,true"
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

# 11. Covering PRs must really land the commit on the released branch of the source repo (the source repo's default branch:
#     the pipeline records none) and be a PR into that repo. A commit pushed straight to main that a PR from a side branch
#     later "contains" must stay no-PR.
scenario side-branch; prpages 282 dana "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" pat 2026-11-13T00:02:00Z '{"base": {"ref": "side"}}'
out="$(resolve)"; expect "a PR merged into a side branch does not cover the commit (X pushed to main, PR containing X merged to a side branch)" "$(q '[.provenance, (.noPr | join(","))] | join("|")')" "unknown|$C2,$M2"
scenario fork-base; prpages 282 dana "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" pat 2026-11-13T00:02:00Z '{"base": {"repo": {"full_name": "evil/shop"}}}'
out="$(resolve)"; expect "a PR into another base repository does not cover it" "$(q '[.provenance, (.noPr | join(","))] | join("|")')" "unknown|$C2,$M2"
scenario unmerged; gh "o/shop/commits/$C2/pulls?per_page=100&page=1" '[{"number":282,"merged_at":null}]'; gh "o/shop/commits/$M2/pulls?per_page=100&page=1" '[{"number":282,"merged_at":null}]'
out="$(resolve)"; expect "a closed-unmerged or open PR (merged_at null) does not cover it" "$(q '[.provenance, (.noPr | join(","))] | join("|")')" "unknown|$C2,$M2"
scenario trunk; gh "o/shop" '{"default_branch": "trunk"}'
out="$(resolve)"; expect "the released branch is the source repo's default branch, not the PR's claim: PRs into main do not cover when it is trunk" "$(q .resolved)" false
scenario squash; gh "o/shop/compare/$P...$R?per_page=1" '{"status":"ahead","total_commits":1}'
gh "o/shop/compare/$P...$R?per_page=100&page=1" "$(jq -nc --argjson a "$(commit "$R" 1 squasher)" '{commits: [$a]}')"
cpulls "$R" '[281]'; prpages 281 dana "$C1" "[{\"sha\":\"$C1\"},{\"sha\":\"$C2\"}]" "" "$R"
out="$(resolve)"; expect "a squash merge: the one commit on main is the PR's merge_commit_sha" "$(q '[.provenance, .proposer, (.noPr | length)] | join("|")')" "unattested|user:dana|0"
scenario rebase; gh "o/shop/compare/$P...$R?per_page=1" '{"status":"ahead","total_commits":2}'
gh "o/shop/compare/$P...$R?per_page=100&page=1" "$(jq -nc --argjson a "$(commit "$(sha 50)" 1 x)" --argjson b "$(commit "$R" 1 x)" '{commits: [$a, $b]}')"
cpulls "$(sha 50)" '[281]'; cpulls "$R" '[281]'; prpages 281 dana "$C1" "[{\"sha\":\"$C1\"},{\"sha\":\"$C2\"}]" "" "$R"
out="$(resolve)"; expect "a rebase merge: only the tip (merge_commit_sha) is provable; the rebased commits have new shas, so fail closed" "$(q '[.provenance, (.noPr | join(","))] | join("|")')" "unknown|$(sha 50)"

# 12. The single proposer: bots never, then the human opener of the PR with the most changed lines (ties: newest).
scenario pick-bot; prpages 281 "dependabot[bot]" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T05:00:00Z '{"additions": 900}'
out="$(resolve)"; expect "a bot's huge PR does not steer: the human opener proposes" "$(q .proposer)" "user:dana"
scenario pick-type; prpages 281 "ci-account" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T05:00:00Z '{"additions": 900, "user": {"type": "Bot"}}'
out="$(resolve)"; expect "an account whose API type is Bot is skipped though its login lacks [bot]" "$(q .proposer)" "user:dana"
scenario pick-setting; prpages 281 "svc-release" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T05:00:00Z '{"additions": 900}'
out="$(PIPELINE_LOGINS=svc-release resolve)"; expect "a login in clearanceGate.pipelineLogins is skipped" "$(q .proposer)" "user:dana"
scenario pick-lines; prpages 281 zed "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" pat 2026-11-13T06:00:00Z '{"additions": 40, "deletions": 5}'
prpages 282 amy "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" pat 2026-11-13T07:00:00Z '{"additions": 20}'
out="$(resolve)"; expect "the human opener of the PR with the most changed lines (not the newest) proposes" "$(q .proposer)" "user:zed"
prpages 282 amy "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" pat 2026-11-13T07:00:00Z '{"additions": 45}'
out="$(resolve)"; expect "equal lines: the newest" "$(q .proposer)" "user:amy"
scenario pick-merger; prpages 281 "a[bot]" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" mia 2026-11-13T01:00:00Z; prpages 282 "b[bot]" "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" lee 2026-11-13T02:00:00Z
out="$(resolve)"; expect "every opener a bot: the human merger of the newest PR proposes" "$(q '[.proposer, .proposerKind] | join(",")')" "user:lee,person"
prpages 282 "b[bot]" "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" "web[bot]" 2026-11-13T02:00:00Z
out="$(resolve)"; expect "the newest PR's merger a bot too: the next newest human merger" "$(q .proposer)" "user:mia"
scenario pick-none; prpages 281 "a[bot]" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" "m[bot]" 2026-11-13T01:00:00Z; prpages 282 "b[bot]" "$C2" "[{\"sha\":\"$C2\"}]" "" "$M2" "n[bot]" 2026-11-13T02:00:00Z
out="$(resolve)"; expect "no human at all: kind unknown" "$(q '[.proposerKind, .provenance] | join(",")')" "unknown,unattested"

# 13. A curl failure is not masked by the jq after it.
scenario curl-fail-pr; rm "$FIX/gh/o_gitops_shop_pulls_7"
out="$(resolve)"; expect "the release PR cannot be read: unresolved, says so" "$(q '[.resolved, .reason] | join(",")')" "false,could not read the release PR"
scenario curl-fail-commit; jq '.items[0].data.kind = "rollback"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(resolve)"; expect "the single commit cannot be read: unresolved, says so" "$(q '[.resolved, (.reason | test("could not read o/shop@"))] | join(",")')" "false,true"

# 13b. An unresolved early return (two records) omits the promoter: the report must still say "not recorded", not null.
n="$(helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/clearance-gate.yaml \
  | yq '.spec.steps[].script' | grep -c '(.promoter // "") != ""')"
expect "the report treats a missing promoter as not recorded" "$n" 1

# 13c. _rs_pages: pages go through jq by pipe (a page over 128 KiB), stop on the RAW length, and fail closed.
pages() { # <page-body generator: a function name taking the page number> <filter> <max pages>
  ( source "$d/lib.sh"; _rs_api() { "$PG" "$(sed -E 's/.*page=([0-9]+).*/\1/' <<<"$2")"; }; PG="$1"; _rs_pages tok "https://x/y" "$2" "$3"; echo "rc=$?" ) 2>&1
}
big_page() { jq -nc --arg pad "$(head -c 1600 /dev/zero | tr '\0' a)" '[range(100) | {name: ($pad + (. | tostring))}]'; }
pg_big() { big_page; }
out="$(pages pg_big '[.[] | .name]' 1 | tail -c 200)"
expect "a full page of 160 KB: read through jq without argv; the list may continue, so rc 3" "${out##*rc=}" 3
pg_small_big() { if [[ "$1" == 1 ]]; then big_page; else echo '[]'; fi; }
out="$(pages pg_small_big '[.[] | .name]' 3)"
expect "a page over 128 KiB is joined (all 100 names kept, rc 0)" "$(jq -r 'length' <<<"${out%rc=*}"),${out##*rc=}" "100,0"
pg_filtered() { if [[ "$1" == 1 ]]; then jq -nc '[range(100) | {merged_at: (if . < 40 then "t" else null end), number: .}]'; else jq -nc '[{merged_at: "t", number: 500}]'; fi; }
out="$(pages pg_filtered '[.[] | select(.merged_at != null) | .number]' 3)"
expect "a full raw page filtered to 40 does not stop: page 2 is read" "$(jq -r 'length,.[-1]' <<<"${out%rc=*}" | paste -sd, -),${out##*rc=}" "41,500,0"
# The compare endpoint's page is an OBJECT: 150 commits across two pages are all read (the raw length is .commits', not the key count).
pg_cmp() { if [[ "$1" == 1 ]]; then jq -nc '{status: "ahead", total_commits: 150, commits: [range(100) | {sha: ("a" + (. | tostring))}]}'; else jq -nc '{status: "ahead", total_commits: 150, commits: [range(50) | {sha: ("b" + (. | tostring))}]}'; fi; }
pages_cmp() { ( source "$d/lib.sh"; _rs_api() { pg_cmp "$(sed -E 's/.*page=([0-9]+).*/\1/' <<<"$2")"; }; _rs_pages tok "https://x/y" '.commits' 3 '.commits'; echo "rc=$?" ) 2>&1; }
out="$(pages_cmp)"
expect "a compare range of 150 commits over two pages: all 150 read" "$(jq -r 'length' <<<"${out%rc=*}"),${out##*rc=}" "150,0"
pg_bad() { echo 'not json'; }
out="$(pages pg_bad '.' 3)"
expect "an unparseable page fails closed" "${out##*rc=}" 1

# 14. A Bot account authored the PR, the pipelineLogins setting is empty and no record names it: still a person's PR, with a warning.
scenario warn; jq '.items[0].data.prUrl = "https://github.com/o/gitops-shop/pull/99"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
out="$(PIPELINE_LOGINS="" PRAUTHORTYPE=Bot resolve)"; expect "empty pipelineLogins and a Bot author: a warning is printed" "$(q '[.applicable, (.warning | test("pipelineLogins"))] | join(",")')" "false,true"
out="$(PIPELINE_LOGINS="" PRAUTHORTYPE=User resolve)"; expect "no warning for a person" "$(q '.warning // "none"')" none

# 15. Platform release-file commits (docs/admin/clearance-gate.md, "Platform release-file commits"): deploy-manifests' commit
#     straight to main is dropped from noPr only when ALL four hold: GitHub verified it, GitHub's activity log shows the
#     pipeline's App pushing it to the released branch (from its first parent), its full diff vs its first parent is only
#     release files, and none of them is the released environment's (prod here). C2 is the direct push of each case; P1 its
#     first parent. The commit is shaped like GitHub's real answer for an App's signed API commit (flight-api ce1f7f7,
#     2026-11-22): author the App's bot, committer web-flow ("GitHub"), verified.
P1="$(sha 900)"
unset PIPELINE_LOGINS PRAUTHOR PRAUTHORTYPE   # case 6 leaves them set in this shell (a line of assignments only)
CFG='{"environments":[{"name":"dev","tier":"ground"},{"name":"perf","tier":"ground"},{"name":"prod","tier":"flight","production":true}],"releaseFile":"glidepath/envs/{env}.release.yaml"}'
act_url() { # <actor login> [<cursor>]: the activity query the lib makes for that login
  printf 'o/shop/activity?ref=refs%%2Fheads%%2Fmain&actor=%s&activity_type=push&per_page=100%s' "$(jq -rn --arg v "$1" '$v | @uri')" "${2:+&after=$2}"
}
act() { # <json array of activity entries> [<cursor>]: what GitHub lists, for every login the cases query (the server's filters are not relied on)
  local l; for l in "cicd-pac[bot]" "other[bot]"; do gh "$(act_url "$l" "${2:-}")" "$1"; done
}
push() { # <actor login> <actor type> <after> <before> [<ref>] [<activity type>]: one activity entry
  jq -nc --arg l "$1" --arg t "$2" --arg a "$3" --arg b "$4" --arg r "${5:-refs/heads/main}" --arg k "${6:-push}" \
    '{before: $b, after: $a, ref: $r, activity_type: $k, actor: {login: $l, type: $t}}'
}
rfc() { # <name> <verified> <pusher login or ""> <json array of changed files> [<parents>] [<compare status>]: C2 pushed straight to main
  scenario "$1"; cpulls "$C2" '[]'; prpages 282 dana "$C2" "[{\"sha\":\"$(sha 777)\"}]" "Co-Authored-By: Claude <noreply@anthropic.com>" "$M2"
  gh "o/shop/commits/$C2" "$(jq -nc --arg s "$C2" --arg p "$P1" --argjson v "$2" --argjson n "${5:-1}" \
    '{sha: $s, parents: ([range($n)] | map({sha: $p})), author: {login: "cicd-pac[bot]", type: "Bot"}, committer: {login: "web-flow", type: "User"},
      commit: {author: {name: "cicd-pac[bot]", email: "312015858+cicd-pac[bot]@users.noreply.github.com"}, committer: {name: "GitHub", email: "noreply@github.com"},
               verification: {verified: $v, reason: (if $v then "valid" else "unsigned" end)}}}')"
  gh "o/shop/compare/$P1...$C2?per_page=1" "$(jq -nc --argjson f "$4" --arg st "${6:-ahead}" '{status: $st, files: [$f[] | if type == "string" then {filename: .} else . end]}')"
  if [[ -n "$3" ]]; then act "[$(push "$3" Bot "$(sha 950)" "$(sha 951)"),$(push "$3" Bot "$C2" "$P1")]"; else act '[]'; fi
}
rel() { RCFG="${RCFG-$CFG}" PIPELINE_LOGINS="${PIPELINE_LOGINS-}" resolve; }
platform_q='[.provenance, (.noPr | join(",")), ((.platformCommits // []) | join(","))] | join("|")'
rfc all-hold true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "all four hold (another environment's release file, verified, pushed by the App, web-flow committer): not counted, provenance from the PRs" "$(q "$platform_q")" "unattested||$C2"
expect "the skipped commit is reported apart" "$(q '.platformCommits | length')" 1
out="$(PIPELINE_LOGINS="CICD-PAC[bot]" PRAUTHOR=someone PRAUTHORTYPE=User rel)"; expect "the App's login from clearanceGate.pipelineLogins (any case): skipped" "$(q "$platform_q")" "unattested||$C2"
out="$(PIPELINE_LOGINS="other[bot]" rel)"; expect "pipelineLogins set to another login: the configured list wins over the PR author, kept" "$(q "$platform_q")" "unknown|$C2|"
out="$(PRAUTHOR=someone PRAUTHORTYPE=User rel)"; expect "no configured login and a release PR not authored by a Bot: the App is unknown, kept" "$(q "$platform_q")" "unknown|$C2|"
rfc rename true "cicd-pac[bot]" '[{"filename":"glidepath/envs/perf.release.yaml","previous_filename":"glidepath/envs/dev.release.yaml"}]'
out="$(rel)"; expect "a rename between two other environments' release files: skipped" "$(q "$platform_q")" "unattested||$C2"
# condition 1: verified
rfc unverified false "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "1 fails: an unverified commit, even one the App pushed: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc verified-string '"true"' "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "1 fails: verified must be the boolean true" "$(q "$platform_q")" "unknown|$C2|"
# condition 2: the App pushed it, per GitHub's activity log (W-48: never the commit's author or committer)
rfc forged true "" '["glidepath/envs/dev.release.yaml"]'
act "[$(push "cicd-pac[bot]" Bot "$(sha 950)" "$(sha 951)"),$(push dana User "$C2" "$P1")]"
out="$(rel)"; expect "2 fails: a person's web-flow-signed API commit naming the App's bot as author, pushed by the person: kept" "$(q "$platform_q")" "unknown|$C2|"
expect "why it was kept is in the step log" "$(grep -c "stays a commit no merged PR contains: GitHub's activity log does not show the pipeline's GitHub App pushing it" "$FIX/stderr")" 1
rfc other-bot true "renovate[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "2 fails: verified, but another bot pushed it: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc no-push true "" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "2 fails: no push of it in GitHub's activity log (a web-UI commit, or one that arrived inside another push): kept" "$(q "$platform_q")" "unknown|$C2|"
rfc user-type true "" '["glidepath/envs/dev.release.yaml"]'; act "[$(push "cicd-pac[bot]" User "$C2" "$P1")]"
out="$(rel)"; expect "2 fails: the actor has the App's login but GitHub does not type it Bot: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc other-ref true "" '["glidepath/envs/dev.release.yaml"]'; act "[$(push "cicd-pac[bot]" Bot "$C2" "$P1" refs/heads/side)]"
out="$(rel)"; expect "2 fails: the App pushed it to another branch: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc force-push true "" '["glidepath/envs/dev.release.yaml"]'; act "[$(push "cicd-pac[bot]" Bot "$C2" "$P1" refs/heads/main force_push)]"
out="$(rel)"; expect "2 fails: a force push is not deploy-manifests' push: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc other-before true "" '["glidepath/envs/dev.release.yaml"]'; act "[$(push "cicd-pac[bot]" Bot "$C2" "$(sha 951)")]"
out="$(rel)"; expect "2 fails: the App's push did not move the branch from the commit's first parent: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc no-activity true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'; rm "$FIX/gh/$(sed -E 's#[^A-Za-z0-9]#_#g' <<<"$(act_url "cicd-pac[bot]")")"
out="$(rel)"; expect "2 fails: the activity log cannot be read: kept" "$(q "$platform_q")" "unknown|$C2|"
expect "the step log says the log could not be read" "$(grep -c "GitHub's activity log (who pushed it) could not be read" "$FIX/stderr")" 1
rfc not-a-list true "" '["glidepath/envs/dev.release.yaml"]'; act '{"message": "x"}'
out="$(rel)"; expect "2 fails: the activity answer is not a list: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc repeated-tip true "" '["glidepath/envs/dev.release.yaml"]'; act "[$(push "cicd-pac[bot]" Bot "$C2" "$P1"),$(push "cicd-pac[bot]" Bot "$C2" "$(sha 951)")]"
out="$(rel)"; expect "the same tip pushed twice by the App (once from its parent, once from elsewhere): every before is kept, skipped" "$(q "$platform_q")" "unattested||$C2"
# paging: the App's push is on the second page, reached through the Link header
rfc page-2 true "" '["glidepath/envs/dev.release.yaml"]'; act "[$(push "cicd-pac[bot]" Bot "$(sha 950)" "$(sha 951)")]"
act "[$(push "cicd-pac[bot]" Bot "$C2" "$P1")]" CUR2
printf 'HTTP/2 200\r\nLink: <https://api.github.com/repos/%s>; rel="next"\r\n' "$(act_url "cicd-pac[bot]" CUR2)" > "$FIX/gh/$(sed -E 's#[^A-Za-z0-9]#_#g' <<<"$(act_url "cicd-pac[bot]")").headers"
out="$(rel)"; expect "the App's push on the second page: found through the next link, skipped" "$(q "$platform_q")" "unattested||$C2"
out="$(RS_ACTIVITY_PAGES=1 rel)"; expect "past the page cap: not found, kept" "$(q "$platform_q")" "unknown|$C2|"
printf 'HTTP/2 200\r\nLink: <https://evil.example/repos/%s>; rel="next"\r\n' "$(act_url "cicd-pac[bot]" CUR2)" > "$FIX/gh/$(sed -E 's#[^A-Za-z0-9]#_#g' <<<"$(act_url "cicd-pac[bot]")").headers"
out="$(rel)"; expect "a next link off api.github.com is not followed (the token never leaves): kept" "$(q "$platform_q")" "unknown|$C2|"
rfc page-1-stop true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'
printf 'HTTP/2 200\r\nLink: <https://api.github.com/repos/%s>; rel="next"\r\n' "$(act_url "cicd-pac[bot]" NOPE)" > "$FIX/gh/$(sed -E 's#[^A-Za-z0-9]#_#g' <<<"$(act_url "cicd-pac[bot]")").headers"
out="$(rel)"; expect "every commit found on the first page: the next page (no fixture: a read error) is never read, skipped" "$(q "$platform_q")" "unattested||$C2"
# condition 3: only release files
rfc extra-file true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml","src/a.ts"]'
out="$(rel)"; expect "3 fails: a source file beside the release file: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc env-file true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml","glidepath/envs/dev.yaml"]'
out="$(rel)"; expect "3 fails: the human env file too (the one-time move onto the split): kept" "$(q "$platform_q")" "unknown|$C2|"
rfc merge-hides true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml","src/hidden.ts"]' 2
out="$(rel)"; expect "3 fails: a merge commit whose diff vs its first parent brings in another file: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc merge-clean true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]' 2
out="$(rel)"; expect "a merge commit whose full diff vs its first parent is only another environment's release file: skipped" "$(q "$platform_q")" "unattested||$C2"
rfc unlisted true "cicd-pac[bot]" '["glidepath/envs/staging.release.yaml"]'
out="$(rel)"; expect "3 fails: the release-file path of an environment cicd.yaml does not list: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc other-path true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(RCFG='{"environments":[{"name":"dev"},{"name":"prod"}],"releaseFile":"deploy/{env}.yaml"}' rel)"; expect "3 fails: deploy.releaseFile resolves elsewhere (deploy-manifests refuses it): nothing is a release file, kept" "$(q "$platform_q")" "unknown|$C2|"
out="$(RCFG='' rel)"; expect "3 fails: no cicd.yaml answer: nothing is a release file, kept" "$(q "$platform_q")" "unknown|$C2|"
out="$(RCFG='{"environments":[{"name":"dev"},{"name":"dev"},{"name":"prod"}]}' rel)"; expect "an environment listed twice is ambiguous: kept" "$(q "$platform_q")" "unknown|$C2|"
out="$(RCFG='{"environments":[{"name":"dev"},{"name":"prod"}]}' rel)"; expect "no deploy.releaseFile: the default glidepath/envs/{env}.release.yaml, skipped" "$(q "$platform_q")" "unattested||$C2"
rfc many-files true "cicd-pac[bot]" "$(jq -nc '[range(300) | "glidepath/envs/dev.release.yaml"]')"
out="$(rel)"; expect "3 fails: a compare listing 300 files may be cut off: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc not-ahead true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]' 1 diverged
out="$(rel)"; expect "3 fails: the compare against the first parent is not 'ahead': kept" "$(q "$platform_q")" "unknown|$C2|"
rfc unreadable true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'; rm "$FIX/gh/$(sed -E 's#[^A-Za-z0-9]#_#g' <<<"o/shop/compare/$P1...$C2?per_page=1")"
out="$(rel)"; expect "the diff cannot be read: kept" "$(q "$platform_q")" "unknown|$C2|"
expect "why it was kept is in the step log" "$(grep -c "stays a commit no merged PR contains: could not read its diff" "$FIX/stderr")" 1
# condition 4: not the released environment's file (the record releases prod)
rfc released-env true "cicd-pac[bot]" '["glidepath/envs/prod.release.yaml"]'
out="$(rel)"; expect "4 fails: the released environment's own release file: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc both-envs true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml","glidepath/envs/prod.release.yaml"]'
out="$(rel)"; expect "4 fails: one commit touching another environment's and the released one's file: kept" "$(q "$platform_q")" "unknown|$C2|"
rfc release-dev true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'; jq '.items[].data.env = "dev"' "$FIX/records.json" > "$FIX/r" && mv "$FIX/r" "$FIX/records.json"
mkdir -p "$FIX/head/c1/dev" "$FIX/base/c1/dev"; mv "$FIX/head/c1/prod/release.yaml" "$FIX/head/c1/dev/"; mv "$FIX/base/c1/prod/release.yaml" "$FIX/base/c1/dev/"
echo '[{"filename":"c1/dev/release.yaml","status":"modified"}]' > "$FIX/files.json"
out="$(rel)"; expect "4 fails: the same dev commit, judged in a release of dev itself: kept" "$(q "$platform_q")" "unknown|$C2|"
( source "$d/lib.sh"
  expect "_rs_release_file_counts: another environment does not count" "$(_rs_release_file_counts dev prod)" false
  expect "_rs_release_file_counts: the released environment counts" "$(_rs_release_file_counts prod prod)" true
  expect "_rs_release_file_counts: an unknown released environment counts" "$(_rs_release_file_counts dev '')" true
  expect "_rs_release_paths: each environment at its own path, only the released one counts" \
    "$(_rs_release_paths "$CFG" prod | jq -c 'to_entries | map("\(.key)=\(.value.env):\(.value.counts)") | join(" ")' | tr -d '"')" \
    "glidepath/envs/dev.release.yaml=dev:false glidepath/envs/perf.release.yaml=perf:false glidepath/envs/prod.release.yaml=prod:true"
  [[ "${fail}" == 0 ]]   # the subshell's own flag: hand a failed assertion out
) || fail=1
# a release whose range is only platform commits still names no source PR: unresolved, as before
rfc only-platform true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'; for c in "$C1" "$M1" "$M2"; do cpulls "$c" '[]'; done
out="$(rel)"; expect "only platform commits and direct pushes: still unresolved (no source PR)" "$(q '[.resolved, (.reason | test("no commit of the range"))] | join(",")')" "false,true"

# 16. Why the proposer is unknown, for Clearance (provenanceReason, clearance#213): unvouched-commits (held for two people)
#     only when the one cause is commits no merged PR contains; unrecorded-agent when a source PR is unknown; else nothing.
why_q='[.proposerKind, (.provenanceReason // "none"), ((.unvouchedCommits // []) | join(","))] | join("|")'
scenario why-unvouched; cpulls "$C2" '[]'; prpages 282 dana "$C2" "[{\"sha\":\"$(sha 777)\"}]" "" "$M2"
out="$(resolve)"; expect "only unvouched commits (every source PR vouched, a human named): unvouched-commits with the remaining noPr" "$(q "$why_q")" "unknown|unvouched-commits|$C2"
echo '{"281":{"identity":{"status":"modified"}},"282":{"identity":{"status":"not-agent"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "unvouched commits AND a modified agent PR: unrecorded-agent (denied)" "$(q "$why_q")" "unknown|unrecorded-agent|"
echo '{"282":{"identity":{"status":"not-agent"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "unvouched commits AND a PR Clearance could not answer for: unrecorded-agent" "$(q "$why_q")" "unknown|unrecorded-agent|"
scenario why-agent; echo '{"281":{"identity":{"status":"unrecorded"}},"282":{"identity":{"status":"not-agent"}}}' > "$FIX/verdicts.json"
out="$(resolve)"; expect "no unvouched commit, a forged-trailer PR: unrecorded-agent" "$(q "$why_q")" "unknown|unrecorded-agent|"
scenario why-nohuman; cpulls "$C2" '[]'; prpages 282 "b[bot]" "$C2" "[{\"sha\":\"$(sha 777)\"}]" "" "$M2" "n[bot]"; prpages 281 "a[bot]" "$C1" "[{\"sha\":\"$C1\"}]" "" "$M1" "m[bot]"
out="$(resolve)"; expect "unvouched commits but no human at all (proposerRule none): nothing sent" "$(q "$why_q")" "unknown|none|"
scenario why-person; out="$(resolve)"; expect "a person's release (kind person): nothing sent" "$(q "$why_q")" "person|none|"
rfc why-platform true "cicd-pac[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "the only direct push is a platform release-file commit: not unknown, nothing sent" "$(q "$why_q")" "person|none|"
rfc why-mixed true "renovate[bot]" '["glidepath/envs/dev.release.yaml"]'
out="$(rel)"; expect "the unvouched list is what remains after the platform skip" "$(q "$why_q")" "unknown|unvouched-commits|$C2"

# 10. The mapping into the change request: the real publish -> render-manifests -> ingest chain (tests/lib/gate_chain.sh, real helm, the
# policy-validation fixture's stand-in chart), with the fetch step's /work laid out the way it leaves it, source.json included.
fx="$(pwd)/charts/glidepath-catalog/tests/fixtures/policy-validation"
helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/render-manifests.yaml > "$d/rtask.yaml" \
  || { echo "FAIL: render (render-manifests)"; exit 1; }
source charts/glidepath-catalog/tests/lib/gate_chain.sh
change_for() { # <name> <source.json content or ""> [provenance verdict json]: runs the chain, prints the change's mapped fields
  local w="$d/chain-$1" P="$d/chain-$1/pub"; mkdir -p "$P/repo/hangar-prod/prod" "$P/base/hangar-prod/prod" "$P/policies" "$P/observability"
  cp "$fx/gitops/hangar-prod/prod/release.yaml" "$P/repo/hangar-prod/prod/"; cp "$fx/gitops/hangar-prod/prod/release.yaml" "$P/base/hangar-prod/prod/"
  printf 'serviceType: ClusterIP\n' > "$P/repo/hangar-prod/prod/values.yaml"; printf 'replicas: 1\n' > "$P/base/hangar-prod/prod/values.yaml"
  : > "$P/policies/unreadable"; : > "$P/observability/hangar-prod.yaml"
  jq -n --argjson prov "${3:-null}" '{owner: "o", repo: "gitops-shop", pr: 7, head: ("a" * 40), base: ("b" * 40), author: "cicd-pac[bot]",
    files: [{filename: "hangar-prod/prod/values.yaml", status: "modified", additions: 1, deletions: 1}], looksAgent: false,
    provenance: $prov, checkRuns: [], policyCommit: ""}' > "$P/meta.json"
  [[ -n "$2" ]] && printf '%s' "$2" > "$P/source.json"
  echo "hangar-prod|prod|default|true" > "$P/plan.txt"
  gate_chain "$d/task.yaml" "$d/rtask.yaml" clearance-gate "$w" shop
  [[ "$(cat "$w/ingest.rc")" == 0 ]] || { echo "-- chain log:"; cat "$w/chain.log"; }
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

why_of() { jq -c '[(.provenanceReason // "none"), ((.unvouchedCommits // []) | join(","))] | join("|")' "$d/chain-$1/eval/hangar-prod__prod/change.json" | tr -d '"'; }
change_for why-u "$(jq -c --arg c "$C2" '.proposerKind = "unknown" | .provenance = "unknown" | .noPr = [$c] | .provenanceReason = "unvouched-commits" | .unvouchedCommits = [$c]' <<<"$good")" >/dev/null
expect "register: unvouched-commits rides in the change request with its commits" "$(why_of why-u)" "unvouched-commits|$C2"
change_for why-a "$(jq -c '.proposerKind = "unknown" | .provenance = "unknown" | .provenanceReason = "unrecorded-agent"' <<<"$good")" >/dev/null
expect "register: unrecorded-agent rides alone" "$(why_of why-a)" "unrecorded-agent|"
change_for why-p "$(jq -c '.provenanceReason = "unvouched-commits" | .unvouchedCommits = ["x"]' <<<"$good")" >/dev/null
expect "register: never with a proposer kind other than unknown" "$(why_of why-p)" "none|"
change_for why-r '{"applicable":true,"resolved":false,"reason":"r","provenanceReason":"unvouched-commits","unvouchedCommits":["a"]}' >/dev/null
expect "register: an unresolved source sends no reason" "$(why_of why-r)" "none|"

[[ "${fail}" == 0 ]] && echo "all passed" || { echo "FAILED"; exit 1; }
