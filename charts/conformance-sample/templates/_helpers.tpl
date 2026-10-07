{{- define "conformance-sample.namespace" -}}app-{{ .Values.appName }}-{{ .Values.envName }}{{- end -}}
{{- define "conformance-sample.labels" -}}
app.kubernetes.io/name: {{ .Values.appName }}
app.kubernetes.io/part-of: {{ .Values.appName }}
hangar.io/env: {{ .Values.envName }}
{{- end -}}
