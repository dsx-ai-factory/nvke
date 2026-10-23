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
Where the authentication ConfigMap is mounted, and the file within it.

Not values: the chart owns both ends. AUTH_CONFIG_PATH is these two joined, and
nothing outside the pod observes either, so making them configurable would only
create a way for the mount and the path to disagree.
*/}}
{{- define "controller.apiAuth.mountPath" -}}
/etc/nke/auth
{{- end }}

{{- define "controller.apiAuth.configFile" -}}
auth.yaml
{{- end }}

{{/*
The nvke-controller-config ConfigMap, its file key, and where it is mounted.
CONTROLLER_CONFIG_PATH is the mount and the file joined, so none are values.
*/}}
{{- define "controller.controllerConfig.name" -}}
{{- required "controllerConfig.name must not be empty" .Values.controllerConfig.name -}}
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

Called from deployment.yaml rather than from configmap-auth.yaml, because that
template is itself gated on create and so cannot see a config set without it.

Emits nothing. The shallow presence checks on audiences and issuers stop here:
every other rule belongs to auth/authn/config.go, whose errors name the field
at fault.
*/}}
{{- define "controller.apiAuth.validate" -}}
{{- $auth := .Values.apiAuth -}}
{{- $create := and $auth.enabled $auth.configMap.create -}}
{{- if and $auth.config (not $create) -}}
{{- fail "apiAuth.config is set but would not be rendered: it is only used when apiAuth.enabled and apiAuth.configMap.create are both true. Set both, or remove apiAuth.config and create the ConfigMap named by apiAuth.configMap.name yourself." -}}
{{- end -}}
{{- if and $auth.configMap.extraFiles (not $create) -}}
{{- fail "apiAuth.configMap.extraFiles is set but would not be rendered: it is only used when apiAuth.enabled and apiAuth.configMap.create are both true. Set both, or drop extraFiles and put the files in the ConfigMap named by apiAuth.configMap.name yourself." -}}
{{- end -}}
{{- if and $create (not $auth.config) -}}
{{- fail "apiAuth.configMap.create is true but apiAuth.config is empty, which would mount an AuthenticationConfiguration carrying no issuers. Populate apiAuth.config, or set create to false and manage the ConfigMap yourself." -}}
{{- end -}}
{{- if $create -}}
{{- if or (hasKey $auth.config "apiVersion") (hasKey $auth.config "kind") -}}
{{- fail "apiAuth.config must not carry apiVersion or kind. The chart supplies both, so a copy here renders them twice, and auth.Load refuses duplicate keys -- the controller would crash-loop rather than start. Keep audiences and issuers only. If you are pasting the file from the README, drop its first two lines." -}}
{{- end -}}
{{- if not $auth.config.audiences -}}
{{- fail "apiAuth.config.audiences is required and must not be empty. The audience is per-install and has no default; a token minted for one install must fail at another." -}}
{{- end -}}
{{- if not $auth.config.issuers -}}
{{- fail "apiAuth.config.issuers is required and must contain at least one issuer. NVKE refuses to start with none, so this would be a crash loop rather than a permissive deployment." -}}
{{- end -}}
{{- $dir := printf "%s/" (include "controller.apiAuth.mountPath" .) -}}
{{- $configFile := include "controller.apiAuth.configFile" . -}}
{{- $extra := default dict $auth.configMap.extraFiles -}}
{{- if hasKey $extra $configFile -}}
{{- fail (printf "apiAuth.configMap.extraFiles must not contain %q: that key is the configuration file itself, and a second copy of it silently replaces the one rendered from apiAuth.config -- NVKE would load whatever is here instead. Put the configuration under apiAuth.config, and use extraFiles only for files it refers to by path." $configFile) -}}
{{- end -}}
{{- range $auth.config.issuers -}}
{{- $ca := (default dict .jwks).caBundlePath -}}
{{- if and $ca (hasPrefix $dir $ca) -}}
{{- $file := trimPrefix $dir $ca -}}
{{- if and (ne $file $configFile) (not (hasKey $extra $file)) -}}
{{- fail (printf "issuer %s sets jwks.caBundlePath to %s, but nothing puts %s there. The chart mounts only the ConfigMap it renders at that path, and that ConfigMap carries %s plus whatever apiAuth.configMap.extraFiles lists. Add the bundle under extraFiles (key %s), or point caBundlePath somewhere you mount yourself through .Values.volumes." (quote (default "<unnamed>" .name)) $ca $file $configFile $file) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The authorization bundle's mount path, fixed by the chart for the same reason the
authentication mount path is: nothing outside the pod observes it, so making it a
value would only create a way for the mount and AUTHZ_BUNDLE_PATH to disagree.
*/}}
{{- define "controller.apiAuthz.mountPath" -}}
/etc/nke/authz
{{- end }}

