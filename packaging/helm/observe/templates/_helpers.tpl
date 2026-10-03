{{- define "observe.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "observe.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "observe.nucleusName" -}}
{{- printf "%s-nucleus" (include "observe.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "observe.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "observe.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "observe.selectorLabels" -}}
app.kubernetes.io/name: {{ include "observe.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: observe
{{- end -}}

{{- define "observe.nucleusSelectorLabels" -}}
app.kubernetes.io/name: {{ include "observe.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: nucleus
{{- end -}}

{{- define "observe.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{- define "observe.secretName" -}}
{{- default (include "observe.fullname" .) .Values.secrets.existingSecret -}}
{{- end -}}

{{- define "observe.nucleusUrl" -}}
{{- if .Values.nucleus.enabled -}}
{{- printf "postgres://%s:%v/observe" (include "observe.nucleusName" .) .Values.nucleus.port -}}
{{- else -}}
{{- required "nucleus.externalUrl is required when nucleus.enabled=false" .Values.nucleus.externalUrl -}}
{{- end -}}
{{- end -}}

{{/*
Cross-value validation. Included by the Secret and the Observe StatefulSet so
every render path trips it.
*/}}
{{- define "observe.validate" -}}
{{- if and .Values.ingress.ingest.enabled (not .Values.ingest.enabled) -}}
{{- fail "ingress.ingest.enabled requires ingest.enabled=true" -}}
{{- end -}}
{{- if not .Values.secrets.existingSecret -}}
{{- if not (or .Values.secrets.autogenerate (and .Values.secrets.jwtSecret .Values.secrets.sessionSalt)) -}}
{{- fail "OBSERVE_JWT_SECRET and OBSERVE_SESSION_SALT are required. Set secrets.existingSecret (preferred), set both secrets.jwtSecret and secrets.sessionSalt, or set secrets.autogenerate=true. Without them Observe generates new ones on every restart and every session is invalidated. See docs/operations/kubernetes.md." -}}
{{- end -}}
{{- end -}}
{{- end -}}
