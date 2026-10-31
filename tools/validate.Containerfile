# The image that runs `glidepath validate` (docs/admin/contract.md) where there is no Python: the cicd-validate check
# on every app repo's pull requests (charts/glidepath-catalog/templates/tasks/cicd-validate.yaml), and later the API's
# validation endpoint. The toolbox image stays bash-only. Same shape as Airframe's tools/validate.Containerfile.
# Build context is the repo root:
#   container build -f tools/validate.Containerfile -t glidepath-validate:dev .
# Usage: glidepath-validate FILE [--format json] [--agent --base OLD_FILE]   (exit 0 = valid, 1 = a problem, 2 = usage)
# No git in the image: --base-ref needs one, so callers hand it the base file (--base).
FROM python:3.13-alpine
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates \
 && pip install --no-cache-dir pyyaml jsonschema \
 && pip uninstall -y setuptools pip
# The validator, the scope rules, the schema and the contract of the commit this image is tagged with: the validator
# finds them relative to itself (ROOT = /opt/glidepath), so a check always runs the rules the contract states.
COPY tools/glidepath-validate tools/cicd_scope.py /opt/glidepath/tools/
COPY schemas/cicd.schema.json /opt/glidepath/schemas/cicd.schema.json
COPY contract/glidepath-contract.json /opt/glidepath/contract/glidepath-contract.json
RUN ln -s /opt/glidepath/tools/glidepath-validate /usr/local/bin/glidepath-validate && adduser -D -u 10001 validator
USER 10001
ENTRYPOINT ["/opt/glidepath/tools/glidepath-validate"]
