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
glidepath-catalog.awsLambdaCredentials: bash for a cloud deploy step. Defines and calls aws_credentials, which exports
either a web identity (no stored key) or the app's access key, in that order of preference.

Web identity, when all three hold:
  - the namespace has the ConfigMap glidepath-cloud-identity (glidepath-app renders it once the fleet's clusters.yaml
    names an AWS account), mounted at /var/run/glidepath-cloud-identity;
  - the projected token at /var/run/secrets/aws-web-identity/token (audience sts.amazonaws.com) belongs to the
    ServiceAccount glidepath-cloud-deployer: the deploy stage runs this task as that ServiceAccount
    (taskRunSpecs). A run still on pipeline-runner gets a token AWS would refuse, so it is not used;
  - FUNCTION_NAME is set.
The role is glidepath-deploy-<FUNCTION_NAME cut to 47> in that account: the one Airframe's AwsLambdaTarget creates
for this function (compositions/awslambdatarget/templates/render-target-resources/deploy-role.yaml, same cut).
The AWS CLI exchanges the token for a session itself.

If STS refuses that role (not created yet, or a function no AwsLambdaTarget manages), it says so and falls back.
Otherwise the app-secrets keys, as before.
*/ -}}
{{- define "glidepath-catalog.awsLambdaCredentials" -}}
aws_token_subject() {
  local payload
  payload="$(cut -d. -f2 "$1" | tr '_-' '/+')"
  while (( ${#payload} % 4 )); do payload="${payload}="; done
  base64 -d <<<"${payload}" 2>/dev/null | jq -r '.sub // ""'
}
aws_credentials() {
  local token=/var/run/secrets/aws-web-identity/token
  local account_file=/var/run/glidepath-cloud-identity/awsAccountId
  if [[ -s "${account_file}" && -s "${token}" && -n "${FUNCTION_NAME:-}" ]] \
     && [[ "$(aws_token_subject "${token}")" == system:serviceaccount:*:glidepath-cloud-deployer ]]; then
    export AWS_ROLE_ARN="arn:aws:iam::$(cat "${account_file}"):role/glidepath-deploy-${FUNCTION_NAME:0:47}"
    export AWS_WEB_IDENTITY_TOKEN_FILE="${token}"
    export AWS_ROLE_SESSION_NAME="$(printf 'glidepath-%s' "${TASKRUN_NAME:-deploy}" | cut -c1-64)"
    if aws sts get-caller-identity >/dev/null 2>/tmp/aws-assume-error; then
      echo "credentials: assuming ${AWS_ROLE_ARN} (web identity, no stored key)"
      return 0
    fi
    # The role can lag the switch (the target creates it a reconcile after the registry names the account), or
    # be missing for a function the target doesn't manage. Use the app's key while one exists rather than fail.
    echo "warning: could not assume ${AWS_ROLE_ARN}: $(tail -1 /tmp/aws-assume-error)" >&2
    unset AWS_ROLE_ARN AWS_WEB_IDENTITY_TOKEN_FILE AWS_ROLE_SESSION_NAME
  fi
  if [[ -f /var/run/secrets/platform/aws-access-key-id && -f /var/run/secrets/platform/aws-secret-access-key ]]; then
    export AWS_ACCESS_KEY_ID="$(cat /var/run/secrets/platform/aws-access-key-id)"
    export AWS_SECRET_ACCESS_KEY="$(cat /var/run/secrets/platform/aws-secret-access-key)"
    echo "credentials: access key from app-secrets"
    return 0
  fi
  echo "error: no AWS credentials for this deploy: neither a glidepath-cloud-deployer web identity (glidepath-cloud-identity ConfigMap + the deploy stage's ServiceAccount) nor aws-access-key-id/aws-secret-access-key in app-secrets - see docs/admin/app-secrets.md." >&2
  return 1
}
aws_credentials
{{- end -}}
