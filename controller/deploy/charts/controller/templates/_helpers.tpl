{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{/*
Expand the name of the chart.
*/}}

{{- define "controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The shared management-plane namespace: the Namespace object, the Roles and
RoleBindings scoped to it, and the managementNamespace key the controller reads
from nvke-controller-config all come from here, so they cannot drift apart.

The default is nvke-system. A site installed before this value existed holds its
Secrets in nke-system and must pin nvkeSystemNamespace.name to it: the old
namespace is not also read, so a site that moves leaves its published
kubeconfigs behind.
*/}}
{{- define "controller.managementNamespace" -}}
{{- .Values.nvkeSystemNamespace.name | default "nvke-system" -}}
{{- end }}

{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts
*/}}
{{- define "controller.namespace" -}}
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
{{- define "controller.fullname" -}}
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
{{- define "controller.chart" -}}
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
{{- define "controller.labels" -}}
helm.sh/chart: {{ include "controller.chart" . }}
{{ include "controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The nvke-controller-config ConfigMap, its file key, and where it is mounted.
CONTROLLER_CONFIG_PATH is the mount and the file joined, so none are values.
*/}}
{{- define "controller.controllerConfig.name" -}}
{{- required "controllerConfig.name must not be empty" .Values.controllerConfig.name -}}
{{- end }}

{{/*
The ConfigMap carrying the tenant RBAC manifest. The ConfigMap itself, the Role
granting get on it, that Role's resourceNames, and the RoleBinding all render
from this one helper: a resourceNames entry left on another name is a render
that installs clean and then denies the controller at reconcile time.
*/}}
{{- define "controller.rbacConfigMap.name" -}}
{{- required "rbacConfigMap.name must not be empty" .Values.rbacConfigMap.name -}}
{{- end }}

{{- define "controller.controllerConfig.file" -}}
config.yaml
{{- end }}

{{- define "controller.controllerConfig.mountPath" -}}
/etc/nvke/controller-config
{{- end }}

