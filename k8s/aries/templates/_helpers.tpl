{{/*
Chart name, overridable.
*/}}
{{- define "aries.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resource name prefix. values.yaml pins this to "aries" via fullnameOverride so
resource names do not depend on the release name; without that it follows the
usual release-name convention.
*/}}
{{- define "aries.fullname" -}}
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
Labels on every resource.
*/}}
{{- define "aries.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "aries.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: aries
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Labels a workload's selector matches on: stable across chart upgrades.
*/}}
{{- define "aries.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aries.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Tool bridge ServiceAccount name. Profiles refer to it by this name.
*/}}
{{- define "aries.bridge.fullname" -}}
{{- printf "%s-bridge" (include "aries.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "aries.serviceAccountName" -}}
{{- default (include "aries.fullname" .) .Values.serviceAccount.name }}
{{- end }}
