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
Cluster records (ADR-0024, amended 2026-10-08). `clusters` is the fleet's clusters.yaml - one record per cluster,
shared with Airframe's cluster-registry chart - passed as a value file. A record may describe this control plane's own
cluster (name == clusterName, roles include control-plane); every other record is a remote cluster, which needs a relay
secret: glidepath.relaySecretName (or the older top-level relaySecretName). Keys this chart does not know (the
airframe: section, aliases, tenantsRepo) are ignored.

clusterZone/clusterRoles take one remote record and return its validated zone (default upper) and roles as a JSON
array (default ["workloads"]).
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
{{- fail (printf "clusters: '%s' has role '%v', expected workloads or platform-services (control-plane belongs only to the record named clusterName; ADR-0024)" $.name .) -}}
{{- end -}}
{{- end -}}
{{- $roles | toJson -}}
{{- end -}}

{{/* Every remote cluster, normalized: {"items": [{name, relaySecretName, zone, roles}]}. */}}
{{- define "glidepath-control-plane.remoteClusters" -}}
{{- $self := .Values.clusterName -}}
{{- $out := list -}}
{{- range .Values.clusters -}}
{{- if ne .name $self -}}
{{- $relay := (.glidepath | default dict).relaySecretName | default .relaySecretName -}}
{{- $_ := required (printf "clusters: '%s' needs glidepath.relaySecretName: the Secret holding the token its notifications present to the relay" .name) $relay -}}
{{- $out = append $out (dict "name" .name "relaySecretName" $relay "zone" (include "glidepath-control-plane.clusterZone" .) "roles" (include "glidepath-control-plane.clusterRoles" . | fromJsonArray)) -}}
{{- end -}}
{{- end -}}
{{- dict "items" $out | toJson -}}
{{- end -}}

{{/*
This control plane's own cluster: its record in `clusters` when there is one, else clusterZone/clusterRoles.
Validated: zone lower (its pipelines deploy to it directly), roles include control-plane.
*/}}
{{- define "glidepath-control-plane.selfCluster" -}}
{{- $self := required "clusterName must be set" .Values.clusterName -}}
{{- $zone := .Values.clusterZone | default "lower" -}}
{{- $roles := .Values.clusterRoles | default (list "control-plane") -}}
{{- range .Values.clusters -}}
{{- if eq .name $self -}}
{{- $zone = .zone | default "lower" -}}
{{- $roles = .roles | default (list "control-plane") -}}
{{- end -}}
{{- end -}}
{{- if ne $zone "lower" -}}
{{- fail (printf "the control-plane cluster '%s' must be zone lower (its pipelines deploy to it directly; ADR-0024)" $self) -}}
{{- end -}}
{{- if not (has "control-plane" $roles) -}}
{{- fail (printf "the control-plane cluster '%s' must have the control-plane role (ADR-0024)" $self) -}}
{{- end -}}
{{- dict "name" $self "zone" $zone "roles" $roles | toJson -}}
{{- end -}}

{{/*
The taxonomy forwarded to every app's glidepath-app, for its environment checks: the control plane's own name and
every known cluster's zone.
*/}}
{{- define "glidepath-control-plane.clusterTaxonomy" -}}
{{- $self := include "glidepath-control-plane.selfCluster" . | fromJson -}}
{{- $zones := dict $self.name $self.zone -}}
{{- range (include "glidepath-control-plane.remoteClusters" . | fromJson).items -}}
{{- $_ := set $zones .name .zone -}}
{{- end -}}
{{- dict "self" $self.name "zones" $zones | toYaml -}}
{{- end -}}
