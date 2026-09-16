{{/*
Expand the name of the chart.
*/}}

{{- define "controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
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
Refuse the ways apiAuth's values can disagree with each other or with the chart,
each of which otherwise renders clean and does the wrong thing silently.

Called from deployment.yaml rather than from configmap-auth.yaml, because that
template is itself gated on create and so cannot see a config set without it.

Emits nothing. The shallow presence checks on audiences and issuers stop here:
every other rule belongs to internal/auth/config.go, whose errors name the field
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
{{- fail "apiAuth.config must not carry apiVersion or kind. The chart supplies both, so a copy here renders them twice, and auth.Load refuses duplicate keys -- CWE would crash-loop rather than start. Keep audiences and issuers only. If you are pasting the file from the README, drop its first two lines." -}}
{{- end -}}
{{- if not $auth.config.audiences -}}
{{- fail "apiAuth.config.audiences is required and must not be empty. The audience is per-install and has no default; a token minted for one install must fail at another." -}}
{{- end -}}
{{- if not $auth.config.issuers -}}
{{- fail "apiAuth.config.issuers is required and must contain at least one issuer. NKE refuses to start with none, so this would be a crash loop rather than a permissive deployment." -}}
{{- end -}}
{{- $dir := printf "%s/" (include "controller.apiAuth.mountPath" .) -}}
{{- $configFile := include "controller.apiAuth.configFile" . -}}
{{- $extra := default dict $auth.configMap.extraFiles -}}
{{- if hasKey $extra $configFile -}}
{{- fail (printf "apiAuth.configMap.extraFiles must not contain %q: that key is the configuration file itself, and a second copy of it silently replaces the one rendered from apiAuth.config -- NKE would load whatever is here instead. Put the configuration under apiAuth.config, and use extraFiles only for files it refers to by path." $configFile) -}}
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
The chart-packaged named presets apiAuthz.policyMode can select
(design-authz-chart-validation.md §4). Each preset's policies.cedar/roles.json
live under files/authz/presets/<mode>/ so `controller.apiAuthz.effectiveBundle`
and `controller.apiAuthz.validate` can both name the same set without
duplicating the list.

"readOnly"/"standard" are an implementation placeholder: functionally correct
today, but the exact set/naming of presets is a deferred product/UX decision
(design-authz-chart-validation.md §4, ux-nke-authz.md §3), not a settled
product surface.
*/}}
{{- define "controller.apiAuthz.presets" -}}
{{- list "readOnly" "standard" | toJson -}}
{{- end }}

{{/*
Resolve the four bundle parts once, in one place, per the effective-bundle rule
in design-authz-chart-validation.md §4:

  effective(policies|roles) = bundle.<part> if set and non-empty,
                               else the packaged policyMode preset if it resolves,
                               else nothing (required; the validator says so by name)

  effective(actions|schema) = bundle.<part> if set and non-empty,
                               else the chart-packaged generated default

Both templates/configmap-authz.yaml (what renders the ConfigMap) and
controller.apiAuthz.validate (what checks it) call this helper, so the two
cannot drift into checking different content than what actually gets mounted.

Emits one JSON object; callers do `include "controller.apiAuthz.effectiveBundle" . | fromJson`.
policies is a string; roles/actions are lists; schema is an object. Any part
with no resolvable source comes back as its zero value ("" / [] / {}) rather
than failing here -- required-part-present checking is controller.apiAuthz.validate's
job, not this helper's, so a caller that only wants the ConfigMap's happy-path
content (already validated elsewhere) does not have to thread failures through.
*/}}
{{- define "controller.apiAuthz.effectiveBundle" -}}
{{- $az := .Values.apiAuthz -}}
{{- $mode := $az.policyMode -}}
{{- $presets := include "controller.apiAuthz.presets" . | fromJsonArray -}}
{{- $modeResolved := and $mode (has $mode $presets) -}}
{{- $presetDir := "" -}}
{{- if $modeResolved -}}
{{- $presetDir = printf "files/authz/presets/%s" $mode -}}
{{- end -}}
{{- $policies := $az.bundle.policies -}}
{{- if and (not $policies) $modeResolved -}}
{{- $policies = .Files.Get (printf "%s/policies.cedar" $presetDir) -}}
{{- end -}}
{{- $roles := $az.bundle.roles -}}
{{- if and (not $roles) $modeResolved -}}
{{- $roles = .Files.Get (printf "%s/roles.json" $presetDir) | fromJsonArray -}}
{{- end -}}
{{- $actions := $az.bundle.actions -}}
{{- if not $actions -}}
{{- $actions = .Files.Get "files/authz/actions.json" | fromJsonArray -}}
{{- end -}}
{{- $schema := $az.bundle.schema -}}
{{- if not $schema -}}
{{- $schema = .Files.Get "files/authz/schema.json" | fromJson -}}
{{- end -}}
{{- dict "policies" $policies "roles" $roles "actions" $actions "schema" $schema "policyModeResolved" $modeResolved | toJson -}}
{{- end }}

