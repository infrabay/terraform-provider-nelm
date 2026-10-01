{{/*
Common labels shared by every resource in this chart.
*/}}
{{- define "volatile.labels" -}}
app.kubernetes.io/name: volatile
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