{{/*
The name of the ConfigMap the chart renders from its packaged default bundle
(configmap-authz.yaml). Derived from the release, so two releases in one
namespace, or a ConfigMap an operator already applied under a fixed name, do
not collide with it. controller.fullname is cut to 49 characters so the name
stays within 63 with the suffix.

Called by the ConfigMap itself, the projected volume that mounts it
(deployment.yaml) and controller.apiAuthz.validate, so all three name the
same object.
*/}}
{{- define "controller.apiAuthz.defaultConfigMapName" -}}
{{- printf "%s-authz-default" (include "controller.fullname" . | trunc 49 | trimSuffix "-") }}
{{- end }}

{{/*
A ConfigMap-name value, trimmed; empty when unset. Takes the value itself.
controller.apiAuthz.validate refuses a value that is not a string, so any
other kind reaching this renders as empty only after the render has failed.
*/}}
{{- define "controller.apiAuthz.configMapName" -}}
{{- if kindIs "string" . -}}
{{- trim . -}}
{{- end -}}
{{- end }}

{{/*
apiAuthz.existingConfigMap, trimmed; empty when unset. The self-reference check
in controller.apiAuthz.validate and the projected volume in deployment.yaml
both read the name from here, so the name checked is the name mounted.
*/}}
{{- define "controller.apiAuthz.existingConfigMap" -}}
{{- include "controller.apiAuthz.configMapName" .Values.apiAuthz.existingConfigMap -}}
{{- end }}

{{/*
The name of the ConfigMap roleMappings.json is mounted from:
apiAuthz.roleMappingsConfigMap, trimmed, or nke-authz-role-mappings when that
is empty. The operator creates this ConfigMap; the chart never renders it, so
tooling can edit the mappings without a helm upgrade undoing the edit.
internal/authz/bundle.go names the same default, and chart_bundle_sync_test.go
keeps the two equal.
*/}}
{{- define "controller.apiAuthz.roleMappingsConfigMap" -}}
{{- default "nke-authz-role-mappings" (include "controller.apiAuthz.configMapName" .Values.apiAuthz.roleMappingsConfigMap) -}}
{{- end }}

{{/*
The files the chart packages for the default bundle, keyed by the name each
has in the mounted directory. files/authz/default/ holds copies of
deploy/authz/'s hand-written files, kept identical by
chart_bundle_sync_test.go; actions.json is generated into files/authz/ by the
same go generate pass that writes deploy/authz/actions.json.

roleMappings.json is not packaged. It comes from the operator's ConfigMap
(controller.apiAuthz.roleMappingsConfigMap).

schema.json is packaged too, but not listed: nothing reads it at runtime
(internal/authz/bundle.go), so the ConfigMap does not carry it.

configmap-authz.yaml ranges over it for the ConfigMap's data and
deployment.yaml for the projected volume's items, so a file added here reaches
both.

Emits one JSON object; callers do `include ... | fromJson`.
*/}}
{{- define "controller.apiAuthz.packagedFiles" -}}
{{- dict
  "policies.cedar" "files/authz/default/policies.cedar"
  "roles.json" "files/authz/default/roles.json"
  "actions.json" "files/authz/actions.json"
  | toJson -}}
{{- end }}