{{/*
Refuse an env entry for a variable nvke-controller-config carries: the controller
reads that setting only from the file, so the entry would do nothing.
files/controller-config-env.yaml lists those variables, each with the
config.yaml key that replaces it.
CONTROLLER_CONFIG_PATH is refused too: a second entry could point the controller
at another file, or at none.
*/}}
{{- define "controller.controllerConfig.validateEnv" -}}
{{- $carried := .Files.Get "files/controller-config-env.yaml" | fromYaml }}
{{- if or (not $carried) (hasKey $carried "Error") }}
{{- fail "files/controller-config-env.yaml is missing or does not parse" }}
{{- end }}
{{- range .Values.env }}
{{- $name := toString .name }}
{{- if eq $name "CONTROLLER_CONFIG_PATH" }}
{{- fail (printf "env sets CONTROLLER_CONFIG_PATH, which the chart sets to the mounted %s file" (include "controller.controllerConfig.name" $)) }}
{{- end }}
{{- if hasKey $carried $name }}
{{- fail (printf "env sets %s, which %s carries as %s; set it through its chart value instead" $name (include "controller.controllerConfig.name" $) (get $carried $name)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
runbooksDir in nvke-controller-config: profiles.runbooksPath when set, else the
runbooks/ directory the profiles image provides, else empty, which leaves the
controller its own default of profilesDir/runbooks.
*/}}
{{- define "controller.runbooksDir" -}}
{{- if .Values.profiles.runbooksPath -}}
{{- .Values.profiles.runbooksPath -}}
{{- else if .Values.profiles.image.enabled -}}
{{- printf "%s/runbooks" .Values.profiles.path -}}
{{- end -}}
{{- end }}

{{/*
(list value goDefault): the value, or goDefault when it is null or empty, as the
controller treats an empty variable. sprig's default would also replace false and 0.
*/}}
{{- define "controller.controllerConfig.orGoDefault" -}}
{{- $value := index . 0 -}}
{{- if or (kindIs "invalid" $value) (eq (toString $value) "") -}}
{{- index . 1 -}}
{{- else -}}
{{- $value -}}
{{- end -}}
{{- end }}

{{/*
A Go duration string in nanoseconds, the unit of time.Duration; refuses the
render if it does not parse, as the controller would refuse it at startup.
*/}}
{{- define "controller.durationNanoseconds" -}}
{{- $base := toDate "2006-01-02" "2000-01-01" -}}
{{- ((mustDateModify . $base).Sub $base).Nanoseconds -}}
{{- end }}

{{/*
Refuse the credential settings the controller would refuse at startup
(internal/config/credential_monitoring.go), so a bad value fails helm upgrade
instead of crash-looping the new pod. The collector inputs are always checked;
every other bound only while its feature is enabled.
*/}}
{{- define "controller.validateCredentialMonitoring" -}}
{{- $renewal := .Values.scheduledMaintenance.certRenewal -}}
{{- $monitoring := .Values.credentialMonitoring -}}
{{- $renewalEnabled := eq (include "controller.controllerConfig.orGoDefault" (list $renewal.enabled true)) "true" -}}
{{- $monitoringEnabled := eq (include "controller.controllerConfig.orGoDefault" (list $monitoring.enabled true)) "true" -}}
{{- $interval := include "controller.durationNanoseconds" ($renewal.interval | default "12h") | int64 -}}
{{- $thresholdDays := include "controller.controllerConfig.orGoDefault" (list $renewal.thresholdDays 30) | int64 -}}
{{- $inspection := include "controller.durationNanoseconds" ($monitoring.inspectionInterval | default "1h") | int64 -}}
{{- $maxAge := include "controller.durationNanoseconds" ($monitoring.observationMaxAge | default "3h") | int64 -}}
{{- $warningDays := include "controller.controllerConfig.orGoDefault" (list $monitoring.warningDays 45) | int64 -}}
{{- if le $maxAge 0 -}}
{{- fail "credentialMonitoring.observationMaxAge must be positive" -}}
{{- end -}}
{{- if le $warningDays 0 -}}
{{- fail "credentialMonitoring.warningDays must be positive" -}}
{{- end -}}
{{- if $renewalEnabled -}}
{{- if or (lt $interval 3600000000000) (gt $interval 86400000000000) -}}
{{- fail "scheduledMaintenance.certRenewal.interval must be between 1h and 24h while renewal is enabled" -}}
{{- end -}}
{{- if or (lt $thresholdDays 7) (gt $thresholdDays 90) -}}
{{- fail "scheduledMaintenance.certRenewal.thresholdDays must be between 7 and 90 while renewal is enabled" -}}
{{- end -}}
{{- end -}}
{{- if $monitoringEnabled -}}
{{- if or (lt $inspection 300000000000) (gt $inspection 43200000000000) -}}
{{- fail "credentialMonitoring.inspectionInterval must be between 5m and 12h while monitoring is enabled" -}}
{{- end -}}
{{- if or (lt $maxAge (mul 2 $inspection)) (gt $maxAge 86400000000000) -}}
{{- fail "credentialMonitoring.observationMaxAge must be at least twice credentialMonitoring.inspectionInterval and at most 24h while monitoring is enabled" -}}
{{- end -}}
{{- if or (lt $warningDays 9) (gt $warningDays 120) -}}
{{- fail "credentialMonitoring.warningDays must be between 9 and 120 while monitoring is enabled" -}}
{{- end -}}
{{- if and $renewalEnabled (lt (mul (sub $warningDays $thresholdDays) 86400000000000) (mul 2 $interval)) -}}
{{- fail "credentialMonitoring.warningDays must exceed scheduledMaintenance.certRenewal.thresholdDays by at least two renewal intervals while monitoring and renewal are enabled" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Refuse a values file that still sets a key this chart renamed. Helm ignores an
unknown key without a word, so the old key would leave the chart on its defaults:
for nkeOIDCAuth, tenant-cluster OIDC quietly reverting to disabled.

Keyed off key presence, not truth: an explicitly supplied empty map is false in
a Helm condition, and would otherwise slip past. The old keys are absent from
values.yaml so that presence only ever comes from the caller. Emits nothing.
*/}}
{{- define "controller.renamedValues.validate" -}}
{{- if hasKey .Values "nkeSystemNamespace" -}}
{{- fail "nkeSystemNamespace was renamed to nvkeSystemNamespace and is no longer read. Move its settings (create, annotations) under nvkeSystemNamespace." -}}
{{- end -}}
{{- if hasKey .Values "nkeOIDCAuth" -}}
{{- fail "nkeOIDCAuth was renamed to nvkeOIDCAuth and is no longer read. Move its settings (enabled, tenantRbacName, issuerURL, discoveryURL, caBundleSecret) under nvkeOIDCAuth." -}}
{{- end -}}
{{- end }}

{{/*
Refuse the ways apiAuth's values can disagree with each other or with the chart,
each of which otherwise renders clean and does the wrong thing silently.

Called from deployment.yaml, beside the other value checks.

Emits nothing. The shallow presence checks on audiences and issuers stop here:
every other rule belongs to auth/authn/config.go, whose errors name the field
at fault.
*/}}
{{- define "controller.apiAuth.validate" -}}
{{- $root := . -}}
{{- $auth := .Values.apiAuth -}}
{{- if hasKey $auth "configMap" -}}
{{- fail (printf "apiAuth.configMap was removed. The chart no longer renders or mounts an authentication ConfigMap: the controller reads apiAuth.config from the apiAuth section of %s. Move a ConfigMap you managed into apiAuth.config, put any CA bundle from extraFiles inline as jwks.caBundle, and drop apiAuth.configMap." (include "controller.controllerConfig.name" $root)) -}}
{{- end -}}
{{- range $root.Values.env -}}
{{- if eq (toString .name) "AUTH_CONFIG_PATH" -}}
{{- fail (printf "env sets AUTH_CONFIG_PATH, which the controller no longer reads and refuses at startup. Move the file's audiences and issuers into apiAuth.config, which the chart renders into %s, and drop the env entry." (include "controller.controllerConfig.name" $root)) -}}
{{- end -}}
{{- end -}}
{{- if and $auth.permissive (not $auth.enabled) -}}
{{- fail "apiAuth.permissive is true but apiAuth.enabled is false. Permissive mode applies only while authentication is on, and the controller refuses it otherwise. Set apiAuth.enabled, or drop apiAuth.permissive." -}}
{{- end -}}
{{- if and $auth.config (not $auth.enabled) -}}
{{- fail "apiAuth.config is set but apiAuth.enabled is false, so it would not be rendered. Set apiAuth.enabled, or remove apiAuth.config." -}}
{{- end -}}
{{- if $auth.enabled -}}
{{- if not $auth.config -}}
{{- fail "apiAuth.enabled is true but apiAuth.config is empty. Populate apiAuth.config with the audiences and issuers to trust." -}}
{{- end -}}
{{- if or (hasKey $auth.config "apiVersion") (hasKey $auth.config "kind") -}}
{{- fail (printf "apiAuth.config must not carry apiVersion or kind. They belonged to the retired auth.yaml file; the chart renders apiAuth.config into the apiAuth section of %s, whose strict loader refuses them. Keep audiences and issuers only." (include "controller.controllerConfig.name" $root)) -}}
{{- end -}}
{{- if hasKey $auth.config "enabled" -}}
{{- fail (printf "apiAuth.config must not carry enabled. apiAuth.enabled is the switch, and apiAuth.config is rendered whole into the apiAuth section of %s beside it, so a copy here is a duplicate key the controller refuses at startup. Keep audiences and issuers only." (include "controller.controllerConfig.name" $root)) -}}
{{- end -}}
{{- if hasKey $auth.config "permissive" -}}
{{- fail (printf "apiAuth.config must not carry permissive. Set apiAuth.permissive instead: the chart renders it into the apiAuth section of %s beside apiAuth.config, so a copy here is a duplicate key the controller refuses at startup." (include "controller.controllerConfig.name" $root)) -}}
{{- end -}}
{{- if not $auth.config.audiences -}}
{{- fail "apiAuth.config.audiences is required and must not be empty. The audience is per-install and has no default; a token minted for one install must fail at another." -}}
{{- end -}}
{{- if not $auth.config.issuers -}}
{{- fail "apiAuth.config.issuers is required and must contain at least one issuer. NVKE refuses to start with none, so this would be a crash loop rather than a permissive deployment." -}}
{{- end -}}
{{- /*
/etc/nke/auth is where the retired nvke-auth-config was mounted, and where
extraFiles put CA bundles. Nothing is mounted there now, so a path into it
would fail at pod start; name the replacement at render instead.
*/ -}}
{{- range $auth.config.issuers -}}
{{- $ca := (default dict .jwks).caBundlePath -}}
{{- if and $ca (hasPrefix "/etc/nke/auth/" $ca) -}}
{{- fail (printf "issuer %s sets jwks.caBundlePath to %s, in the directory the retired authentication ConfigMap was mounted at. Nothing is mounted there now. Put the PEM inline as jwks.caBundle instead, or point caBundlePath at a file you mount through .Values.volumes." (quote (default "<unnamed>" .name)) $ca) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
