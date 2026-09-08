{{/*
glidepath-app.hasStage - true/"" if any pipelines flow has a step for the given stage name.
Usage: {{ include "glidepath-app.hasStage" (dict "ctx" . "name" "test") }}
Returns "true" or "" (named templates can only return strings) - compare with `eq ... "true"`.
*/}}
{{- define "glidepath-app.hasStage" -}}
{{- $targetName := .name | default "" -}}
{{- $flows := dict -}}
{{- if hasKey . "ctx" -}}
  {{- $flows = .ctx.Values.pipelines | default (dict) -}}
{{- else -}}
  {{- $flows = .Values.pipelines | default (dict) -}}
{{- end -}}
{{- $found := "" -}}
{{- range $flowName, $flow := $flows -}}
  {{- $normalizedFlow := fromYaml (include "glidepath-app.normalizeFlow" (dict "flow" $flow)) -}}
  {{- $steps := $normalizedFlow.steps | default (list) -}}
  {{- range $step := $steps -}}
    {{- if eq ($step.stage | default "") $targetName -}}
      {{- $found = "true" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- $found -}}
{{- end -}}

{{/*
glidepath-app.cacheSize - t-shirt size lookup for build.cache.size (see
templates/env/build-cache-pvc.yaml). Same sizing as the legacy cd-pipelines-user chart.
Defaults to "small".
*/}}
{{- define "glidepath-app.cacheSize" -}}
{{- $size := .size | default "small" -}}
{{- if eq $size "small" -}}1Gi
{{- else if eq $size "medium" -}}2.5Gi
{{- else if eq $size "large" -}}5Gi
{{- else if eq $size "xlarge" -}}8Gi
{{- else -}}1Gi
{{- end -}}
{{- end -}}

{{/*
glidepath-app.sourceVolumeSize - t-shirt size lookup for build.sourceVolume.size, the
ephemeral per-PipelineRun source workspace (checkout + build + kaniko context) - a
separate, larger table than cacheSize above. Duplicated as bash in
glidepath-catalog's deliver-onboarding-files.yaml, which renders at Tekton runtime
rather than Helm time and so can't call this helper.
*/}}
{{- define "glidepath-app.sourceVolumeSize" -}}
{{- $size := .size | default "small" -}}
{{- if eq $size "small" -}}2Gi
{{- else if eq $size "medium" -}}5Gi
{{- else if eq $size "large" -}}10Gi
{{- else if eq $size "xlarge" -}}20Gi
{{- else -}}2Gi
{{- end -}}
{{- end -}}

{{/*
glidepath-app.envNamespace - builds `<type>-<app-name>-<env>`, the one namespace
pattern this platform uses. Every namespace an app gets (cicd, a deploy target, release
staging, a PR env) is a flat peer under this pattern, not a hierarchy - see
docs/naming-conventions.md.

Takes [<root context>, <env>] as a list (Helm named templates take one arg).
Usage: {{ include "glidepath-app.envNamespace" (list $ "staging") }}
*/}}
{{- define "glidepath-app.envNamespace" -}}
{{- $ctx := index . 0 -}}
{{- $env := index . 1 -}}
{{- $ctx.Values.platformIdentity.type }}-{{ $ctx.Values.platformIdentity.appName }}-{{ $env }}
{{- end -}}

{{/*
glidepath-app.localDeployEnvs - lowerEnvironments plus same-cluster
upperEnvironments entries: every env that needs pipeline-runner RBAC into a local
namespace. Cluster-mapped upper envs (docs/multi-cluster.md) are excluded - they have no
local namespace to grant RBAC into.

Usage: {{ include "glidepath-app.localDeployEnvs" . | fromYamlArray }}
*/}}
{{- define "glidepath-app.localDeployEnvs" -}}
{{- $envs := .Values.deploy.lowerEnvironments | default (list) -}}
{{- range .Values.deploy.upperEnvironments | default (list) -}}
  {{- if kindIs "map" . -}}
    {{- if not .cluster -}}
      {{- $envs = append $envs .name -}}
    {{- end -}}
  {{- else -}}
    {{- $envs = append $envs . -}}
  {{- end -}}
{{- end -}}
{{- $envs | toYaml -}}
{{- end -}}

