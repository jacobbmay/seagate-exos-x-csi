{{- define "csidriver.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name | kebabcase }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "csidriver.caBundleName" -}}
{{- printf "%s-storage-api-ca" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "csidriver.extraArgs" -}}
{{- range .extraArgs }}
  - {{ toYaml . }}
{{- end }}
{{- end -}}
