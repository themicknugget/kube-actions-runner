{{/*
Expand the name of the chart.
*/}}
{{- define "kube-actions-runner.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "kube-actions-runner.fullname" -}}
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
{{- define "kube-actions-runner.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kube-actions-runner.labels" -}}
helm.sh/chart: {{ include "kube-actions-runner.chart" . }}
{{ include "kube-actions-runner.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kube-actions-runner
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kube-actions-runner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-actions-runner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kube-actions-runner.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kube-actions-runner.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the secret to use
*/}}
{{- define "kube-actions-runner.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- printf "%s-secrets" (include "kube-actions-runner.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Return the image name
*/}}
{{- define "kube-actions-runner.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Cloudflare tunnel fullname
*/}}
{{- define "kube-actions-runner.cloudflare.fullname" -}}
{{- printf "%s-cloudflared" (include "kube-actions-runner.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Cloudflare tunnel labels
*/}}
{{- define "kube-actions-runner.cloudflare.labels" -}}
helm.sh/chart: {{ include "kube-actions-runner.chart" . }}
{{ include "kube-actions-runner.cloudflare.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kube-actions-runner
app.kubernetes.io/component: ingress
{{- end }}

{{/*
Cloudflare tunnel selector labels
*/}}
{{- define "kube-actions-runner.cloudflare.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-actions-runner.cloudflare.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app: cloudflared
{{- end }}

{{/*
Cleanup job fullname
*/}}
{{- define "kube-actions-runner.cleanup.fullname" -}}
{{- printf "%s-cleanup" (include "kube-actions-runner.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Cleanup job labels
*/}}
{{- define "kube-actions-runner.cleanup.labels" -}}
helm.sh/chart: {{ include "kube-actions-runner.chart" . }}
{{ include "kube-actions-runner.cleanup.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kube-actions-runner
app.kubernetes.io/component: cleanup
{{- end }}

{{/*
Cleanup job selector labels
*/}}
{{- define "kube-actions-runner.cleanup.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-actions-runner.cleanup.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app: runner-cleanup
{{- end }}
