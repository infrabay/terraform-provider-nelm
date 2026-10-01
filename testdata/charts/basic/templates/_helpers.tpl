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
namespace. clusterNameOverride (unset by default, so the name is unchanged)
is an acceptance-test hook: a FIXED name like the ones ingress-nginx or
cert-manager give their cluster-scoped objects, which a namespace move of
the release reuses while the old release still owns it.
*/}}
{{- define "basic.clusterName" -}}
{{- .Values.clusterNameOverride | default (printf "%s-%s-basic" .Release.Name .Release.Namespace) -}}
{{- end -}}
