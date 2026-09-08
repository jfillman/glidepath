{{/*
glidepath-control-plane.labels - shared label set for every resource in this chart,
mirroring charts/glidepath-catalog/templates/_helpers.tpl's own helper. Callers add
hangar.io/subcomponent: <broker|dora-exporter|sigstore|secretstore|hooks> alongside it -
see docs/admin/naming-conventions.md.
*/}}
{{- define "glidepath-control-plane.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: glidepath
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
hangar.io/component: control-plane
{{- end -}}

{{/*
glidepath-control-plane.githubOwner - the GitHub org/user this deployment's repos
live under, derived from the already-required platformCicdRepoUrl (not a second
hardcoded value to keep in sync) - used to derive tenantsRepoUrl and to scope
onboarding-appproject.yaml's sourceRepos allowlist, so neither hardcodes a specific
org/user and this chart stays installable by anyone, not just this repo's own owner.
Expects the standard https://github.com/<owner>/<repo>[.git] shape.
*/}}
{{- define "glidepath-control-plane.githubOwner" -}}
{{- regexReplaceAll "^https://github\\.com/([^/]+)/.*$" (required "platformCicdRepoUrl must be set" .Values.platformCicdRepoUrl) "${1}" -}}
{{- end -}}
