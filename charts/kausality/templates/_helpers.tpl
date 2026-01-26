{{/*
Expand the name of the chart.
*/}}
{{- define "kausality.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kausality.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "kausality.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kausality.labels" -}}
helm.sh/chart: {{ include "kausality.chart" . }}
{{ include "kausality.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kausality.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kausality.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kausality.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kausality.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Webhook fullname (for ClusterRole naming)
*/}}
{{- define "kausality.webhookFullname" -}}
{{- printf "%s-webhook" (include "kausality.fullname" .) }}
{{- end }}

{{/*
Create the webhook service name
*/}}
{{- define "kausality.webhookServiceName" -}}
{{- include "kausality.webhookFullname" . }}
{{- end }}

{{/*
Create the certificate secret name
*/}}
{{- define "kausality.certificateSecretName" -}}
{{- printf "%s-webhook-cert" (include "kausality.fullname" .) }}
{{- end }}

{{/*
Controller fullname
*/}}
{{- define "kausality.controllerFullname" -}}
{{- printf "%s-controller" (include "kausality.fullname" .) }}
{{- end }}

{{/*
Controller labels
*/}}
{{- define "kausality.controllerLabels" -}}
helm.sh/chart: {{ include "kausality.chart" . }}
{{ include "kausality.controllerSelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Controller selector labels
*/}}
{{- define "kausality.controllerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kausality.name" . }}-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Controller service account name
*/}}
{{- define "kausality.controllerServiceAccountName" -}}
{{- printf "%s-controller" (include "kausality.fullname" .) }}
{{- end }}

{{/*
Backend fullname
*/}}
{{- define "kausality.backendFullname" -}}
{{- printf "%s-backend" (include "kausality.fullname" .) }}
{{- end }}

{{/*
Backend labels
*/}}
{{- define "kausality.backendLabels" -}}
helm.sh/chart: {{ include "kausality.chart" . }}
{{ include "kausality.backendSelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Backend selector labels
*/}}
{{- define "kausality.backendSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kausality.name" . }}-backend
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: backend
{{- end }}

{{/*
Backend service URL (for webhook to call)
*/}}
{{- define "kausality.backendServiceURL" -}}
{{- printf "http://%s.%s.svc.cluster.local:%d/webhook" (include "kausality.backendFullname" .) .Release.Namespace (.Values.backend.service.port | int) }}
{{- end }}
