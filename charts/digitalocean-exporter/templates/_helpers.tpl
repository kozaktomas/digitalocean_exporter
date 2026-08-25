{{- define "digitalocean-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "digitalocean-exporter.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "digitalocean-exporter.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "digitalocean-exporter.labels" -}}
app.kubernetes.io/name: {{ include "digitalocean-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "digitalocean-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "digitalocean-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "digitalocean-exporter.secretName" -}}
{{- if .Values.digitalocean.existingSecret -}}
{{- .Values.digitalocean.existingSecret -}}
{{- else -}}
{{- include "digitalocean-exporter.fullname" . -}}
{{- end -}}
{{- end -}}

{{- define "digitalocean-exporter.spacesSecretName" -}}
{{- if .Values.spaces.existingSecret -}}
{{- .Values.spaces.existingSecret -}}
{{- else -}}
{{- printf "%s-spaces" (include "digitalocean-exporter.fullname" .) -}}
{{- end -}}
{{- end -}}