{{/*
Refuse the ways apiAuthz's values can disagree with each other or with the
chart, each of which otherwise renders clean and does the wrong thing silently
-- design-authz-chart-validation.md §5.

Gated on apiAuthz.enabled && apiAuthz.configMap.create, never on
apiAuthz.enabled alone: configMap.create=false is the bring-your-own-ConfigMap
path, where apiAuthz.bundle/apiAuthz.policyMode are not set and not meant to
be, and a validator gated only on apiAuthz.enabled would fail every install
using an externally-managed ConfigMap for values nobody was asked to fill in.
Mirrors controller.apiAuth.validate's own $create := and $auth.enabled
$auth.configMap.create (_helpers.tpl, above).

Called from deployment.yaml rather than from configmap-authz.yaml, because
that template is itself gated on create and so cannot see a bundle set
without it -- same reason controller.apiAuth.validate is called from
deployment.yaml.

Scope, per §5: values-plumbing coherence and the two required-bundle-part
checks, plus the one approximate structural check (the empty-policy regex)
Codex's boundary named as acceptable to keep in Helm. Vocabulary,
reachability, duplicate-UID and entity-presence checks stay out of Helm
entirely -- they belong to authz.ValidateBundle (internal/authz), which is
Go's canonical validation API, not a second implementation here.

Emits nothing.
*/}}
{{- define "controller.apiAuthz.validate" -}}
{{- $az := .Values.apiAuthz -}}
{{- $create := and $az.enabled $az.configMap.create -}}
{{- if and (not $create) (or $az.bundle.policies $az.bundle.roles $az.bundle.actions $az.bundle.schema $az.policyMode) -}}
{{- fail "apiAuthz.bundle / apiAuthz.policyMode is set but would not be rendered: they are only used when apiAuthz.enabled and apiAuthz.configMap.create are both true. Set both, or remove apiAuthz.bundle/apiAuthz.policyMode and manage the ConfigMap named by apiAuthz.configMap.name yourself." -}}
{{- end -}}
{{- if $create -}}
{{- $bundle := include "controller.apiAuthz.effectiveBundle" . | fromJson -}}
{{- if and $az.policyMode (not $bundle.policyModeResolved) -}}
{{- $presets := include "controller.apiAuthz.presets" . | fromJsonArray -}}
{{- fail (printf "apiAuthz.policyMode %q is not a packaged preset. NKE ships: %s. Set apiAuthz.policyMode to one of these, supply apiAuthz.bundle.policies/apiAuthz.bundle.roles directly, or leave policyMode unset and set apiAuthz.configMap.create=false to manage the ConfigMap yourself." $az.policyMode (join ", " $presets)) -}}
{{- end -}}
{{- if not $bundle.policies -}}
{{- fail "apiAuthz.configMap.create is true but apiAuthz.bundle.policies is empty and apiAuthz.policyMode is unset. policies.cedar has no chart-packaged default outside the named presets -- NewLocal (internal/authz/local.go:54-57) would refuse to start on a bundle missing it. Set apiAuthz.bundle.policies, set apiAuthz.policyMode to one of the packaged presets, or set apiAuthz.configMap.create=false and manage the ConfigMap yourself." -}}
{{- end -}}
{{- if not $bundle.roles -}}
{{- fail "apiAuthz.bundle.roles is empty and apiAuthz.policyMode is unset. NewLocal (internal/authz/local.go:266) refuses to start on a roles.json with no NKE::Role entities for the same reason: every request would be refused, which looks identical at request time to a policy nobody wrote. Add at least one role, set apiAuthz.policyMode to a packaged preset, or set apiAuthz.enabled=false if authorization isn't ready yet." -}}
{{- end -}}
{{- if and $bundle.policies (not (regexMatch "(?m)^(permit|forbid)\\(" $bundle.policies)) -}}
{{- fail "apiAuthz.bundle.policies (or the packaged apiAuthz.policyMode preset) does not appear to contain any permit/forbid rule. This is an approximate structural check, not a real Cedar parse (design-authz-chart-validation.md §5) -- NewLocal re-parses for real at pod startup either way. Set apiAuthz.bundle.policies to a document containing at least one policy, or check that the selected apiAuthz.policyMode preset is not empty." -}}
{{- end -}}
{{- end -}}
{{- end }}
