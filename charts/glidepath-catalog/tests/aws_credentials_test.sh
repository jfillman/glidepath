#!/usr/bin/env bash
# Runs glidepath-catalog.awsLambdaCredentials (the AWS login of deploy-lambda's steps), as rendered into
# deploy-lambda.yaml, against a stub aws CLI: web identity only, and each missing piece fails with its own message.
# The app-secrets key was removed as a fallback in phase 4 of the deploy federation (docs/admin/cloud-deploy-identity.md),
# so a key in the mounted secrets must not be used. Needs helm, python3 (pyyaml) and base64.
# Usage: charts/glidepath-catalog/tests/aws_credentials_test.sh
set -u
cd "$(dirname "$0")/../../.."
fail=0
work="$(mktemp -d)"

# The update-function step's script, cut down to the helper: from aws_token_subject() to its call.
helm template t charts/glidepath-catalog --show-only templates/tasks/deploy-lambda.yaml \
  | python3 -c "
import sys,yaml
t=yaml.safe_load(sys.stdin.read())
s=[s for s in t['spec']['steps'] if s['name']=='update-function'][0]['script']
start=s.index('aws_token_subject() {')
end=s.index('\naws_credentials\n', start)+len('\naws_credentials\n')
print(s[start:end])" > "${work}/helper.sh"
grep -q 'aws_credentials' "${work}/helper.sh" || { echo "FAIL: could not extract the helper"; exit 1; }
# Point its fixed paths into the sandbox.
sed -i.bak -e "s#/var/run/secrets/aws-web-identity#${work}/wi#g" \
  -e "s#/var/run/glidepath-cloud-identity#${work}/ci#g" -e "s#/tmp/aws-assume-error#${work}/err#g" "${work}/helper.sh"

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
# case: label, want exit (0/1), want text in output, account, token sub ('' = no token), function, sts-fail
check() {
  rm -rf "${work}/wi" "${work}/ci"; mkdir -p "${work}/wi" "${work}/ci"
  [[ -n "$4" ]] && printf '%s' "$4" > "${work}/ci/awsAccountId"
  [[ -n "$5" ]] && token "$5" > "${work}/wi/token"
  out="$(PATH="${work}/bin:${PATH}" FUNCTION_NAME="$6" STS_FAIL="$7" TASKRUN_NAME=tr-1 \
    AWS_ACCESS_KEY_ID= bash -c "set -euo pipefail; source '${work}/helper.sh'; echo ARN=\${AWS_ROLE_ARN}" 2>&1)"
  rc=$?; [[ $rc -ne 0 ]] && rc=1
  if [[ $rc -eq $2 && "$out" == *"$3"* ]]; then echo "ok   $1"; else echo "FAIL $1: rc=$rc out=$out"; fail=1; fi
}
SA=system:serviceaccount:app-smoke-fn-cicd:glidepath-cloud-deployer
LONG=glidepath-smoke-fn-with-a-very-long-name-that-goes-past-the-limit
check "web identity: assumes the function's role" 0 "ARN=arn:aws:iam::700017004810:role/glidepath-deploy-glidepath-smoke-fn" 700017004810 "$SA" glidepath-smoke-fn 0
check "web identity: logs that no key is stored" 0 "(web identity, no stored key)" 700017004810 "$SA" glidepath-smoke-fn 0
check "role name cut to 47 characters" 0 "role/glidepath-deploy-${LONG:0:47} (web" 700017004810 "$SA" "$LONG" 0
check "no account ConfigMap: fails, names clusters.yaml" 1 "aws.accountId" "" "$SA" glidepath-smoke-fn 0
check "runs as pipeline-runner: fails, names taskRunSpecs" 1 "taskRunSpecs" 700017004810 system:serviceaccount:app-smoke-fn-cicd:pipeline-runner glidepath-smoke-fn 0
check "no token at all: fails" 1 "not running as glidepath-cloud-deployer" 700017004810 "" glidepath-smoke-fn 0
check "STS refuses the role: fails, names the target's Role" 1 "-deploy Role is Ready" 700017004810 "$SA" glidepath-smoke-fn 1
check "no function name: fails" 1 "no function name" 700017004810 "$SA" "" 0

# A stored key in the mounted secrets is never read: the helper has no such path any more.
if grep -q 'aws-access-key-id\|AWS_ACCESS_KEY_ID' "${work}/helper.sh"; then
  echo "FAIL the helper still reads a stored key"; fail=1
else
  echo "ok   the helper never reads a stored key"
fi
# deploy-lambda mounts no app-secrets.
if helm template t charts/glidepath-catalog --show-only templates/tasks/deploy-lambda.yaml | grep -q 'secretName: app-secrets'; then
  echo "FAIL deploy-lambda still mounts app-secrets"; fail=1
else
  echo "ok   deploy-lambda mounts no app-secrets"
fi
exit $fail
