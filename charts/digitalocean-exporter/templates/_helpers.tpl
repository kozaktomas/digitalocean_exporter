{{- define "digitalocean-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "digitalocean-exporter.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "digitalocean-exporter.name" .) | trunc 63 | trimSuffix "-" -}}
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
