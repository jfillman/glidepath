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

{{/*
Cluster taxonomy (ADR-0024). clusterZone/clusterRoles take one cluster-registry entry and return its validated zone
(default upper) and roles as a JSON array (default ["workloads"]).
*/}}
{{- define "glidepath-control-plane.clusterZone" -}}
{{- $zone := .zone | default "upper" -}}
{{- if not (has $zone (list "lower" "upper")) -}}
{{- fail (printf "clusters: '%s' has zone '%v', expected lower or upper (ADR-0024)" .name $zone) -}}
{{- end -}}
{{- $zone -}}
{{- end -}}

{{- define "glidepath-control-plane.clusterRoles" -}}
{{- $roles := .roles | default (list "workloads") -}}
{{- range $roles -}}
{{- if not (has . (list "workloads" "platform-services")) -}}
{{- fail (printf "clusters: '%s' has role '%v', expected workloads or platform-services (control-plane is the cluster running this chart; ADR-0024)" $.name .) -}}
{{- end -}}
{{- end -}}
{{- $roles | toJson -}}
{{- end -}}

{{/*
The taxonomy forwarded to every app's glidepath-app, for its environment checks: the control plane's own name and
every known cluster's zone. Validates this cluster's own clusterZone/clusterRoles too.
*/}}
{{- define "glidepath-control-plane.clusterTaxonomy" -}}
{{- $self := required "clusterName must be set" .Values.clusterName -}}
{{- if ne (.Values.clusterZone | default "lower") "lower" -}}
{{- fail (printf "clusterZone: the control-plane cluster '%s' must be zone lower (its pipelines deploy to it directly; ADR-0024)" $self) -}}
{{- end -}}
{{- if not (has "control-plane" (.Values.clusterRoles | default (list "control-plane"))) -}}
{{- fail "clusterRoles must include control-plane: the cluster running glidepath-control-plane is the control plane (ADR-0024)" -}}
{{- end -}}
{{- $zones := dict $self "lower" -}}
{{- range .Values.clusters -}}
{{- if eq .name $self -}}
{{- fail (printf "clusters: '%s' is this control plane's own cluster; it is described by clusterZone/clusterRoles, not registered (ADR-0024)" .name) -}}
{{- end -}}
{{- $_ := set $zones .name (include "glidepath-control-plane.clusterZone" .) -}}
{{- end -}}
{{- dict "self" $self "zones" $zones | toYaml -}}
{{- end -}}
