# release-source.sh: the source of a pipeline release PR, for the clearance-gate Task (docs/admin/clearance-gate.md,
# "Who proposed a release"). glidepath-catalog.releaseSourceLib hands it to the fetch step as the RELEASE_SOURCE_LIB env var, which the step evals.
#
# A release PR in the gitops repo is written by the pipeline's GitHub App, so its author and commit say nothing about the
# change being released. The change was written in the app repo. This resolves who wrote it and what Clearance's
# provenance says about it, from what the pipeline recorded and can verify, never from text the PR carries:
#
#   release_source_resolve <owner> <repo> <pr> <files json> <head dir> <base dir> <PR author login> <PR author account type> [<release config json>]     prints one JSON object
#
# <release config json> is {environments, releaseFile}: the app cicd.yaml's deploy.environments and deploy.releaseFile, as the
# fetch step read them for its own protection decision. It only decides which commits are platform release-file commits
# (step 3b); empty or unreadable, none is.
#
# Reads: the release record (release-tracking-<chainId> in this namespace, written by open-release-pr; only the
# pipeline's ServiceAccount can write it), the PR's commit count, the manifest at base and head (checked out), the
# app repo's compare, commit, pull-request and repository activity APIs (the GitHub App's installation token), and Clearance's
# POST /v1/provenance. Needs github_app_installation_token (lib/github-app.sh), kubectl, curl, jq, yq; CLEARANCE_URL
# and the token at /var/run/secrets/clearance when Clearance is configured; APP_NAME and NAMESPACE.
#
# Output:
#   {applicable: false}                                  no release record names this PR: a person's PR, unchanged
#   {applicable: true, resolved: false, reason, ...}     a pipeline release whose source cannot be established
#   {applicable: true, resolved: true, mode, proposer, proposerKind, agent, provenance, authors, prs, ...}
# `reason` is what the gate prints. Returns non-zero (reason on stderr) only when it cannot even tell whether the PR is
# a release (the release records cannot be read): the check fails rather than guess.
#
# Trusted: the release record (pipeline-written: image, repository, digest, source repo and commit, kind, initiator), the
# app repo's merged pull requests as GitHub serves them (who opened and who merged each: authenticated account facts),
# Clearance's provenance verdicts. NOT trusted: the release PR's author, title, body, branch name or commit message,
# and NEVER a commit's author or committer: GitHub derives the commit author login from the commit's author email, which
# whoever pushes the commit chooses. Identity comes only from the merged PRs that contain the commits.
# The release PR is accepted as the pipeline's only if its content is exactly what the record says the pipeline wrote
# (one commit, one manifest file, only the image and release-tracking fields, each equal to the record's); anything
# else is unresolved. Safe choice when the record has no digest: the PR must carry none either.

_rs_api() { # <token> <url> [<extra curl args>...]
  curl --fail --silent --show-error --retry 3 --retry-connrefused --max-time 20 \
    -H "Authorization: Bearer $1" -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" "${@:3}" "$2"
}
# A JSON value as a file jq reads (--slurpfile, one element): a value goes to jq or curl through a pipe, never in argv, which
# Linux caps at 128 KiB per argument.
_rs_json() { printf '%s' "$1"; }
_rs_pages() { # <token> <url without paging> <jq filter turning a page into an array> <max pages> [<jq selector of the page's raw collection, default .>]: the arrays joined
  # Stops on the RAW page's length (a filter may shrink a full page). Fails (1) on any read or parse error, and (3) when the last
  # page allowed was still full: the list may continue, so it is not trusted.
  # The compare endpoint answers an OBJECT whose .commits is the collection: the raw length is that collection's, not the object's
  # key count (the 5th argument selects it).
  local page=1 out='[]' body part sep='?' raw sel="${5:-.}"
  [[ "$2" == *\?* ]] && sep='&'
  while (( page <= $4 )); do
    body="$(_rs_api "$1" "$2${sep}per_page=100&page=${page}")" || return 1
    raw="$(jq "(${sel}) | if type == \"array\" then length else error(\"not a list\") end" <<<"${body}")" || return 1
    part="$(jq -c "$3" <<<"${body}")" || return 1
    out="$(jq -c --slurpfile more <(_rs_json "${part}") '. + $more[0]' <<<"${out}")" || return 1
    (( raw < 100 )) && { printf '%s' "${out}"; return 0; }
    page=$((page + 1))
  done
  return 3
}