{{/*
Refuse the apiAuthz values the chart cannot honour, each of which would
otherwise render clean and mount something other than what the operator
asked for.

  - apiAuthz.configMap and apiAuthz.bundle, with any subkey. A values file
    written for them gets an error naming existingConfigMap rather than a
    render that silently runs a different bundle.
  - existingConfigMap or roleMappingsConfigMap set to anything but a
    string. `--set ...=false` or `=0` parses as a bool or number, and a list
    or map is never a name; rendering it would mount a ConfigMap called
    "false" or "0".
  - existingConfigMap naming the chart's own ConfigMap. The override path
    mounts actions.json from that ConfigMap and the other two files from
    the named one; naming the chart's own would look like an override while
    running the shipped bundle, and editing that object is undone by the
    next helm upgrade.
  - roleMappingsConfigMap naming the chart's own ConfigMap, for the same
    reason: the chart renders no roleMappings.json there, and a key added
    by hand is undone by the next helm upgrade.
  - An empty packaged file. .Files.Get returns "" for a file that is not in
    the chart, so this catches a chart packaged without its bundle. No
    values reach it.

Bundle content is not checked here. The shipped default is checked in CI
(the conformance tests, policy_fences_test.go, chart_bundle_sync_test.go),
and an operator's ConfigMap is not visible at render. loadBundle checks
either at startup and on every reload.

Called from deployment.yaml. Emits nothing.
*/}}
{{- define "controller.apiAuthz.validate" -}}
{{- $az := .Values.apiAuthz -}}
{{- if or (hasKey $az "configMap") (hasKey $az "bundle") -}}
{{- fail "apiAuthz.configMap and apiAuthz.bundle are not values of this chart. Put policies.cedar and roles.json in a ConfigMap you manage and set apiAuthz.existingConfigMap to its name, and roleMappings.json in the one apiAuthz.roleMappingsConfigMap names; or remove these keys to run the policies this chart ships." -}}
{{- end -}}
{{- if $az.enabled -}}
{{- range $key := list "existingConfigMap" "roleMappingsConfigMap" -}}
{{- $v := index $az $key -}}
{{- if not (or (kindIs "invalid" $v) (kindIs "string" $v)) -}}
{{- fail (printf "apiAuthz.%s must be a ConfigMap name, a string; got %s %v. Quote it in your values file, or pass it with --set-string." $key (kindOf $v) $v) -}}
{{- end -}}
{{- end -}}
{{- $default := include "controller.apiAuthz.defaultConfigMapName" . -}}
{{- if eq (include "controller.apiAuthz.existingConfigMap" .) $default -}}
{{- fail (printf "apiAuthz.existingConfigMap names %s, the ConfigMap this chart renders from its packaged bundle. Leave existingConfigMap empty to run that bundle, or copy it under a name of your own and set that." $default) -}}
{{- end -}}
{{- if eq (include "controller.apiAuthz.roleMappingsConfigMap" .) $default -}}
{{- fail (printf "apiAuthz.roleMappingsConfigMap names %s, the ConfigMap this chart renders from its packaged bundle, which holds no roleMappings.json. Put roleMappings.json in a ConfigMap you manage and set roleMappingsConfigMap to its name, or leave it empty to use nke-authz-role-mappings." $default) -}}
{{- end -}}
{{- range $key, $path := include "controller.apiAuthz.packagedFiles" . | fromJson -}}
{{- if not ($.Files.Get $path) -}}
{{- fail (printf "the chart's packaged %s (%s) is missing or empty, so the ConfigMap would carry an empty %s. The chart was packaged without its authorization bundle; rebuild it." $key $path $key) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
