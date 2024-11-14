{{- define "redisOperator.fullname" -}}
{{ .Values.redisOperator.name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "redisOperator.labels" -}}
app.kubernetes.io/name: {{ .Values.redisOperator.name }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/component: operator
app.kubernetes.io/part-of: {{ .Release.Name }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "redisOperator.selectorLabels" -}}
app.kubernetes.io/name: {{ .Values.redisOperator.name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: operator
{{- end }}

