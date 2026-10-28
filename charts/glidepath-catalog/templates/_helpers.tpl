{{/*
glidepath-catalog.labels - the standard Kubernetes-recommended label set
(app.kubernetes.io/*, for generic tooling interop: kubectl, Lens, ArgoCD's own resource
tree, etc.) plus this platform's own hangar.io/component marker. Every resource in
this chart includes this via `{{- include "glidepath-catalog.labels" . | nindent 4 }}`
under its own metadata.labels, then adds any resource-specific labels
(hangar.io/catalog, hangar.io/stub) alongside it - see docs/naming-conventions.md.
*/}}
{{- define "glidepath-catalog.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: glidepath
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
hangar.io/component: catalog
{{- end -}}

{{- /*
glidepath-catalog.awsCredentials: bash for a cloud deploy step. Defines aws_credentials ROLE_NAME, which exports a web
identity for the IAM role ROLE_NAME: there is no stored key. Since 2026-10-16 (phase 4 of the deploy federation) the
app-secrets access key is no longer a fallback for Lambda, and ECS never had one under web identity.

The caller names the role, the one Airframe's target created for what it deploys:
  - deploy-lambda: aws_credentials "glidepath-deploy-${FUNCTION_NAME:0:47}"   (AwsLambdaTarget deploy-role.yaml)
  - deploy-ecs:    aws_credentials "glidepath-deploy-ecs-${SERVICE:0:43}"     (AwsEcsTarget deploy-role.yaml)
(the same cuts as the compositions: IAM role names stop at 64 characters).

It needs:
  - the namespace has the ConfigMap glidepath-cloud-identity (glidepath-app renders it once the fleet's clusters.yaml
    names an AWS account), mounted at /var/run/glidepath-cloud-identity;
  - the projected token at /var/run/secrets/aws-web-identity/token (audience sts.amazonaws.com) belongs to the
    ServiceAccount glidepath-cloud-deployer: the deploy stage runs this task as that ServiceAccount
    (taskRunSpecs). A run still on pipeline-runner gets a token AWS would refuse, so it is not used;
  - a role name.
The AWS CLI exchanges the token for a session itself.

If one is missing, or STS refuses the role (not created yet, or nothing the platform's targets manage), the step fails
and says which, with the fix.
*/ -}}
{{- /*
glidepath-catalog.releaseImagePin - bash function release_image_pin MANIFEST SOURCE_REPO DIGEST (ADR-0029). With
promotion.pinDigest on, writes release.image.digest and release.image.source; off, deletes them, so a release file
never keeps a digest from an earlier release under a newer tag. DIGEST must be sha256:<64 hex> when pinning.
*/ -}}
{{- define "glidepath-catalog.releaseImagePin" -}}
release_image_pin() {
  local manifest="$1" source_repo="$2" digest="$3"
{{- if .Values.promotion.pinDigest }}
  if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "error: promotion.pinDigest is on but the image digest is '${digest}', not sha256:<hex> - resolve-image-ref could not read it from the registry" >&2
    return 1
  fi
  yq -i -e ".release.image.digest = \"${digest}\" | .release.image.source = \"${source_repo}\"" "${manifest}"
{{- else }}
  yq -i 'del(.release.image.digest, .release.image.source)' "${manifest}"
{{- end }}
}
{{- end -}}

{{- define "glidepath-catalog.awsCredentials" -}}
aws_token_subject() {
  local payload
  payload="$(cut -d. -f2 "$1" | tr '_-' '/+')"
  while (( ${#payload} % 4 )); do payload="${payload}="; done
  base64 -d <<<"${payload}" 2>/dev/null | jq -r '.sub // ""'
}
aws_credentials() {
  local role_name="${1:-}"
  local token=/var/run/secrets/aws-web-identity/token
  local account_file=/var/run/glidepath-cloud-identity/awsAccountId
  local doc="see docs/admin/cloud-deploy-identity.md"
  if [[ ! -s "${account_file}" ]]; then
    echo "error: no AWS account for cloud deploys in this namespace (ConfigMap glidepath-cloud-identity): the cluster's clusters.yaml entry needs aws.accountId - ${doc}." >&2
    return 1
  fi
  if [[ ! -s "${token}" ]] || [[ "$(aws_token_subject "${token}")" != system:serviceaccount:*:glidepath-cloud-deployer ]]; then
    echo "error: this step is not running as glidepath-cloud-deployer, the only identity AWS trusts for a deploy: the run's taskRunSpecs must give it to this task (a promote flow picks this up at the next onboarding resync) - ${doc}." >&2
    return 1
  fi
  if [[ -z "${role_name}" || "${role_name}" == "glidepath-deploy-" || "${role_name}" == "glidepath-deploy-ecs-" ]]; then
    echo "error: no deploy role name (empty function or service name) - ${doc}." >&2
    return 1
  fi
  export AWS_ROLE_ARN="arn:aws:iam::$(cat "${account_file}"):role/${role_name}"
  export AWS_WEB_IDENTITY_TOKEN_FILE="${token}"
  export AWS_ROLE_SESSION_NAME="$(printf 'glidepath-%s' "${TASKRUN_NAME:-deploy}" | cut -c1-64)"
  if ! aws sts get-caller-identity >/dev/null 2>/tmp/aws-assume-error; then
    echo "error: could not assume ${AWS_ROLE_ARN}: $(tail -1 /tmp/aws-assume-error). The cloud target (AwsLambdaTarget / AwsEcsTarget) creates this role; check its <target>-deploy Role is Ready - ${doc}." >&2
    return 1
  fi
  echo "credentials: assuming ${AWS_ROLE_ARN} (web identity, no stored key)"
}
{{- end -}}

{{- /*
glidepath-catalog.guardrails: the effective release-guardrail registry as a JSON list - .Values.releaseGuardrails
with each entry's `required` resolved (default true; .Values.releaseGuardrailOverrides.<name>.required wins; a stub
is always "required" because its check is always green and the field means nothing to it). sast and provenance
refuse required: false here, so no cluster can render a catalog that advises on the image's integrity. The itsm
entry's status follows .Values.itsm.provider (stub while none: the gate is then the loud stub whatever the registry
says). Every
consumer of the registry ranges over this (fromJsonArray), never over .Values.releaseGuardrails directly, so the
override map and the refusal apply everywhere at once (docs/admin/release-guardrails.md).
*/ -}}
{{- define "glidepath-catalog.guardrails" -}}
{{- $out := list -}}
{{- $overrides := .Values.releaseGuardrailOverrides | default dict -}}
{{- range .Values.releaseGuardrails -}}
{{- $g := deepCopy . -}}
{{- $required := true -}}
{{- if hasKey $g "required" }}{{ $required = $g.required }}{{ end -}}
{{- $o := index $overrides $g.name -}}
{{- if and $o (kindIs "map" $o) (hasKey $o "required") }}{{ $required = index $o "required" }}{{ end -}}
{{- if and (eq $g.name "itsm") (hasKey $.Values "itsm") }}{{ $_ := set $g "status" (ternary "stub" "real" (eq ($.Values.itsm.provider | default "none") "none")) }}{{ end -}}
{{- if eq ($g.status | default "real") "stub" }}{{ $required = true }}{{ end -}}
{{- if and (not $required) (has $g.name (list "sast" "provenance")) -}}
{{- fail (printf "releaseGuardrails: %s cannot be required: false - it verifies the image's integrity and must enforce, not advise (docs/admin/release-guardrails.md)" $g.name) -}}
{{- end -}}
{{- $_ := set $g "required" $required -}}
{{- $out = append $out $g -}}
{{- end -}}
{{- /* ADR-0029 phase 3: with promotion on, the promote pipeline's check is a required gate. It is not a
releaseGuardrails entry because nothing delivers a .tekton file for it (the fan-in starts it), and with promotion
off no PR would ever get the check and every merge would read as a bypass. */ -}}
{{- if .Values.promotion.enabled -}}
{{- $out = append $out (dict "name" "image-promotion" "description" "The release image copied by digest to the Flight registry with its signatures and attestations, and a promotion attestation for this release" "status" "real" "class" "integrity" "required" true "targets" (list "k8s-rollout")) -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{- /*
glidepath-catalog.gateRequired: "true" or "false" for one gate, from the effective registry above.
Usage: {{ $required := eq (include "glidepath-catalog.gateRequired" (dict "root" . "gate" "qa")) "true" }}
A gate missing from the registry is "true" (nothing has made it advisory).
*/ -}}
{{- define "glidepath-catalog.gateRequired" -}}
{{- $required := true -}}
{{- range (include "glidepath-catalog.guardrails" .root | fromJsonArray) -}}
{{- if eq .name $.gate }}{{ $required = .required }}{{ end -}}
{{- end -}}
{{- ternary "true" "false" $required -}}
{{- end -}}

{{- /*
glidepath-catalog.itsmEnabled: "true" when a change-management provider is configured (.Values.itsm.provider is
servicenow or webhook), else "false". The lifecycle tasks (open-release-pr's change creation, bypass-merge-check's
itsm-implement, the outcome pipelines' itsm-close) and the real itsm gate render only when true.
*/ -}}
{{- define "glidepath-catalog.itsmEnabled" -}}
{{- $p := .Values.itsm.provider | default "none" -}}
{{- if or (eq $p "servicenow") (eq $p "webhook") }}true{{ else }}false{{ end -}}
{{- end -}}

{{- /* glidepath-catalog.itsmEnv: the env entries files/lib/itsm.sh reads, for a step's `env:` list.
The two URLs are emitted only when set: an env entry with `value: ""` is stored by the API server without its
value (omitempty), so Argo CD diffs it against the chart forever and the Task never leaves OutOfSync (seen live
2026-10-20 on hangar-dev: itsm-gate, itsm-change and open-release-pr, ITSM_SERVICENOW_URL under the webhook
provider). itsm.sh defaults an absent URL to empty, so the scripts read the same thing either way. */ -}}
{{- define "glidepath-catalog.itsmEnv" -}}
- name: ITSM_PROVIDER
  value: {{ .Values.itsm.provider | default "none" | quote }}
{{- with (.Values.itsm.servicenow.instanceUrl | default "" | toString) }}
- name: ITSM_SERVICENOW_URL
  value: {{ . | quote }}
{{- end }}
- name: ITSM_SERVICENOW_APPROVED_STATES
  value: {{ join "," (.Values.itsm.servicenow.approvedStates | default (list "-2" "-1")) | quote }}
{{- with (.Values.itsm.webhook.url | default "" | toString) }}
- name: ITSM_WEBHOOK_URL
  value: {{ . | quote }}
{{- end }}
- name: ITSM_CREDENTIALS_DIR
  value: /var/run/secrets/itsm
{{- end -}}

{{- /* glidepath-catalog.itsmVolume / itsmVolumeMount: the optional credentials Secret, for a Task's volumes and a step's volumeMounts. */ -}}
{{- define "glidepath-catalog.itsmVolume" -}}
- name: itsm-credentials
  secret:
    secretName: {{ .Values.itsm.credentialsSecret | default "itsm-credentials" }}
    optional: true
{{- end -}}
{{- define "glidepath-catalog.itsmVolumeMount" -}}
- name: itsm-credentials
  mountPath: /var/run/secrets/itsm
  readOnly: true
{{- end -}}

{{- /* glidepath-catalog.itsmLib: files/lib/itsm.sh, to splice into a step script after its `source` lines. */ -}}
{{- define "glidepath-catalog.itsmLib" -}}
{{ .Files.Get "files/lib/itsm.sh" }}
{{- end -}}

{{/*
Bash for the promote Task's steps (ADR-0029): release_pr_token OWNER REPO sets $token to a release-pr token from the
broker's /promoter-token (checks + pull_requests: write, contents: read on that gitops repo); gh_ calls the GitHub API
with it; check_run_start OWNER REPO SHA opens the image-promotion check run and writes its id to <work>/check-id.
*/}}
{{- define "glidepath-catalog.promoterGitHub" -}}
token=""
release_pr_token() {
  local raw
  raw="$(curl --silent --show-error --max-time 20 --retry 3 --retry-connrefused -w '\n%{http_code}' -X POST "${PROMOTER_TOKEN_URL}" \
    -H "Authorization: Bearer $(cat "${PROMOTER_TOKEN_FILE:-/var/run/secrets/promoter/token}")" -H "Content-Type: application/json" \
    -d "$(jq -n --arg o "$1" --arg r "$2" '{purpose: "release-pr", owner: $o, repo: $r}')")" || return 1
  [[ "${raw##*$'\n'}" == "200" ]] || { echo "promoter-token: ${raw}" >&2; return 1; }
  token="$(jq -r .token <<<"${raw%$'\n'*}")"
}
gh_() {
  curl --fail-with-body --silent --show-error --max-time 20 --retry 3 --retry-connrefused \
    -H "Authorization: Bearer ${token}" -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" "$@"
}
check_run_start() {
  local out
  out="$(gh_ -X POST "https://api.github.com/repos/$1/$2/check-runs" \
    -d "$(jq -n --arg sha "$3" --arg id "${RUN_NAME}" \
      '{name: "Pipelines as Code CI / image-promotion-", head_sha: $sha, external_id: $id, status: "in_progress",
        output: {title: "Promoting to Flight", summary: "Copying the release image to Flight and verifying it."}}')")" || return 1
  jq -r .id <<<"${out}" > "${W}/check-id"
}
{{- end -}}
