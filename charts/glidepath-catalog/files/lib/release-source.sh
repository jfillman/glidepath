# release-source.sh: the source of a pipeline release PR, for the clearance-gate Task (docs/admin/clearance-gate.md,
# "Who proposed a release"). glidepath-catalog.releaseSourceLib splices it into the fetch step.
#
# A release PR in the gitops repo is written by the pipeline's GitHub App, so its author and commit say nothing about the
# change being released. The change was written in the app repo. This resolves who wrote it and what Clearance's
# provenance says about it, from what the pipeline recorded and can verify, never from text the PR carries:
#
#   release_source_resolve <owner> <repo> <pr> <files json> <head dir> <base dir>     prints one JSON object
#
# Reads: the release record (release-tracking-<chainId> in this namespace, written by open-release-pr; only the
# pipeline's ServiceAccount can write it), the PR's commit count, the manifest at base and head (checked out), the
# app repo's compare, commit and pull-request APIs (the GitHub App's installation token), and Clearance's
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
# Trusted: the release record (pipeline-written: image, digest, source repo and commit, kind, initiator), the app repo's
# git history and PR metadata as GitHub serves them, Clearance's provenance verdicts. NOT trusted: the release PR's
# author, title, body, branch name or commit message, or anything in it beyond the image fields the record describes.
# The PR is accepted as the pipeline's only if its content is exactly what the record says the pipeline wrote (one
# commit, one manifest file, nothing changed in it but the image the record names); anything else is unresolved.

_rs_api() { # <token> <url>
  curl --fail --silent --show-error --retry 3 --retry-connrefused --max-time 20 \
    -H "Authorization: Bearer $1" -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" "$2"
}
_rs_pages() { # <token> <url without paging> <jq filter turning a page into an array> <max pages>: the arrays joined
  local page=1 out='[]' body part sep='?'
  [[ "$2" == *\?* ]] && sep='&'
  while (( page <= $4 )); do
    body="$(_rs_api "$1" "$2${sep}per_page=100&page=${page}")" || return 1
    part="$(jq -c "$3" <<<"${body}")" || return 1
    out="$(jq -c --argjson more "${part}" '. + $more' <<<"${out}")"
    (( $(jq 'length' <<<"${part}") < 100 )) && break
    page=$((page + 1))
  done
  printf '%s' "${out}"
}