{{/*
glidepath-app.localUpperEnvs - same-cluster upperEnvironments entries only, no
lowerEnvironments mixed in. Used by release-application.yaml/appproject.yaml: one ArgoCD
Application/destination per local upper env ("dev" is a deploy-stage concept, never an
ArgoCD one). Cluster-mapped entries get no local Application; theirs is delivered via
GitOps instead (docs/multi-cluster.md).

Usage: {{ include "glidepath-app.localUpperEnvs" . | fromYamlArray }}
*/}}
{{- define "glidepath-app.localUpperEnvs" -}}
{{- $envs := list -}}
{{- range .Values.deploy.upperEnvironments | default (list) -}}
  {{- if kindIs "map" . -}}
    {{- if not .cluster -}}
      {{- $envs = append $envs .name -}}
    {{- end -}}
  {{- else -}}
    {{- $envs = append $envs . -}}
  {{- end -}}
{{- end -}}
{{- $envs | toYaml -}}
{{- end -}}

{{/*
glidepath-app.upperEnvClusters - env name -> cluster map ("" = same-cluster) built
from deploy.upperEnvironments, normalizing the plain-string vs {name, cluster} shape
once. Shared by validateFlows's consistency check and by the renderers that resolve a
release step's cluster when the step omits cluster: - without this shared fallback, an
omitted cluster: silently produced a same-cluster release instead of the tenant's
declared cluster-mapped one.

Usage: {{ $clusters := include "glidepath-app.upperEnvClusters" . | fromYaml }}
*/}}
{{- define "glidepath-app.upperEnvClusters" -}}
{{- $result := dict -}}
{{- range .Values.deploy.upperEnvironments | default (list) -}}
  {{- if kindIs "map" . -}}
    {{- $_ := set $result .name (.cluster | default "") -}}
  {{- else -}}
    {{- $_ := set $result . "" -}}
  {{- end -}}
{{- end -}}
{{- $result | toYaml -}}
{{- end -}}

{{/*
glidepath-app.resolveStepCluster - a release step's effective cluster: its own
cluster: if set, else whatever upperEnvClusters registers for its env, else "". This is
what actually implements the "omit cluster: and let it resolve" advice from validateFlows.

Takes [<root context>, <step>] as a list.
Usage: {{ include "glidepath-app.resolveStepCluster" (list $ $step) }}
*/}}
{{- define "glidepath-app.resolveStepCluster" -}}
{{- $ctx := index . 0 -}}
{{- $step := index . 1 -}}
{{- if $step.cluster -}}
{{- $step.cluster -}}
{{- else -}}
{{- $clusters := fromYaml (include "glidepath-app.upperEnvClusters" $ctx) -}}
{{- get $clusters ($step.env | default "") | default "" -}}
{{- end -}}
{{- end -}}

{{/*
glidepath-app.hasClusterMappedUpperEnv - "true"/"false": does any
deploy.upperEnvironments entry map to a different physical cluster? Gates whether
templates/clusters/read-registry-rbac.yaml renders at all - most apps don't need it.

Usage: {{ include "glidepath-app.hasClusterMappedUpperEnv" . }}
*/}}
{{- define "glidepath-app.hasClusterMappedUpperEnv" -}}
{{- $found := false -}}
{{- range .Values.deploy.upperEnvironments | default (list) -}}
  {{- if and (kindIs "map" .) .cluster -}}
    {{- $found = true -}}
  {{- end -}}
{{- end -}}
{{- $found -}}
{{- end -}}

