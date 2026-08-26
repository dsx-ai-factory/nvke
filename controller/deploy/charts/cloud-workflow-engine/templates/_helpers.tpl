{{/*
Expand the name of the chart.
*/}}

{{- define "cloud-workflow-engine.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts
*/}}
{{- define "cloud-workflow-engine.namespace" -}}
{{- if .Values.namespaceOverride -}}
{{- .Values.namespaceOverride -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cloud-workflow-engine.fullname" -}}
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
{{- define "cloud-workflow-engine.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create postgres connection string
*/}}
{{- define "app-db.connectionString" -}}
{{- printf "postgresql://%s:%s@%s/%s?sslmode=%s" (.Values.appDb.postgresUser | urlquery) (.Values.appDb.postgresPassword | urlquery) .Values.appDb.url .Values.appDb.dbName (.Values.appDb.sslMode | default "disable") -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "cloud-workflow-engine.labels" -}}
helm.sh/chart: {{ include "cloud-workflow-engine.chart" . }}
{{ include "cloud-workflow-engine.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "cloud-workflow-engine.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cloud-workflow-engine.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "cloud-workflow-engine.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cloud-workflow-engine.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Where the authentication ConfigMap is mounted, and the file within it.

Not values: the chart owns both ends. AUTH_CONFIG_PATH is these two joined, and
nothing outside the pod observes either, so making them configurable would only
create a way for the mount and the path to disagree.
*/}}
{{- define "cloud-workflow-engine.apiAuth.mountPath" -}}
/etc/nke/auth
{{- end }}

{{- define "cloud-workflow-engine.apiAuth.configFile" -}}
auth.yaml
{{- end }}

{{/*
Refuse the two ways apiAuth.config and apiAuth.configMap.create can disagree,
each of which otherwise renders clean and does the wrong thing silently.

Called from deployment.yaml rather than from configmap-auth.yaml, because that
template is itself gated on create and so cannot see a config set without it.

Emits nothing. The shallow presence checks on audiences and issuers stop here:
every other rule belongs to internal/auth/config.go, whose errors name the field
at fault.
*/}}
{{- define "cloud-workflow-engine.apiAuth.validate" -}}
{{- $auth := .Values.apiAuth -}}
{{- $create := and $auth.enabled $auth.configMap.create -}}
{{- if and $auth.config (not $create) -}}
{{- fail "apiAuth.config is set but would not be rendered: it is only used when apiAuth.enabled and apiAuth.configMap.create are both true. Set both, or remove apiAuth.config and create the ConfigMap named by apiAuth.configMap.name yourself." -}}
{{- end -}}
{{- if and $create (not $auth.config) -}}
{{- fail "apiAuth.configMap.create is true but apiAuth.config is empty, which would mount an AuthenticationConfiguration carrying no issuers. Populate apiAuth.config, or set create to false and manage the ConfigMap yourself." -}}
{{- end -}}
{{- if $create -}}
{{- if or (hasKey $auth.config "apiVersion") (hasKey $auth.config "kind") -}}
{{- fail "apiAuth.config must not carry apiVersion or kind. The chart supplies both, so a copy here renders them twice, and auth.Load refuses duplicate keys -- CWE would crash-loop rather than start. Keep audiences and issuers only. If you are pasting the file from the README, drop its first two lines." -}}
{{- end -}}
{{- if not $auth.config.audiences -}}
{{- fail "apiAuth.config.audiences is required and must not be empty. The audience is per-install and has no default; a token minted for one install must fail at another." -}}
{{- end -}}
{{- if not $auth.config.issuers -}}
{{- fail "apiAuth.config.issuers is required and must contain at least one issuer. NKE refuses to start with none, so this would be a crash loop rather than a permissive deployment." -}}
{{- end -}}
{{- end -}}
{{- end }}