release_source_resolve() {
  local owner="$1" repo="$2" prn="$3" files="$4" hdir="$5" bdir="$6"
  local pr_url="https://github.com/${owner}/${repo}/pull/${prn}" records rec tok n
  records="$(kubectl get configmap -n "${NAMESPACE}" -l "hangar.io/subcomponent=release-tracking,hangar.io/app=${APP_NAME}" -o json)" \
    || { echo "could not read the release records in ${NAMESPACE}: cannot tell whether ${pr_url} is a pipeline release" >&2; return 1; }
  rec="$(jq -c --arg u "${pr_url}" '[.items[] | select(.data.prUrl == $u)] | .[0].data // empty' <<<"${records}")"
  if [[ -z "${rec}" ]]; then
    jq -nc '{applicable: false}'
    return 0
  fi
  local rkind renv rcluster rimage rdigest rrev rurl rinit rid rtag base_info
  _rs_field() { jq -r --arg k "$1" '.[$k] // ""' <<<"${rec}"; }
  rkind="$(_rs_field kind)"; renv="$(_rs_field env)"; rcluster="$(_rs_field cluster)"; rimage="$(_rs_field image)"
  rdigest="$(_rs_field imageDigest)"; rrev="$(_rs_field gitRevision)"; rurl="$(_rs_field gitUrl)"
  rinit="$(_rs_field initiator)"; rid="$(_rs_field releaseId)"; rtag="${rimage##*:}"
  base_info="$(jq -nc --arg k "${rkind}" --arg i "${rinit}" --arg id "${rid}" --arg rev "${rrev}" --arg img "${rimage}" \
    '{releaseKind: $k, releaseId: $id, promoter: $i, releasedCommit: $rev, image: $img}')"
  _rs_fail() { jq -nc --arg r "$1" --argjson b "${base_info}" '{applicable: true, resolved: false, reason: $r} + $b'; }

  # 1. Is this PR what the pipeline wrote for that record? One commit; one file, <cluster>/<env>/{release,values}.yaml;
  #    nothing in it differs from base but the image fields; the head's image is the record's.
  tok="$(github_app_installation_token "${owner}" "${repo}")" || { _rs_fail "no GitHub token for ${owner}/${repo}"; return 0; }
  n="$(_rs_api "${tok}" "https://api.github.com/repos/${owner}/${repo}/pulls/${prn}" | jq -r '.commits')" \
    || { _rs_fail "could not read the release PR"; return 0; }
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
  if [[ -n "${rdigest}" && "$(jq -r '(. // {}) | .release.image.digest // ""' <<<"${head_json}")" != "${rdigest}" ]]; then
    _rs_fail "the image digest in the release PR is not the digest the pipeline recorded for this release"; return 0
  fi
  if [[ ! "${rrev}" =~ ^[0-9a-f]{40}$ ]] || [[ ! "${rurl}" =~ ^https://github\.com/([^/]+)/([^/.]+)(\.git)?/?$ ]]; then
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
  if [[ "${rkind}" == rollback ]]; then
    mode=rollback
  else
    base_tag="$(jq -r '(. // {}) | .release.image.tag // .rollout.image.tag // ""' <<<"${base_json}")"
    base_digest="$(jq -r '(. // {}) | .release.image.digest // ""' <<<"${base_json}")"
    if [[ -z "${base_tag}" ]]; then
      _rs_fail "the environment has no earlier release (nothing at base to compare with), so the commits this release carries are unknown"; return 0
    fi
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
        commits="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/compare/${prev}...${rrev}" '.commits' 3)" \
          || { _rs_fail "could not list the commits of ${prev:0:7}...${rrev:0:7}"; return 0; } ;;
      *) _rs_fail "GitHub compared ${prev:0:7}...${rrev:0:7} as '${status}'"; return 0 ;;
    esac
  fi
  if [[ "${mode}" != range ]]; then
    commits="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/commits/${rrev}" | jq -c '[.]')" \
      || { _rs_fail "could not read ${sowner}/${srepo}@${rrev:0:7}"; return 0; }
  fi
  commits="$(jq -c '[.[] | {sha, parents: (.parents | length), login: (.author.login // "")}]' <<<"${commits}")"
  if [[ "$(jq 'length' <<<"${commits}")" == 0 ]]; then
    _rs_fail "no commits between ${prev:0:7} and ${rrev:0:7}"; return 0
  fi

  # 3. Authors: the account of each non-merge commit (a merge commit's author is whoever merged, not who wrote). Named
  #    most commits first, then by login, so the first is deterministic.
  local authors noacct
  noacct="$(jq -r '[.[] | select(.parents < 2 and .login == "") | .sha[0:7]] | join(", ")' <<<"${commits}")"
  if [[ -n "${noacct}" ]]; then
    _rs_fail "commit(s) ${noacct} have no GitHub account for their author, so the author cannot be named"; return 0
  fi
  authors="$(jq -c '(map(select(.parents < 2))) as $w | (if ($w | length) > 0 then $w else . end)
    | group_by(.login) | map({login: .[0].login, commits: length}) | sort_by(-.commits, .login)' <<<"${commits}")"

  # 4. Each source pull request's provenance, as Clearance's own verdict (POST /v1/provenance, the body agent-gates
  #    sends, with every commit's co-author trailers). A commit with no merged pull request cannot be asked about.
  local covered='[]' prs='[]' nopr='' sha pulls p pn phead pauthor pcommits pfiles body verdict code out ident agent
  while IFS= read -r sha; do
    [[ -n "${sha}" ]] || continue
    jq -e --arg s "${sha}" 'index($s) != null' <<<"${covered}" >/dev/null && continue
    pulls="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/commits/${sha}/pulls" | jq -c '[.[] | select(.merged_at != null) | .number]')" \
      || { _rs_fail "could not find the pull request of ${sowner}/${srepo}@${sha:0:7}"; return 0; }
    if [[ "${pulls}" == "[]" ]]; then nopr="${nopr:+${nopr}, }${sha:0:7}"; continue; fi
    for pn in $(jq -r '.[]' <<<"${pulls}"); do
      p="$(_rs_api "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/pulls/${pn}")" \
        || { _rs_fail "could not read ${sowner}/${srepo}#${pn}"; return 0; }
      pcommits="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/pulls/${pn}/commits" '.' 3)" \
        || { _rs_fail "could not read the commits of ${sowner}/${srepo}#${pn}"; return 0; }
      pfiles="$(_rs_pages "${stok}" "https://api.github.com/repos/${sowner}/${srepo}/pulls/${pn}/files" '[.[] | .filename, (.previous_filename // empty)]' 30)" \
        || { _rs_fail "could not read the files of ${sowner}/${srepo}#${pn}"; return 0; }
      covered="$(jq -c --argjson c "$(jq -c '[.[].sha]' <<<"${pcommits}")" '. + $c' <<<"${covered}")"
      phead="$(jq -r '.head.sha' <<<"${p}")"; pauthor="$(jq -r '.user.login' <<<"${p}")"
      body="$(jq -nc --arg repo "${sowner}/${srepo}" --argjson pr "${pn}" --arg head "${phead}" --arg author "${pauthor}" \
          --argjson paths "${pfiles}" --argjson commits "${pcommits}" '
        ([$commits[] | select(.sha == $head)][0].commit.message // "") as $head_msg
        | {repo: $repo, pr: $pr, head: $head, author: $author, paths: $paths,
           trailers: ([$head_msg | splits("\n") | capture("^Hangar-(?<k>Task|Session|Run|Agent): *(?<v>[^ ]+) *$")? | {key: (.k | ascii_downcase), value: .v}] | from_entries),
           coauthors: ([$commits[] | .sha as $s | (.commit.message | splits("\n")
             | select(test("^(Co-authored-by|Assisted-by|Generated-by):"; "i") and length <= 1024)) | {commit: $s, trailer: .}] | .[0:200])}')"
      ident=unknown; agent=""
      if [[ -n "${CLEARANCE_URL:-}" ]]; then
        out="$(curl --silent --show-error --max-time 20 --retry 2 --retry-connrefused -w '\n%{http_code}' \
                 -H "Authorization: Bearer $(cat /var/run/secrets/clearance/clearance-token)" -H "Content-Type: application/json" \
                 --data "${body}" "${CLEARANCE_URL%/}/v1/provenance" || true)"
        code="${out##*$'\n'}"
        if [[ "${code}" == 200 ]]; then
          verdict="${out%$'\n'*}"
          ident="$(jq -r '.identity.status // "unknown"' <<<"${verdict}")"
          agent="$(jq -r '.record.agent // ""' <<<"${verdict}")"
        fi
      fi
      case "${ident}" in confirmed|not-agent|unattested) ;; *) ident=unknown ;; esac
      prs="$(jq -c --argjson n "${pn}" --arg a "${pauthor}" --arg i "${ident}" --arg g "${agent}" '. + [{number: $n, author: $a, identity: $i, agent: $g}]' <<<"${prs}")"
    done
  done < <(jq -r '.[] | select(.parents < 2) | .sha' <<<"${commits}")
  if [[ -n "${nopr}" ]]; then
    prs="$(jq -c --arg n "${nopr}" '. + [{number: 0, author: "", identity: "unknown", agent: "", note: ("commit(s) " + $n + " have no merged pull request")}]' <<<"${prs}")"
  fi

  # 5. The weakest verdict of the source, in the order unknown < unattested < not-agent < confirmed. `confirmed` (an agent
  #    opened it through Clearance and nothing was added) is claimed only if every source PR is confirmed. One PR with an
  #    AI co-author trailer Clearance has no record of makes the release `unattested`; a release of only PRs with no AI
  #    signal is `not-agent` (a person's change); a commit nobody can vouch for, a forged trailer, a modified agent PR or
  #    an answer Clearance could not give (any verdict that is not one of the three) makes it `unknown`.
  jq -nc --argjson base "${base_info}" --arg mode "${mode}" --arg repo "${sowner}/${srepo}" --arg prev "${prev}" --arg head "${rrev}" \
    --argjson commits "${commits}" --argjson authors "${authors}" --argjson prs "${prs}" '
    def rank: {"unknown": 0, "unattested": 1, "not-agent": 2, "confirmed": 3}[.];
    ($prs | map(.identity) | sort_by(rank) | .[0] // "unknown") as $prov
    | ($prs | map(select(.identity == "confirmed" and .agent != "") | .agent) | unique) as $agents
    | $base + {applicable: true, resolved: true, mode: $mode, sourceRepo: $repo, previousCommit: $prev, releasedCommit: $head,
        commits: ($commits | length), authors: $authors, prs: $prs, provenance: $prov, agents: $agents,
        proposerKind: (if $prov == "confirmed" then "agent" elif $prov == "unknown" then "unknown" else "person" end)}
    | .proposer = (if .proposerKind == "agent" and ($agents | length) > 0 then "autopilot-runs/" + $agents[0] else "user:" + ($authors[0].login // "unknown") end)
    | .agent = (if .proposerKind == "agent" and ($agents | length) > 0 then $agents[0] else null end)'
}
