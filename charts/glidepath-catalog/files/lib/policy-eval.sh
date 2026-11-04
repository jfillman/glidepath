# policy-eval.sh: the offline admission evaluation (docs/admin/policy-validation.md, ADR-0028), shared by the
# policy-validation-gate and clearance-gate Tasks so both evaluate a release exactly the same way
# (glidepath-catalog.policyEvalLib splices it into a step script). The render step's policy-eval-prepare.py writes the
# inputs these read.
#
#   policy_bundle_fetch <cluster> <owner>      the broker's /policy-bundle for one target cluster: its admission
#                                              policies into /work/policies/<cluster>/bundle.yaml and where they came
#                                              from (repoURL, path, revision, commit, files) into
#                                              /work/policies/<cluster>.source.json. Non-zero, with the reason on
#                                              stderr, when the bundle cannot be read. Needs POLICY_BUNDLE_URL and the
#                                              policy-bundle-audience token at /var/run/secrets/policy-bundle.
#   policy_eval_env <cluster> <env> <file>     `kyverno apply` of the cluster's normalized bundle to
#                                              /work/eval/<cluster>__<env>/resources.yaml; every non-passing result is
#                                              appended to <file> as a JSON line {env, policy, rule, result, level
#                                              (deny | warn | broken), resource, message}. 0 evaluated, 2 nothing to
#                                              evaluate (the cluster has no admission policy), 1 Kyverno could not
#                                              evaluate (a `broken` line is appended).
#   kyverno_engine                             "kyverno/<version>" of the CLI on PATH; nothing when it cannot say.
#   policy_bundle_ref <cluster>                the rule set a verdict was reached against, "<repoURL>//<path>@<commit>"
#                                              (the revision when the broker named no commit); nothing without a bundle.

policy_bundle_fetch() {
  local cluster="$1" owner="$2" resp code body
  resp="$(curl --silent --show-error --max-time 60 --retry 3 --retry-connrefused -w '\n%{http_code}' -X POST "${POLICY_BUNDLE_URL}" \
    -H "Authorization: Bearer $(cat /var/run/secrets/policy-bundle/policy-bundle-token)" -H "Content-Type: application/json" \
    -d "$(jq -nc --arg c "${cluster}" --arg o "${owner}" '{cluster: $c, owner: $o}')")" \
    || { echo "could not read ${cluster}'s admission policies: the broker did not answer" >&2; return 1; }
  code="${resp##*$'\n'}"; body="${resp%$'\n'*}"
  if [[ "${code}" != 200 ]]; then
    echo "could not read ${cluster}'s admission policies: HTTP ${code}: $(jq -r '.error // .' <<<"${body}" 2>/dev/null || echo "${body}")" >&2
    return 1
  fi
  mkdir -p "/work/policies/${cluster}"
  jq -c '{repoURL, path, revision, commit, files}' <<<"${body}" > "/work/policies/${cluster}.source.json"
  # One JSON document per YAML document (JSON is YAML), so the render step reads it like any policy file.
  jq -r '.documents[] | tojson, "---"' <<<"${body}" > "/work/policies/${cluster}/bundle.yaml"
  echo "policies for ${cluster}: $(jq -r '"\(.repoURL) \(.path) @ \(.revision) (\(.commit[0:12])): \(.documents | length) document(s) in \(.files) file(s)"' <<<"${body}")"
}

policy_eval_env() {
  local cluster="$1" env="$2" findings="$3" out bundle krc
  out="/work/eval/${cluster}__${env}"
  bundle="/work/policies/${cluster}.bundle.yaml"
  if [[ ! -s "${bundle}" ]] || [[ "$(jq 'length' "/work/policies/${cluster}.index.json")" == 0 ]]; then
    echo "${cluster}/${env}: ${cluster} has no admission policy to evaluate"
    return 2
  fi
  # kyverno exits 1 when a rule fails, so its exit code is not the signal: a report that parses is.
  kyverno apply "${bundle}" --resource "${out}/resources.yaml" --crd-paths "${out}/crds.yaml" \
    -f "${out}/values.yaml" --policy-report --output-format json --remove-color \
    > "${out}/report.json" 2> "${out}/kyverno.err"
  krc=$?
  if [[ ! -s "${out}/report.json" && "${krc}" == 0 ]]; then
    echo '{"results":[]}' > "${out}/report.json"  # every policy skipped: no report at all
  fi
  if ! jq -e '.' "${out}/report.json" >/dev/null 2>&1; then
    echo "FAIL ${cluster}/${env}: kyverno could not evaluate the policies (exit ${krc})"; tail -20 "${out}/kyverno.err"
    echo "{\"env\":\"${cluster}/${env}\",\"level\":\"broken\",\"message\":\"kyverno could not evaluate (exit ${krc}): $(tail -1 "${out}/kyverno.err" | tr -d '"\\')\"}" >> "${findings}"
    return 1
  fi
  # Every non-passing result, with the policy's source and mode. fail under a deny policy = deny; any other
  # fail, warn or error = warn (an error is a rule offline evaluation could not decide).
  jq -c --arg e "${cluster}/${env}" --slurpfile idx "/work/policies/${cluster}.index.json" '
    .results[]? | select(.result == "fail" or .result == "warn" or .result == "error")
    | ($idx[0][.policy] // {source: .policy, mode: "warn"}) as $p
    | {env: $e, policy: $p.source, rule: (.rule // ""), result,
       level: (if .result == "fail" and $p.mode == "deny" then "deny" else "warn" end),
       resource: ((.resources // [])[0] | "\(.kind // "?")/\(.name // "?")"),
       message: ((.message // "") | gsub("\\s+"; " "))}' "${out}/report.json" >> "${findings}"
  echo "${cluster}/${env}: $(jq -c '.summary // {}' "${out}/report.json")"
}

kyverno_engine() {
  local v
  v="$(kyverno version 2>/dev/null | sed -nE 's/^Version: *v?([0-9][0-9A-Za-z.+-]*).*$/\1/p' | head -1)"
  [[ -n "${v}" ]] && echo "kyverno/${v}"
  return 0
}

policy_bundle_ref() {
  local src="/work/policies/$1.source.json"
  [[ -f "${src}" ]] || return 0
  jq -r 'if (.repoURL // "") == "" then empty else "\(.repoURL)//\(.path // "")@\(.commit // .revision // "")" end' "${src}"
}