{{/*
glidepath-app.namespace - shorthand for envNamespace with env="cicd": this
Application's own CI/CD execution namespace, a sibling of "dev"/"staging"/"pr-42", not a
special base others build on. The common case, so it takes the context directly.

Usage: {{ include "glidepath-app.namespace" . }} (or `$` from inside a range/with block)
*/}}
{{- define "glidepath-app.namespace" -}}
{{- include "glidepath-app.envNamespace" (list . "cicd") -}}
{{- end -}}

{{/*
glidepath-app.normalizeFlow - accepts either the newer object form
  pipelines:
    ci:
      trigger: { type: branch.created }
      steps: [ ... ]

or the legacy list form that the old config used:
  pipelines:
    ci:
      - task: build
        trigger: branch.created

The normalized result is a dict with a `trigger` object and a `steps` list so the
renderer can treat both forms uniformly.
*/}}
{{- define "glidepath-app.normalizeFlow" -}}
{{- $flow := .flow -}}
{{- $rootTrigger := dict -}}
{{- $steps := list -}}
{{- if kindIs "slice" $flow -}}
  {{- range $idx, $entry := $flow -}}
    {{- $step := dict -}}
    {{- if hasKey $entry "task" -}}
      {{- $_ := set $step "stage" $entry.task -}}
    {{- else if hasKey $entry "stage" -}}
      {{- $_ := set $step "stage" $entry.stage -}}
    {{- end -}}
    {{- if hasKey $entry "env" -}}
      {{- $_ := set $step "env" $entry.env -}}
    {{- end -}}
    {{- if hasKey $entry "testName" -}}
      {{- $_ := set $step "testName" $entry.testName -}}
    {{- end -}}
    {{- if hasKey $entry "cluster" -}}
      {{- $_ := set $step "cluster" $entry.cluster -}}
    {{- end -}}
    {{- if hasKey $entry "name" -}}
      {{- $_ := set $step "name" $entry.name -}}
    {{- end -}}
    {{- if hasKey $entry "gitopsRepo" -}}
      {{- $_ := set $step "gitopsRepo" $entry.gitopsRepo -}}
    {{- end -}}
    {{- if hasKey $entry "manifestPath" -}}
      {{- $_ := set $step "manifestPath" $entry.manifestPath -}}
    {{- end -}}
    {{- if eq $idx 0 -}}
      {{- if hasKey $entry "trigger" -}}
        {{- $entryTrigger := $entry.trigger -}}
        {{- if kindIs "map" $entryTrigger -}}
          {{- if hasKey $entryTrigger "source" -}}
            {{- $_ := set $rootTrigger "source" (get $entryTrigger "source") -}}
          {{- end -}}
          {{- if hasKey $entryTrigger "event" -}}
            {{- $_ := set $rootTrigger "event" (get $entryTrigger "event") -}}
          {{- end -}}
          {{- if hasKey $entryTrigger "type" -}}
            {{- $_ := set $rootTrigger "type" (get $entryTrigger "type") -}}
            {{- if not (hasKey $entryTrigger "event") -}}
              {{- $_ := set $rootTrigger "event" (get $entryTrigger "type") -}}
            {{- end -}}
          {{- end -}}
        {{- else if kindIs "string" $entryTrigger -}}
          {{- $_ := set $rootTrigger "source" "git" -}}
          {{- $_ := set $rootTrigger "event" $entryTrigger -}}
          {{- $_ := set $rootTrigger "type" $entryTrigger -}}
        {{- end -}}
      {{- end -}}
      {{- if hasKey $entry "branch" -}}
        {{- $_ := set $rootTrigger "branch" $entry.branch -}}
      {{- end -}}
      {{- if hasKey $entry "branchPattern" -}}
        {{- $_ := set $rootTrigger "branchPattern" $entry.branchPattern -}}
      {{- end -}}
      {{- if hasKey $entry "tagPattern" -}}
        {{- $_ := set $rootTrigger "tagPattern" $entry.tagPattern -}}
      {{- end -}}
      {{- if hasKey $entry "filePathPattern" -}}
        {{- $_ := set $rootTrigger "filePathPattern" $entry.filePathPattern -}}
      {{- end -}}
    {{- end -}}
    {{- $steps = append $steps $step -}}
  {{- end -}}
{{- else if kindIs "map" $flow -}}
  {{- if hasKey $flow "trigger" -}}
    {{- $flowTrigger := get $flow "trigger" -}}
    {{- if kindIs "map" $flowTrigger -}}
      {{- if hasKey $flowTrigger "source" -}}
        {{- $_ := set $rootTrigger "source" (get $flowTrigger "source") -}}
      {{- end -}}
      {{- if hasKey $flowTrigger "event" -}}
        {{- $_ := set $rootTrigger "event" (get $flowTrigger "event") -}}
      {{- end -}}
      {{- if hasKey $flowTrigger "type" -}}
        {{- $_ := set $rootTrigger "type" (get $flowTrigger "type") -}}
        {{- if not (hasKey $flowTrigger "event") -}}
          {{- $_ := set $rootTrigger "event" (get $flowTrigger "type") -}}
        {{- end -}}
      {{- end -}}
    {{- else if kindIs "string" $flowTrigger -}}
      {{- $_ := set $rootTrigger "source" "git" -}}
      {{- $_ := set $rootTrigger "event" $flowTrigger -}}
      {{- $_ := set $rootTrigger "type" $flowTrigger -}}
    {{- end -}}
    {{- if hasKey $flow.trigger "branch" -}}
      {{- $_ := set $rootTrigger "branch" (get $flow.trigger "branch") -}}
    {{- end -}}
    {{- if hasKey $flow.trigger "branchPattern" -}}
      {{- $_ := set $rootTrigger "branchPattern" (get $flow.trigger "branchPattern") -}}
    {{- end -}}
    {{- if hasKey $flow.trigger "tagPattern" -}}
      {{- $_ := set $rootTrigger "tagPattern" (get $flow.trigger "tagPattern") -}}
    {{- end -}}
    {{- if hasKey $flow.trigger "filePathPattern" -}}
      {{- $_ := set $rootTrigger "filePathPattern" (get $flow.trigger "filePathPattern") -}}
    {{- end -}}
  {{- end -}}
  {{- if hasKey $flow "steps" -}}
    {{- range $entry := $flow.steps -}}
      {{- $step := dict -}}
      {{- if hasKey $entry "stage" -}}
        {{- $_ := set $step "stage" $entry.stage -}}
      {{- end -}}
      {{- if hasKey $entry "env" -}}
        {{- $_ := set $step "env" $entry.env -}}
      {{- end -}}
      {{- if hasKey $entry "testName" -}}
        {{- $_ := set $step "testName" $entry.testName -}}
      {{- end -}}
      {{- if hasKey $entry "cluster" -}}
        {{- $_ := set $step "cluster" $entry.cluster -}}
      {{- end -}}
      {{- if hasKey $entry "name" -}}
        {{- $_ := set $step "name" $entry.name -}}
      {{- end -}}
      {{- if hasKey $entry "gitopsRepo" -}}
        {{- $_ := set $step "gitopsRepo" $entry.gitopsRepo -}}
      {{- end -}}
      {{- if hasKey $entry "manifestPath" -}}
        {{- $_ := set $step "manifestPath" $entry.manifestPath -}}
      {{- end -}}
      {{- $steps = append $steps $step -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- if eq ($rootTrigger.source | default "") "" -}}
  {{- $defaultEvent := $rootTrigger.event | default ($rootTrigger.type | default "") -}}
  {{- /* No release.created - PaC has no GitHub "release" webhook support (see
  flow-triggers.yaml). validateFlows below re-derives this same git-rooted inference;
  keep both lists in sync. */ -}}
  {{- if or (eq $defaultEvent "push") (eq $defaultEvent "pull_request") (eq $defaultEvent "branch.created") (eq $defaultEvent "tag") (eq $defaultEvent "deploy") -}}
    {{- $_ := set $rootTrigger "source" "git" -}}
  {{- else if ne $defaultEvent "" -}}
    {{- $_ := set $rootTrigger "source" "event" -}}
  {{- end -}}
{{- end -}}
{{- dict "trigger" $rootTrigger "steps" $steps | toYaml -}}
{{- end -}}

