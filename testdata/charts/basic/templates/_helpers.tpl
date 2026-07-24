{{/*
Common labels shared by every namespaced resource in this chart.
*/}}
{{- define "basic.labels" -}}
app.kubernetes.io/name: basic
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Cluster-scoped resources (ClusterRole/ClusterRoleBinding) must be unique
across parallel test runs that reuse the same release name in different
namespaces, so their name is templated with both the release name AND
namespace.
*/}}
{{- define "basic.clusterName" -}}
{{ .Release.Name }}-{{ .Release.Namespace }}-basic
{{- end -}}