# _rs_release_paths <release config json> <released environment>: {"<path>": {env, counts}} for every environment cicd.yaml
# lists, at its resolved release-file path: deploy.releaseFile with {env} replaced by the environment's name (default
# glidepath/envs/{env}.release.yaml), the same substitution deploy-manifests makes. Only the path deploy-manifests can write
# (glidepath/envs/<env>.release.yaml, <env> a plain name) is listed, so a path names exactly one environment. counts is
# true when a commit changing that file must stay counted for this release (_rs_release_file_counts); an environment listed
# more than once is ambiguous and always counts.
_rs_release_paths() {
  local cfg="$1" m out='{}' p e dup c
  [[ -n "${cfg}" ]] || cfg='{}'
  cfg="$(jq -c 'if type == "object" then . else {} end' <<<"${cfg}" 2>/dev/null)" || cfg='{}'
  [[ -n "${cfg}" ]] || cfg='{}'
  m="$(jq -c '
    (.environments // null) as $e | ((.releaseFile // "") | if type == "string" and . != "" then . else "glidepath/envs/{env}.release.yaml" end) as $t
    | if ($e | type) != "array" then {} else
        [$e[] | select(type == "object" and ((.name // null) | type) == "string" and (.name | test("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$")))] as $named
        | [$named[] | .name as $n | ($t | split("{env}") | join($n)) as $p | select($p == "glidepath/envs/" + $n + ".release.yaml")
            | {key: $p, value: {env: $n, dup: (($named | map(select(.name == $n)) | length) != 1)}}]
        | from_entries end' <<<"${cfg}" 2>/dev/null)" || m='{}'
  [[ -n "${m}" ]] || m='{}'
  while IFS=$'\t' read -r p e dup; do
    [[ -n "${p}" ]] || continue
    c=true
    [[ "${dup}" == false ]] && c="$(_rs_release_file_counts "${e}" "$2")"
    out="$(jq -c --arg p "${p}" --arg e "${e}" --arg c "${c}" '. + {($p): {env: $e, counts: ($c != "false")}}' <<<"${out}")"
  done < <(jq -r 'to_entries[] | [.key, .value.env, (.value.dup | tostring)] | @tsv' <<<"${m}")
  printf '%s\n' "${out}"
}
# _rs_release_file_counts <environment of the release file> <released environment>: condition 4 of step 3b, in one place.
# Prints false when a platform commit that changes only that environment's release file cannot change what the released
# environment runs, true otherwise. A release file says which image its own environment runs; the released environment's
# image is built from source that never reads another environment's release file (the assumption the rule rests on), and it
# renders from its own release file. So a commit changing only another environment's release file is not part of this
# release, and one changing the released environment's own file is: it stays counted.
_rs_release_file_counts() {
  [[ -n "$1" && -n "$2" && "$1" != "$2" ]] && echo false || echo true
}
# _rs_app_pushes <token> <owner/repo> <branch> <app logins json> <shas json>: condition 2 of step 3b, read from GitHub's own
# record of who moved the branch (the repository activity API, GET /repos/{owner}/{repo}/activity). Prints a JSON object
# {"<sha>": "<before>"} with every sha of <shas> that GitHub records as the new tip (`after`) of a `push` to refs/heads/<branch>
# whose actor is one of <app logins> (an account GitHub types Bot); <before> is the tip that push moved from. The actor is the
# account that authenticated the ref update, so a person with write access cannot make it the App: only a holder of the App's
# installation token can. The query filters (ref, actor, activity_type) only narrow the listing; every field is checked here.
# Reads at most RS_ACTIVITY_PAGES pages of 100 per login (default 10), newest first, and stops once every sha is found; a sha
# past that is not found (kept counted). Follows only a next link on https://api.github.com/. Returns 1 on any read or parse
# error: then nothing is found.
_rs_app_pushes() {
  local tok="$1" repo="$2" ref="refs/heads/$3" logins="$4" want="$5" found='{}' login url body hdr page next
  hdr="$(mktemp)" || return 1
  for login in $(jq -r '.[]' <<<"${logins}"); do
    url="https://api.github.com/repos/${repo}/activity?ref=$(jq -rn --arg v "${ref}" '$v | @uri')&actor=$(jq -rn --arg v "${login}" '$v | @uri')&activity_type=push&per_page=100"
    page=1
    while [[ -n "${url}" ]] && (( page <= ${RS_ACTIVITY_PAGES:-10} )); do
      : > "${hdr}"
      body="$(_rs_api "${tok}" "${url}" -D "${hdr}")" || { rm -f "${hdr}"; return 1; }
      found="$(jq -c --argjson f "${found}" --argjson w "${want}" --argjson l "${logins}" --arg ref "${ref}" '
        if type != "array" then error("not a list") else . end
        | $f + ([.[] | select(type == "object" and .activity_type == "push" and .ref == $ref and (.actor.type // "") == "Bot"
                   and (((.actor.login // "") | ascii_downcase) as $a | $a != "" and ($l | index($a)) != null)
                   and ((.after // "") as $s | $w | index($s)) != null)
                 | {key: .after, value: (.before // "")}] | from_entries)' <<<"${body}")" || { rm -f "${hdr}"; return 1; }
      [[ "$(jq --argjson w "${want}" '. as $f | [$w[] | select(. as $s | $f | has($s) | not)] | length' <<<"${found}")" == 0 ]] && { rm -f "${hdr}"; printf '%s\n' "${found}"; return 0; }
      next="$(tr -d '\r' < "${hdr}" | grep -i '^link:' | tr ',' '\n' | sed -nE 's/.*<([^>]+)>; *rel="next".*/\1/p' | head -n 1)"
      [[ -z "${next}" || "${next}" == https://api.github.com/* ]] || next=""
      url="${next}"; page=$((page + 1))
    done
  done
  rm -f "${hdr}"
  printf '%s\n' "${found}"
}
# _rs_platform_commit <token> <owner/repo> <sha> <app logins json> <release paths json> <app pushes json or null>: prints nothing
# when the commit is a platform release-file commit (all four conditions of step 3b hold), else one line saying which does not.
# <app pushes> is _rs_app_pushes' answer (null: it could not be read).
_rs_platform_commit() {
  local c p1 cmp files bad
  [[ "$(jq 'length' <<<"$4")" -gt 0 ]] || { echo "the pipeline's GitHub App login is unknown (clearanceGate.pipelineLogins is empty and the release PR's author is not a Bot)"; return 0; }
  c="$(_rs_api "$1" "https://api.github.com/repos/$2/commits/$3")" || { echo "could not read the commit"; return 0; }
  [[ "$(jq -r '.commit.verification.verified == true' <<<"${c}" 2>/dev/null)" == true ]] || { echo "GitHub does not report it verified"; return 0; }
  p1="$(jq -r '.parents[0].sha // ""' <<<"${c}")"
  [[ "${p1}" =~ ^[0-9a-f]{40}$ ]] || { echo "it has no parent to compare with"; return 0; }
  # 2. GitHub's activity log, not the commit: an App's signed API commit always has GitHub (web-flow) as committer, and its
  #    author is an email anyone can write into a web-flow-signed commit of their own.
  [[ "${6:-null}" != null ]] || { echo "GitHub's activity log (who pushed it) could not be read"; return 0; }
  [[ "$(jq -r --arg s "$3" 'has($s)' <<<"$6" 2>/dev/null)" == true ]] \
    || { echo "GitHub's activity log does not show the pipeline's GitHub App pushing it to the released branch"; return 0; }
  [[ "$(jq -r --arg s "$3" '.[$s]' <<<"$6")" == "${p1}" ]] \
    || { echo "the App's push that made it the branch tip did not move the branch from its first parent"; return 0; }
  cmp="$(_rs_api "$1" "https://api.github.com/repos/$2/compare/${p1}...$3?per_page=1")" || { echo "could not read its diff"; return 0; }
  # GitHub lists at most 300 files of a compare: a full list is not provably complete.
  files="$(jq -c 'if .status == "ahead" and (.files | type) == "array" and (.files | length) > 0 and (.files | length) < 300
                  then [.files[] | .filename, (.previous_filename // empty)] | unique else null end' <<<"${cmp}" 2>/dev/null)" || files=null
  [[ -n "${files}" && "${files}" != null ]] || { echo "its diff against its first parent could not be read in full"; return 0; }
  bad="$(jq -r --argjson m "$5" '[.[] | select($m[.] == null)] | join(", ")' <<<"${files}")"
  [[ -z "${bad}" ]] || { echo "it changes files that are not release files: ${bad}"; return 0; }
  bad="$(jq -r --argjson m "$5" '[.[] | select($m[.].counts != false) | "\(.) (\($m[.].env))"] | join(", ")' <<<"${files}")"
  [[ -z "${bad}" ]] || { echo "it changes the release file of the environment being released: ${bad}"; return 0; }
}

release_source_resolve() {
  local owner="$1" repo="$2" prn="$3" files="$4" hdir="$5" bdir="$6" pr_author="${7:-}" pr_author_type="${8:-}" rcfg="${9:-}"
  local pr_url="https://github.com/${owner}/${repo}/pull/${prn}" records rec tok n
  records="$(kubectl get configmap -n "${NAMESPACE}" -l "hangar.io/subcomponent=release-tracking,hangar.io/app=${APP_NAME}" -o json)" \
    || { echo "could not read the release records in ${NAMESPACE}: cannot tell whether ${pr_url} is a pipeline release" >&2; return 1; }
  local nrec
  nrec="$(jq --arg u "${pr_url}" '[.items[] | select(.data.prUrl == $u)] | length' <<<"${records}")"
  if (( nrec > 1 )); then
    jq -nc --arg r "${nrec} release records name ${pr_url}; the pipeline writes one, so none is trusted" '{applicable: true, resolved: false, reason: $r}'
    return 0
  fi
  rec="$(jq -c --arg u "${pr_url}" '[.items[] | select(.data.prUrl == $u)] | .[0].data // empty' <<<"${records}")"
  if [[ -z "${rec}" ]]; then
    # No record names this PR. A person's own PR is unchanged. A PR authored by the pipeline's own App (a configured
    # login, never a hardcoded one) with no record is not a person's change and its source is unknown.
    local pl
    for pl in $(tr ',' ' ' <<<"${PIPELINE_LOGINS:-}"); do
      if [[ -n "${pr_author}" && "$(tr 'A-Z' 'a-z' <<<"${pl}")" == "$(tr 'A-Z' 'a-z' <<<"${pr_author}")" ]]; then
        jq -nc --arg r "the PR is authored by the pipeline's GitHub App (${pr_author}) but no release record names it (swept, or not written by this pipeline), so the change it releases is unknown" \
          '{applicable: true, resolved: false, reason: $r}'
        return 0
      fi
    done
    if [[ "${pr_author_type}" == Bot && -z "${PIPELINE_LOGINS:-}" ]]; then
      jq -nc '{applicable: false, warning: "the PR is authored by a Bot account and no release record names it, but clearanceGate.pipelineLogins is empty, so it is treated as a person'"'"'s change. If this is the pipeline'"'"'s GitHub App, set clearanceGate.pipelineLogins to its REST login (name[bot])."}'
    else
      jq -nc '{applicable: false}'
    fi
    return 0
  fi
  local rkind renv rcluster rimage rdigest rrev rurl rinit rid rtag base_info rrepo rsource rappns
  _rs_field() { jq -r --arg k "$1" '.[$k] // ""' <<<"${rec}"; }
  rkind="$(_rs_field kind)"; renv="$(_rs_field env)"; rcluster="$(_rs_field cluster)"; rimage="$(_rs_field image)"
  rdigest="$(_rs_field imageDigest)"; rrev="$(_rs_field gitRevision)"; rurl="$(_rs_field gitUrl)"
  rinit="$(_rs_field initiator)"; rid="$(_rs_field releaseId)"; rtag="${rimage##*:}"
  rrepo="$(_rs_field imageRepository)"; rsource="$(_rs_field imageSource)"; rappns="$(_rs_field appNamespace)"
  base_info="$(jq -nc --arg k "${rkind}" --arg i "${rinit}" --arg id "${rid}" --arg rev "${rrev}" --arg img "${rimage}" \
    '{releaseKind: $k, releaseId: $id, promoter: $i, releasedCommit: $rev, image: $img}')"
  _rs_fail() { jq -nc --arg r "$1" --argjson b "${base_info}" '{applicable: true, resolved: false, reason: $r} + $b'; }

  # 1. Is this PR what the pipeline wrote for that record? One commit; one file, <cluster>/<env>/{release,values}.yaml;
  #    nothing in it differs from base but the image fields; the head's image is the record's.
  tok="$(github_app_installation_token "${owner}" "${repo}")" || { _rs_fail "no GitHub token for ${owner}/${repo}"; return 0; }
  local prj
  prj="$(_rs_api "${tok}" "https://api.github.com/repos/${owner}/${repo}/pulls/${prn}")" || { _rs_fail "could not read the release PR"; return 0; }
  n="$(jq -r '.commits' <<<"${prj}")" || { _rs_fail "could not read the release PR"; return 0; }
  if [[ "${n}" != 1 ]]; then
    _rs_fail "the release PR has ${n} commits, the pipeline writes exactly one: it was changed after the pipeline opened it"; return 0
  fi
  local manifest
  manifest="$(jq -r '.[0].filename // ""' <<<"${files}")"
  if [[ -z "${rcluster}" || -z "${renv}" || "$(jq 'length' <<<"${files}")" != 1 || "${manifest}" != "${rcluster}/${renv}/release.yaml" && "${manifest}" != "${rcluster}/${renv}/values.yaml" ]]; then
    _rs_fail "the release PR changes $(jq -r '[.[].filename] | join(", ")' <<<"${files}"), not just the one manifest the record names (${rcluster}/${renv}/release.yaml or values.yaml): it was changed after the pipeline opened it"; return 0
  fi
  local strip='(. // {}) | del(.release.image.repository, .release.image.tag, .release.image.digest, .release.image.source, .rollout.image, .releaseTracking) | walk(if type == "object" then with_entries(select(.value != {})) else . end)'
  local head_json base_json
  head_json="$(yq -o json '.' "${hdir}/${manifest}" 2>/dev/null || true)"; [[ -n "${head_json}" ]] || head_json='{}'
  base_json='{}'
  if [[ -f "${bdir}/${manifest}" ]]; then
    base_json="$(yq -o json '.' "${bdir}/${manifest}" 2>/dev/null || true)"; [[ -n "${base_json}" ]] || base_json='{}'
  fi
  if [[ "$(jq -cS "${strip}" <<<"${head_json}")" != "$(jq -cS "${strip}" <<<"${base_json}")" ]]; then
    _rs_fail "the release PR changes more than the image in ${manifest}: it was changed after the pipeline opened it"; return 0
  fi
  if [[ -z "${rtag}" || "$(jq -r '(. // {}) | .release.image.tag // ""' <<<"${head_json}")" != "${rtag}" ]]; then
    _rs_fail "the image in the release PR ($(jq -r '(. // {}) | .release.image.tag // "none"' <<<"${head_json}")) is not the record's (${rtag:-none})"; return 0
  fi
  # Every identity-bearing field the comparison above strips is checked against the record, none left unchecked. A record
  # without the repository (written before the gate kept it) cannot vouch for the PR: unresolved, the safe choice.
  if [[ -z "${rrepo}" ]]; then
    _rs_fail "the release record names no image repository (it predates the gate keeping it), so the PR's image cannot be checked against it"; return 0
  fi
  if [[ "$(jq -r '(. // {}) | [(.release.image // {}) | .repository // "", .source // "", .digest // ""] | join("|")' <<<"${head_json}")" != "${rrepo}|${rsource}|${rdigest}" ]]; then
    _rs_fail "the image repository, source or digest in the release PR is not the one the pipeline recorded for this release (a record with no digest means the PR must carry none either)"; return 0
  fi
  if [[ "$(jq -r '(. // {}) | [((.release.image // {}) | keys - ["repository","tag","digest","source"] | length), ((.rollout // {}) | has("image")), ((.releaseTracking // {}) | keys - ["cluster","appNamespace","releaseId"] | length), (.releaseTracking.releaseId // ""), (.releaseTracking.cluster // ""), (.releaseTracking.appNamespace // "")] | map(tostring) | join("|")' <<<"${head_json}")" != "0|false|0|${rid}|${rcluster}|${rappns}" ]]; then
    _rs_fail "the release PR's image or releaseTracking fields are not what the pipeline writes for this record"; return 0
  fi
  local rurl_norm
  rurl_norm="$(sed -E 's#(\.git)?/?$##' <<<"${rurl}")"
  if [[ ! "${rrev}" =~ ^[0-9a-f]{40}$ ]] || [[ ! "${rurl_norm}" =~ ^https://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)$ ]]; then
    _rs_fail "the release record names no usable source commit (${rurl:-no repo}@${rrev:-none})"; return 0
  fi
  local sowner="${BASH_REMATCH[1]}" srepo="${BASH_REMATCH[2]}" tag_sha=""
  [[ "${rtag}" =~ -([0-9a-f]{7,40})$ ]] && tag_sha="${BASH_REMATCH[1]}"
  if [[ -n "${tag_sha}" && "${rrev}" != "${tag_sha}"* ]]; then
    _rs_fail "the image tag ${rtag} does not end in the record's source commit ${rrev:0:7}"; return 0
  fi

  # 2. The commits this release carries. A rollback (the record says so, or the image is older than the one the
  #    environment runs) restores one commit that was released before: that commit alone. A release of the very same
  #    commit is that commit alone too. Otherwise previous release -> this release.
  local stok mode="range" prev="" prev_rec base_tag base_digest cmp status commits='[]' total
  stok="$(github_app_installation_token "${sowner}" "${srepo}")" || { _rs_fail "no GitHub token for ${sowner}/${srepo}"; return 0; }
  # The released branch: the source repo's default branch, from GitHub (the pipeline records no branch, and the PR's own
  # claim is never used). A covering PR must have been merged into it.
  local meta rbranch
  meta="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}")" || { _rs_fail "could not read ${sowner}/${srepo}"; return 0; }
  rbranch="$(jq -r '.default_branch // ""' <<<"${meta}")" || rbranch=""
  [[ -n "${rbranch}" ]] || { _rs_fail "could not tell the released branch of ${sowner}/${srepo}"; return 0; }
  if [[ "${rkind}" == rollback ]]; then
    mode=rollback
  else
    base_tag="$(jq -r '(. // {}) | .release.image.tag // .rollout.image.tag // ""' <<<"${base_json}")"
    base_digest="$(jq -r '(. // {}) | .release.image.digest // ""' <<<"${base_json}")"
    if [[ -z "${base_tag}" ]]; then
      mode=first   # nothing released here before: the range is the single released commit
    fi
  fi
  if [[ "${rkind}" != rollback && "${mode}" != first ]]; then
    prev_rec="$(jq -c --arg env "${renv}" --arg cl "${rcluster}" --arg t "${base_tag}" --arg d "${base_digest}" --arg me "${rid}" '
      [.items[].data | select(.env == $env and .cluster == $cl and .releaseId != $me and (.mergedAt // "") != ""
         and ((.image // "") | endswith(":" + $t)) and ($d == "" or (.imageDigest // "") == "" or .imageDigest == $d))]
      | sort_by(.mergedAt) | last // empty' <<<"${records}")"
    if [[ -z "${prev_rec}" ]]; then
      _rs_fail "no release record for the image the environment runs now (${base_tag}); the record may have been swept, so the commits this release carries are unknown"; return 0
    fi
    prev="$(jq -r '.gitRevision // ""' <<<"${prev_rec}")"
    if [[ ! "${prev}" =~ ^[0-9a-f]{40}$ || "$(jq -r '.gitUrl // ""' <<<"${prev_rec}" | sed -E 's#(\.git)?/?$##')" != "$(sed -E 's#(\.git)?/?$##' <<<"${rurl}")" ]]; then
      _rs_fail "the previous release's record names no usable source commit in ${sowner}/${srepo}"; return 0
    fi
    cmp="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/compare/${prev}...${rrev}?per_page=1")" \
      || { _rs_fail "could not compare ${prev:0:7}...${rrev:0:7} in ${sowner}/${srepo}"; return 0; }
    status="$(jq -r '.status' <<<"${cmp}")"; total="$(jq -r '.total_commits' <<<"${cmp}")"
    if (( total > 250 )); then
      _rs_fail "${prev:0:7}...${rrev:0:7} is ${total} commits; GitHub lists at most 250, so the authors cannot all be named"; return 0
    fi
    case "${status}" in
      identical) mode=single ;;
      behind) mode=rollback ;;
      ahead|diverged)
        commits="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/compare/${prev}...${rrev}" '.commits' 3 '.commits')" \
          || { _rs_fail "could not list the commits of ${prev:0:7}...${rrev:0:7} (or more than 300)"; return 0; }
        # total_commits (above, at most 250) is what the range holds: fewer listed than that is not read in full, so not trusted.
        if (( total > $(jq 'length' <<<"${commits}") )); then
          _rs_fail "${prev:0:7}...${rrev:0:7} holds ${total} commits and only $(jq 'length' <<<"${commits}") were listed; the range is not read in full, so it is not trusted"; return 0
        fi ;;
      *) _rs_fail "GitHub compared ${prev:0:7}...${rrev:0:7} as '${status}'"; return 0 ;;
    esac
  fi
  if [[ "${mode}" != range ]]; then   # rollback, single, first
    commits="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/commits/${rrev}")" \
      || { _rs_fail "could not read ${sowner}/${srepo}@${rrev:0:7}"; return 0; }
    commits="$(jq -c '[.]' <<<"${commits}")" || { _rs_fail "could not read ${sowner}/${srepo}@${rrev:0:7}"; return 0; }
  fi
  commits="$(jq -c '[.[] | {sha}]' <<<"${commits}")"
  if [[ "$(jq 'length' <<<"${commits}")" == 0 ]]; then
    _rs_fail "no commits between ${prev:0:7} and ${rrev:0:7}"; return 0
  fi

  # 3. Each commit of the range, merge commits included, maps to the merged pull request that contains it: the commit is that
  #    PR's merge commit or one of its commits. Identity is the PR's opener and merger (authenticated GitHub accounts),
  #    never a commit's author or committer (GitHub takes the author login from an email the pusher sets). A commit no merged
  #    PR contains (a direct push, a merge commit made outside a PR) is "no PR": it cannot be vouched for.
  #    Each PR's provenance is Clearance's own verdict (POST /v1/provenance, the body agent-gates sends, with every
  #    commit's co-author trailers).
  local rc covered='[]' prs='[]' nopr='[]' sha pulls p pn phead popener pmerger pmerged pmsha pcommits pfiles body verdict code out ident agent login
  while IFS= read -r sha; do
    [[ -n "${sha}" ]] || continue
    jq -e --arg s "${sha}" 'index($s) != null' <<<"${covered}" >/dev/null && continue
    rc=0; pulls="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/commits/${sha}/pulls" '[.[] | select(.merged_at != null) | .number]' 3)" || rc=$?
    if (( rc == 3 )); then
      _rs_fail "${sowner}/${srepo}@${sha:0:7} is associated with 300 or more pull requests; the list is not read in full, so it is not trusted"; return 0
    elif (( rc != 0 )); then
      _rs_fail "could not find the pull request of ${sowner}/${srepo}@${sha:0:7}"; return 0
    fi
    for pn in $(jq -r '.[]' <<<"${pulls}"); do
      jq -e --argjson n "${pn}" 'any(.[]; .number == $n)' <<<"${prs}" >/dev/null && continue
      p="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/pulls/${pn}")" \
        || { _rs_fail "could not read ${sowner}/${srepo}#${pn}"; return 0; }
      pcommits="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/pulls/${pn}/commits" '.' 3)" \
        || { _rs_fail "could not read the commits of ${sowner}/${srepo}#${pn}"; return 0; }
      pfiles="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/pulls/${pn}/files" '[.[] | .filename, (.previous_filename // empty)]' 30)" \
        || { _rs_fail "could not read the files of ${sowner}/${srepo}#${pn}"; return 0; }
      # Only a PR merged INTO the released branch of THIS repository covers a commit: a PR from a branch containing the
      # commit merged into a side branch, or into another repository, proves nothing about the commit on the released branch.
      if [[ "$(jq -r --arg b "${rbranch}" --arg r "${sowner}/${srepo}" '(.merged_at != null) and (.base.ref == $b) and ((.base.repo.full_name // "" | ascii_downcase) == ($r | ascii_downcase))' <<<"${p}")" != true ]]; then
        continue
      fi
      phead="$(jq -r '.head.sha' <<<"${p}")"; popener="$(jq -r '.user.login // ""' <<<"${p}")"
      pmerger="$(jq -r '.merged_by.login // ""' <<<"${p}")"; pmerged="$(jq -r '.merged_at // ""' <<<"${p}")"
      pmsha="$(jq -r '.merge_commit_sha // ""' <<<"${p}")"
      local poType pmType plines
      poType="$(jq -r '.user.type // ""' <<<"${p}")"; pmType="$(jq -r '.merged_by.type // ""' <<<"${p}")"
      plines="$(jq -r '(.additions // 0) + (.deletions // 0)' <<<"${p}")"
      for login in "${popener}" "${pmerger}"; do
        # A GitHub login (letters, digits, - _ . and the [bot] suffix) must fit Clearance's principal pattern, which is wider
        # (it also allows : / @); GitHub logins never contain those, so the narrower class is deliberate.
        if [[ ! "${login}" =~ ^[]A-Za-z0-9_.@[-]{1,190}$ ]]; then
          _rs_fail "${sowner}/${srepo}#${pn} has an opener or merger (${popener:-none}, ${pmerger:-none}) Clearance cannot record as a principal"; return 0
        fi
      done
      covered="$(jq -c --argjson c "$(jq -c '[.[].sha]' <<<"${pcommits}")" --arg m "${pmsha}" '. + $c + [$m]' <<<"${covered}")"
      body="$(jq -nc --arg repo "${sowner}/${srepo}" --argjson pr "${pn}" --arg head "${phead}" --arg author "${popener}" \
          --slurpfile pathsf <(_rs_json "${pfiles}") --slurpfile commitsf <(_rs_json "${pcommits}") '
        $pathsf[0] as $paths | $commitsf[0] as $commits
        | ([$commits[] | select(.sha == $head)][0].commit.message // "") as $head_msg
        | {repo: $repo, pr: $pr, head: $head, author: $author, paths: $paths,
           trailers: ([$head_msg | splits("\n") | capture("^Hangar-(?<k>Task|Session|Run|Agent): *(?<v>[^ ]+) *$")? | {key: (.k | ascii_downcase), value: .v}] | from_entries),
           coauthors: ([$commits[] | .sha as $s | (.commit.message | splits("\n")
             | select(test("^(Co-authored-by|Assisted-by|Generated-by):"; "i") and length <= 1024)) | {commit: $s, trailer: .}] | .[0:200])}')"
      ident=unknown; agent=""
      if [[ -n "${CLEARANCE_URL:-}" ]]; then
        out="$(curl --silent --show-error --max-time 20 --retry 2 --retry-connrefused -w '\n%{http_code}' \
                 -H "Authorization: Bearer $(cat /var/run/secrets/clearance/clearance-token)" -H "Content-Type: application/json" \
                 --data @- "${CLEARANCE_URL%/}/v1/provenance" < <(_rs_json "${body}") || true)"
        code="${out##*$'\n'}"
        if [[ "${code}" == 200 ]]; then
          verdict="${out%$'\n'*}"
          ident="$(jq -r '.identity.status // "unknown"' <<<"${verdict}")"
          agent="$(jq -r '.record.agent // ""' <<<"${verdict}")"
        fi
      fi
      case "${ident}" in confirmed|not-agent|unattested) ;; *) ident=unknown ;; esac
      prs="$(jq -c --argjson n "${pn}" --arg a "${popener}" --arg at "${poType}" --arg m "${pmerger}" --arg mt "${pmType}" --arg t "${pmerged}" --argjson l "${plines}" --arg i "${ident}" --arg g "${agent}" \
        '. + [{number: $n, author: $a, authorType: $at, merger: $m, mergerType: $mt, mergedAt: $t, lines: $l, identity: $i, agent: $g}]' <<<"${prs}")"
    done
    jq -e --arg s "${sha}" 'index($s) != null' <<<"${covered}" >/dev/null || nopr="$(jq -c --arg s "${sha}" '. + [$s]' <<<"${nopr}")"
  done < <(jq -r '.[].sha' <<<"${commits}")
  # 3b. Platform release-file commits (docs/admin/clearance-gate.md, "Platform release-file commits"). deploy-manifests writes a
  #    Ground environment's image to <folder>/envs/<env>.release.yaml with a commit straight to main on every deploy: no PR
  #    contains it, so it would be "no PR" and make every later release of the app `unknown`. Such a commit is dropped from
  #    noPr only if ALL of these hold, and kept otherwise (any read error keeps it too):
  #      1. GitHub reports it verified (commit.verification.verified): deploy-manifests creates it through GitHub's API with
  #         the App's installation token and no author or committer, so GitHub signs it;
  #      2. the pipeline's GitHub App pushed it: GitHub's repository activity records a `push` to the released branch whose new
  #         tip is this commit, moved from its first parent, by the App (_rs_app_pushes). The App is a login in
  #         clearanceGate.pipelineLogins, or, when that is empty, the author of this release PR when GitHub types it Bot (the
  #         pipeline opened the PR with the same App: step 1 checked the PR is exactly what the pipeline's record says it
  #         wrote). The commit's own committer and author prove nothing (W-48): GitHub signs an App's API commit with GitHub
  #         (web-flow) as committer, and anyone with write access can create a web-flow-signed commit naming the App's bot as
  #         author. The activity's actor is whoever authenticated the ref update, which only the App's token can make the App;
  #      3. every file of its full diff against its first parent (a merge commit's too) is a resolved release-file path;
  #      4. none of those paths is the release file of the environment this PR releases to (_rs_release_file_counts):
  #         the commit can only change what another environment runs. A commit that touches the released environment's
  #         own file, alone or with another environment's, stays counted.
  #    Assumes a build never reads another environment's release file (the file only says which image that environment runs).
  local platform='[]' applogins rpaths why kept='[]'
  if [[ "$(jq 'length' <<<"${nopr}")" -gt 0 ]]; then
    applogins="$(jq -nc --arg s "${PIPELINE_LOGINS:-}" --arg a "${pr_author}" --arg t "${pr_author_type}" '
      ($s | ascii_downcase | split(",") | map(select(. != ""))) as $l
      | if ($l | length) > 0 then $l elif $t == "Bot" and ($a | ascii_downcase | endswith("[bot]")) then [$a | ascii_downcase] else [] end')"
    rpaths="$(_rs_release_paths "${rcfg}" "${renv}")"
    local pushes=null
    if [[ "$(jq 'length' <<<"${applogins}")" -gt 0 ]]; then
      pushes="$(_rs_app_pushes "${stok}" "${sowner}/${srepo}" "${rbranch}" "${applogins}" "${nopr}")" || pushes=null
      [[ -n "${pushes}" ]] || pushes=null
    fi
    while IFS= read -r sha; do
      [[ -n "${sha}" ]] || continue
      why="$(_rs_platform_commit "${stok}" "${sowner}/${srepo}" "${sha}" "${applogins}" "${rpaths}" "${pushes}")"
      if [[ -z "${why}" ]]; then
        platform="$(jq -c --arg s "${sha}" '. + [$s]' <<<"${platform}")"
      else
        kept="$(jq -c --arg s "${sha}" '. + [$s]' <<<"${kept}")"
        echo "release source: ${sha:0:7} stays a commit no merged PR contains: ${why}" >&2
      fi
    done < <(jq -r '.[]' <<<"${nopr}")
    nopr="${kept}"
  fi
  if [[ "$(jq 'length' <<<"${prs}")" == 0 ]]; then
    _rs_fail "no commit of the range maps to a merged pull request (commits: $(jq -r 'map(.[0:7]) | join(", ")' <<<"${nopr}")), so its author cannot be named" ; return 0
  fi

  # 4. The weakest verdict of the source, in the order unknown < unattested < not-agent < confirmed. `confirmed` (an agent
  #    opened it through Clearance and nothing was added) is claimed only if every source PR is confirmed. One PR with an
  #    AI co-author trailer Clearance has no record of makes the release `unattested`; a release of only PRs with no AI
  #    signal is `not-agent` (a person's change); a commit no merged PR contains, a forged trailer, a modified agent PR or
  #    an answer Clearance could not give (any verdict that is not one of the three) makes it `unknown`.
  #    The proposer is ONE human, chosen so the cheapest steers fail: bot accounts (a login ending [bot], an account of API
  #    type Bot, or one in PIPELINE_LOGINS) are never chosen; among human openers, the opener of the source PR with the most
  #    changed lines (ties: the newest merged); if every opener is a bot, the human merger of the newest PR; with no human
  #    at all the kind is unknown. Clearance takes one proposer, so the other openers and every merger are recorded
  #    (authors, mergers) but not yet excluded.
  local pl
  pl="$(jq -nc --arg s "${PIPELINE_LOGINS:-}" '$s | ascii_downcase | split(",") | map(select(. != ""))')"
  jq -nc --argjson base "${base_info}" --arg mode "${mode}" --arg repo "${sowner}/${srepo}" --arg prev "${prev}" --arg head "${rrev}" \
    --slurpfile commitsf <(_rs_json "${commits}") --slurpfile prsf <(_rs_json "${prs}") --slurpfile noprf <(_rs_json "${nopr}") --argjson pl "${pl}" --argjson platform "${platform}" '
    $commitsf[0] as $commits | $prsf[0] as $prs | $noprf[0] as $nopr |
    def rank: {"unknown": 0, "unattested": 1, "not-agent": 2, "confirmed": 3}[.];
    def tally(f): group_by(f) | map({login: (.[0] | f), prs: length}) | sort_by(.login);
    def isbot($login; $type): ($login | ascii_downcase) as $l | ($l | endswith("[bot]")) or ($type == "Bot") or ($pl | index($l) != null);
    ($prs | map(.identity) + (if ($nopr | length) > 0 then ["unknown"] else [] end) | sort_by(rank) | .[0]) as $prov
    | ($prs | map(select(.identity == "confirmed" and .agent != "") | .agent) | unique) as $agents
    | ($prs | map(select(isbot(.author; .authorType) | not)) | sort_by(.lines, .mergedAt, .number) | last) as $byOpener
    | ($prs | map(select(.merger != "" and (isbot(.merger; .mergerType) | not))) | sort_by(.mergedAt, .number) | last) as $byMerger
    | (if $byOpener != null then {who: $byOpener.author, rule: "opener of the source PR with the most changed lines"}
       elif $byMerger != null then {who: $byMerger.merger, rule: "human merger of the newest source PR (every opener is a bot)"}
       else {who: ($prs | sort_by(.mergedAt, .number) | last | .author), rule: "none: no human opened or merged any source PR"} end) as $pick
    | $base + {applicable: true, resolved: true, mode: $mode, sourceRepo: $repo, previousCommit: $prev, releasedCommit: $head,
        commits: ($commits | length), authors: ($prs | tally(.author)), mergers: ($prs | tally(.merger)), prs: $prs, noPr: $nopr, platformCommits: $platform,
        provenance: $prov, agents: $agents, proposerRule: $pick.rule,
        proposerKind: (if $pick.rule | startswith("none") then "unknown" elif $prov == "confirmed" then "agent" elif $prov == "unknown" then "unknown" else "person" end)}
    | .proposer = (if .proposerKind == "agent" and ($agents | length) > 0 then "autopilot-runs/" + $agents[0] else "user:" + $pick.who end)
    # Why the proposer is unknown, for Clearance (POST /v1/clearances provenanceReason, clearance docs/gate.md "Unvouched
    # commits"): `unvouched-commits` (held at tier 3 for two people) only when the one cause is commits no merged PR
    # contains: some remain, every source PR is vouched for (confirmed, not-agent or unattested) and a human was named;
    # `unrecorded-agent` (denied) when a source PR itself is unknown (a forged trailer, a modified or unrecorded agent PR,
    # no answer from Clearance); nothing otherwise (no human at all), which Clearance also denies.
    | (if .proposerKind != "unknown" then {}
       elif ($nopr | length) > 0 and all($prs[]; .identity == "confirmed" or .identity == "not-agent" or .identity == "unattested")
            and ($pick.rule | startswith("none") | not)
         then {provenanceReason: "unvouched-commits", unvouchedCommits: ($nopr | unique)}
       elif any($prs[]; .identity == "unknown") then {provenanceReason: "unrecorded-agent"}
       else {} end) as $why
    | . + $why
    | .agent = (if .proposerKind == "agent" and ($agents | length) > 0 then $agents[0] else null end)'
}