{{/*
glidepath-app.labels - see glidepath-catalog's identical helper for the full
rationale. Includes hangar.io/app unconditionally, not just where selection strictly
needs it - many Applications' resources share one namespace (docs/naming-conventions.md).
*/}}
{{/*
glidepath-app.validateFlows - validates pipeline flow definitions. Rules:
- Any stage may be git-rooted; start-flow-root-span.yaml's task is idempotent either way.
- build, if present, must be the first (and only) step - nothing chains into or from it.
- A git-rooted release must be the flow's sole step (it starts a new trace root).
  Event-chained release has no position restriction - deploy/test/release may repeat or
  reorder freely once past a required leading build; flow-triggers.yaml keys off each
  step's actual predecessor, not a fixed slot.
- cluster: is only valid on a release step.
- env: is required for deploy, release, and test steps.
- A deploy step's env must be listed under deploy.lowerEnvironments/upperEnvironments -
  deploy-rbac.yaml only grants pipeline-runner access to envs from that list, so this
  catches what would otherwise be a late, bare Forbidden RBAC error.
- A test step needs a resolvable test name (its own `testName` or top-level
  `test.name`), so repeated test steps in one flow can be told apart. Not called
  `name` at the step level - that field already means the deploy/release target name
  (see resolveStepCluster) - so a test step's own identifier is `testName` instead.

Fails fast with a descriptive message if violated.
*/}}
{{- define "glidepath-app.validateFlows" -}}
{{- $flows := .Values.pipelines | default (dict) -}}
{{- $defaultTestName := .Values.test.name | default "" -}}
{{- $lowerEnvs := .Values.deploy.lowerEnvironments | default (list) -}}
{{- /* upperEnvironments entries are a plain string (same-cluster) or a {name, cluster}
object (docs/multi-cluster.md); upperEnvClusters normalizes both shapes. */ -}}
{{- $upperEnvClusters := fromYaml (include "glidepath-app.upperEnvClusters" .) -}}
{{- $upperEnvs := keys $upperEnvClusters -}}
{{- $deployEnvs := concat $lowerEnvs $upperEnvs -}}
{{- range $flowName, $flow := $flows -}}
  {{- $normalized := fromYaml (include "glidepath-app.normalizeFlow" (dict "flow" $flow)) -}}
  {{- $trigger := $normalized.trigger | default (dict) -}}
  {{- $steps := $normalized.steps | default (list) -}}
  {{- $stepCount := len $steps -}}

  {{- if gt $stepCount 0 -}}
    {{- $triggerSource := $trigger.source | default "" -}}
    {{- if eq $triggerSource "" -}}
      {{- $triggerEvent := $trigger.event | default ($trigger.type | default "") -}}
      {{- if or (eq $triggerEvent "push") (eq $triggerEvent "pull_request") (eq $triggerEvent "branch.created") (eq $triggerEvent "tag") -}}
        {{- $triggerSource = "git" -}}
      {{- else if ne $triggerEvent "" -}}
        {{- $triggerSource = "event" -}}
      {{- end -}}
    {{- end -}}

    {{- $firstStage := (index $steps 0).stage | default "build" -}}

    {{- if eq $triggerSource "git" -}}
      {{- if eq $firstStage "release" -}}
        {{- if gt $stepCount 1 -}}
          {{- fail (printf "Flow '%s': git-rooted release must be first (and only) step in flow to create a new trace root. For release that continues existing trace, make it event-chained (final step after deploy)." $flowName) -}}
        {{- end -}}
      {{- end -}}
    {{- end -}}

    {{- range $index, $step := $steps -}}
      {{- $stageName := $step.stage | default "" -}}

      {{- if or (eq $stageName "deploy") (eq $stageName "release") (eq $stageName "test") -}}
        {{- if not $step.env -}}
          {{- fail (printf "Flow '%s' step %d (%s): env is required for %s stage" $flowName (add $index 1) $stageName $stageName) -}}
        {{- end -}}
      {{- end -}}

      {{- /* deploy-rbac.yaml only grants access to envs listed under
      deploy.lowerEnvironments/upperEnvironments */ -}}
      {{- if and (eq $stageName "deploy") $step.env (not (has $step.env $deployEnvs)) -}}
        {{- fail (printf "Flow '%s' step %d: deploy env '%s' is not listed under deploy.lowerEnvironments or deploy.upperEnvironments - pipeline-runner has no RBAC into that namespace, this would fail at deploy time with a Forbidden error instead. Add it to one of those lists." $flowName (add $index 1) $step.env) -}}
      {{- end -}}

      {{- if eq $stageName "test" -}}
        {{- if and (not $step.testName) (not $defaultTestName) -}}
          {{- fail (printf "Flow '%s' step %d: test stage needs a test name - set this step's own `testName`, or a top-level `test.name` shared by all test steps." $flowName (add $index 1)) -}}
        {{- end -}}
      {{- end -}}

      {{- if and $step.cluster (ne $stageName "release") -}}
        {{- fail (printf "Flow '%s' step %d: cluster is only valid for release stage, not %s" $flowName (add $index 1) $stageName) -}}
      {{- end -}}

      {{- /* upperEnvironments is the single source of truth for env->cluster; a step's
      own cluster: (if set) is only checked for consistency, not a second input. */ -}}
      {{- if and (eq $stageName "release") $step.env -}}
        {{- if not (has $step.env $upperEnvs) -}}
          {{- fail (printf "Flow '%s' step %d: release env '%s' is not listed under deploy.upperEnvironments. Add it there (as a plain name for a same-cluster env, or {name, cluster} for one hosted on a different cluster - see docs/multi-cluster.md)." $flowName (add $index 1) $step.env) -}}
        {{- end -}}
        {{- $registeredCluster := get $upperEnvClusters $step.env -}}
        {{- if and $step.cluster (ne $step.cluster $registeredCluster) -}}
          {{- fail (printf "Flow '%s' step %d: release step declares cluster '%s' but deploy.upperEnvironments has env '%s' mapped to cluster '%s' - these must agree. Prefer omitting the step's own cluster: and letting it resolve from upperEnvironments." $flowName (add $index 1) $step.cluster $step.env $registeredCluster) -}}
        {{- end -}}
      {{- end -}}

      {{- if and (gt $index 0) $step.trigger -}}
        {{- fail (printf "Flow '%s' step %d: trigger can only be defined on first step, not step %d (%s)" $flowName 1 (add $index 1) $stageName) -}}
      {{- end -}}

      {{- if and (eq $stageName "build") (gt $index 0) -}}
        {{- fail (printf "Flow '%s' step %d: build can only be the first step of a flow, not step %d - nothing chains into build. Give it its own flow, or move it to step 1." $flowName (add $index 1) (add $index 1)) -}}
      {{- end -}}
    {{- end -}}

    {{- $triggerEvent := $trigger.event | default ($trigger.type | default "") -}}
    {{- if eq $triggerEvent "tag" -}}
      {{- $tagPattern := $trigger.tagPattern | default "" -}}
      {{- if not $tagPattern -}}
        {{- fail (printf "Flow '%s': trigger.tagPattern is required when event is 'tag' (e.g., 'v[0-9]+.[0-9]+.[0-9]+')" $flowName) -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- define "glidepath-app.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: glidepath
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
hangar.io/component: app
hangar.io/app: {{ .Values.platformIdentity.appName }}
{{- end -}}
