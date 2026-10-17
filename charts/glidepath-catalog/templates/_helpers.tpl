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
