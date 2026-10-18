#!/usr/bin/env bash
# Runs glidepath-catalog.awsCredentials (the AWS login of the cloud deploy steps), as rendered into deploy-lambda.yaml
# and deploy-ecs.yaml together with each step's own call line, against a stub aws CLI: web identity only, the role
# each Task names, and each missing piece fails with its own message. No step may read a stored key: Lambda's
# app-secrets fallback was removed in phase 4 of the deploy federation, and ECS moved to web identity after it
# (docs/admin/cloud-deploy-identity.md). Needs helm, python3 (pyyaml) and base64.
# Usage: charts/glidepath-catalog/tests/aws_credentials_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"

# A step's script cut down to the helper and its call: from aws_token_subject() to the `aws_credentials "..."` line.
extract() { # task-file step-name out
  helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only "templates/tasks/$1" \
    | python3 -c "
import sys,yaml
t=yaml.safe_load(sys.stdin.read())
s=[s for s in t['spec']['steps'] if s['name']=='$2'][0]['script']
start=s.index('aws_token_subject() {')
call=s.index('\naws_credentials \"', start)
end=s.index('\n', call+1)+1
print(s[start:end])" > "$3"
  grep -q '^aws_credentials "' "$3" || { echo "FAIL: could not extract the helper and call from $1/$2"; exit 1; }
  # Point its fixed paths into the sandbox.
  sed -i.bak -e "s#/var/run/secrets/aws-web-identity#${work}/wi#g" \
    -e "s#/var/run/glidepath-cloud-identity#${work}/ci#g" -e "s#/tmp/aws-assume-error#${work}/err#g" "$3"
}
extract deploy-lambda.yaml prepare "${work}/lambda-prepare.sh"
extract deploy-lambda.yaml update-function "${work}/lambda-update.sh"
extract deploy-ecs.yaml prepare "${work}/ecs-prepare.sh"
extract deploy-ecs.yaml update-service "${work}/ecs-update.sh"

mkdir -p "${work}/bin"
cat > "${work}/bin/aws" <<'STUB'
#!/usr/bin/env bash
# sts get-caller-identity: succeeds only for a web identity, never for a stored key.
[[ -n "${AWS_ACCESS_KEY_ID:-}" ]] && { echo "stub: a stored key was used" >&2; exit 3; }
[[ "${STS_FAIL:-0}" == 1 ]] && { echo "An error occurred (AccessDenied) when calling the AssumeRoleWithWebIdentity operation: Not authorized" >&2; exit 254; }
[[ -n "${AWS_ROLE_ARN:-}" && -s "${AWS_WEB_IDENTITY_TOKEN_FILE:-}" ]] || { echo "stub: no credentials" >&2; exit 255; }
echo '{}'
STUB
chmod +x "${work}/bin/aws"

token() { # a JWT whose payload has this sub
  local p; p="$(printf '{"sub":"%s","aud":["sts.amazonaws.com"]}' "$1" | base64 | tr -d '=\n' | tr '/+' '_-')"
  printf 'eyJhbGciOiJSUzI1NiJ9.%s.sig' "$p"
}
# check label script want-rc want-text account token-sub function service sts-fail
check() {
  rm -rf "${work}/wi" "${work}/ci"; mkdir -p "${work}/wi" "${work}/ci"
  [[ -n "$4" ]] && printf '%s' "$4" > "${work}/ci/awsAccountId"
  [[ -n "$5" ]] && token "$5" > "${work}/wi/token"
  out="$(PATH="${work}/bin:${PATH}" FUNCTION_NAME="$6" SERVICE="$7" STS_FAIL="$8" TASKRUN_NAME=tr-1 \
    AWS_ACCESS_KEY_ID= bash -c "set -euo pipefail; source '${work}/$2'; echo ARN=\${AWS_ROLE_ARN}" 2>&1)"
  rc=$?; [[ $rc -ne 0 ]] && rc=1
  if [[ $rc -eq $3 && "$out" == *"$9"* ]]; then echo "ok   $1"; else echo "FAIL $1: rc=$rc out=$out"; fail=1; fi
}
SA=system:serviceaccount:app-smoke-fn-cicd:glidepath-cloud-deployer
RUNNER=system:serviceaccount:app-smoke-fn-cicd:pipeline-runner
LONG=glidepath-smoke-fn-with-a-very-long-name-that-goes-past-the-limit
A=700017004810
for step in lambda-prepare lambda-update; do
  check "$step: assumes the function's role" $step.sh 0 "$A" "$SA" glidepath-smoke-fn "" 0 "ARN=arn:aws:iam::$A:role/glidepath-deploy-glidepath-smoke-fn"
  check "$step: logs that no key is stored" $step.sh 0 "$A" "$SA" glidepath-smoke-fn "" 0 "(web identity, no stored key)"
  check "$step: role name cut to 47 characters" $step.sh 0 "$A" "$SA" "$LONG" "" 0 "role/glidepath-deploy-${LONG:0:47} (web"
  check "$step: no function name fails" $step.sh 1 "$A" "$SA" "" "" 0 "no deploy role name"
done
for step in ecs-prepare ecs-update; do
  check "$step: assumes the service's role" $step.sh 0 "$A" "$SA" "" smoke-ecs-dev 0 "ARN=arn:aws:iam::$A:role/glidepath-deploy-ecs-smoke-ecs-dev"
  check "$step: role name cut to 43 characters" $step.sh 0 "$A" "$SA" "" "$LONG" 0 "role/glidepath-deploy-ecs-${LONG:0:43} (web"
  check "$step: no service name fails" $step.sh 1 "$A" "$SA" "" "" 0 "no deploy role name"
done
check "no account ConfigMap: fails, names clusters.yaml" ecs-prepare.sh 1 "" "$SA" "" smoke-ecs-dev 0 "aws.accountId"
check "runs as pipeline-runner: fails, names taskRunSpecs" ecs-prepare.sh 1 "$A" "$RUNNER" "" smoke-ecs-dev 0 "taskRunSpecs"
check "no token at all: fails" lambda-prepare.sh 1 "$A" "" glidepath-smoke-fn "" 0 "not running as glidepath-cloud-deployer"
check "STS refuses the role: fails, names the target's Role" ecs-update.sh 1 "$A" "$SA" "" smoke-ecs-dev 1 "-deploy Role is Ready"

# No cloud deploy Task reads a stored key or mounts app-secrets.
for task in deploy-lambda deploy-ecs; do
  rendered="$(helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml --show-only templates/tasks/$task.yaml)"
  scripts="$(python3 -c "import sys,yaml; print('\n'.join(s.get('script','') for s in yaml.safe_load(sys.stdin.read())['spec']['steps']))" <<<"${rendered}")"
  [[ -n "${scripts}" ]] || { echo "FAIL $task: no step scripts extracted"; fail=1; }
  if grep -q 'aws-access-key-id\|AWS_ACCESS_KEY_ID\|AWS_SECRET_ACCESS_KEY' <<<"${scripts}"; then
    echo "FAIL $task still reads a stored key"; fail=1
  else
    echo "ok   $task never reads a stored key"
  fi
  if grep -q 'secretName: app-secrets' <<<"${rendered}"; then
    echo "FAIL $task still mounts app-secrets"; fail=1
  else
    echo "ok   $task mounts no app-secrets"
  fi
done
exit $fail
