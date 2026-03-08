{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-failover-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 35 characters (not 63) because the longest resource suffix
is "-leader-election-rolebinding" (28 chars) and Kubernetes names must be <= 63.
*/}}
{{- define "k8s-failover-manager.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 35 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 35 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 35 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "k8s-failover-manager.labels" -}}
app.kubernetes.io/name: {{ include "k8s-failover-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "k8s-failover-manager.selectorLabels" -}}
control-plane: controller-manager
app.kubernetes.io/name: {{ include "k8s-failover-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name for the controller manager
*/}}
{{- define "k8s-failover-manager.serviceAccountName" -}}
{{ include "k8s-failover-manager.fullname" . }}-controller-manager
{{- end }}

{{/*
ServiceAccount name for the connection killer
*/}}
{{- define "k8s-failover-manager.connectionKillerServiceAccountName" -}}
{{ include "k8s-failover-manager.fullname" . }}-connection-killer
{{- end }}
