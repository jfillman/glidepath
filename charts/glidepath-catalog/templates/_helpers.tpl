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
