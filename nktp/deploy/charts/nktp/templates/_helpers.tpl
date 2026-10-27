{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}
{{/*
Resolve an image map ({registry, repository, tag}) into an
OpenTelemetryCollector spec.image / container image string, honoring
global.imageRegistry.

When global.imageRegistry is set, it replaces the image's own registry.
The tag must be a string: YAML reads an unquoted 1.0 as a number.
*/}}
{{- define "nktp.image" -}}
{{- if not (kindIs "string" .image.tag) -}}
{{- fail (printf "image tag %v for %s must be a quoted string" .image.tag .image.repository) -}}
{{- end -}}
{{- $registry := .image.registry -}}
{{- if .imageRegistry -}}
{{- $registry = .imageRegistry -}}
{{- end -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry .image.repository .image.tag -}}
{{- else -}}
{{- printf "%s:%s" .image.repository .image.tag -}}
{{- end -}}
{{- end -}}

{{/*
Return a workload's Kubernetes values with its sidecar Collector appended to
initContainers. restartPolicy Always makes it a native sidecar: it starts before
the main Collector and stops after it.

Operator injection cannot add this sidecar. The Operator skips injection when a
Pod has a container named otc-container, which is the name it gives every
Collector's main container. See docs/nktp/architecture/self-telemetry.md.

The sidecar configuration is passed unchanged, as JSON, to --config=yaml:.
Environment variables are added to the sidecar's env when the list omits them,
as daemonset.yaml does for the node Collector. Every sidecar configuration uses
POD_NAME, so it is always added. Workloads inherit priorityClass.name unless
kubernetes.priorityClassName is explicitly set; an empty string opts out.

Arguments: name (values key), values (workload values), root (chart root),
sidecarEnv (optional, more env entries to add to the sidecar when missing),
mainEnv (optional, env entries to add to the main Collector when missing).
*/}}
{{- define "nktp.withSidecar" -}}
{{- $sidecar := .values.sidecar | default dict -}}
{{- $otelcol := $sidecar.otelcol | default dict -}}
{{- if not (kindIs "map" $otelcol.config) -}}
{{- fail (printf "%s.sidecar.otelcol.config must be a native Collector YAML map" .name) -}}
{{- end -}}
{{- $image := include "nktp.image" (dict "imageRegistry" .root.Values.global.imageRegistry "image" ($otelcol.image | default .root.Values.otelcol_image)) -}}
{{- $container := mustMergeOverwrite (deepCopy ($sidecar.kubernetes | default dict)) (dict "name" "otc-sidecar" "image" $image "restartPolicy" "Always" "args" (list (printf "--config=yaml:%s" (toJson $otelcol.config)))) -}}
{{- $podName := dict "name" "POD_NAME" "valueFrom" (dict "fieldRef" (dict "fieldPath" "metadata.name")) -}}
{{- $sidecarEnv := concat (.sidecarEnv | default list) (list $podName) -}}
{{- $_ := set $container "env" (include "nktp.addEnv" (dict "env" $container.env "add" $sidecarEnv) | fromYaml).env -}}
{{- $workload := deepCopy .values.kubernetes -}}
{{- if not (hasKey $workload "priorityClassName") -}}
{{- $_ := set $workload "priorityClassName" .root.Values.priorityClass.name -}}
{{- end -}}
{{- if .mainEnv -}}
{{- $_ := set $workload "env" (include "nktp.addEnv" (dict "env" $workload.env "add" .mainEnv) | fromYaml).env -}}
{{- end -}}
{{- $_ := set $workload "initContainers" (append ($workload.initContainers | default list) $container) -}}
{{- toYaml $workload -}}
{{- end -}}

{{/*
Return {env: <list>}: env with each entry of add appended when env has no entry
with the same name. A user's own entry always wins.

Arguments: env (list, may be empty), add (list of env entries).
*/}}
{{- define "nktp.addEnv" -}}
{{- $env := .env | default list -}}
{{- range $entry := .add -}}
  {{- $found := false -}}
  {{- range $env -}}
    {{- if eq .name $entry.name -}}{{- $found = true -}}{{- end -}}
  {{- end -}}
  {{- if not $found -}}
    {{- $env = append $env $entry -}}
  {{- end -}}
{{- end -}}
{{- toYaml (dict "env" $env) -}}
{{- end -}}

{{/*
Return {env: <list>} with K8S_CLUSTER_NAME set to global.clusterName, or an
empty list when it is not set. The transform/cluster processor in the gateways,
the watchdog and the watchdog sidecar reads this variable inside an OTTL string,
so the name must not contain a double quote or a backslash.
*/}}
{{- define "nktp.clusterNameEnv" -}}
{{- $env := list -}}
{{- $raw := .Values.global.clusterName -}}
{{- /* Only a missing or empty value is unset. 0 and false are names, not "unset". */}}
{{- $name := ternary "" (toString $raw) (kindIs "invalid" $raw) -}}
{{- if ne $name "" -}}
{{- if or (contains "\"" $name) (contains "\\" $name) -}}
{{- fail "global.clusterName must not contain double quotes or backslashes" -}}
{{- end -}}
{{- $env = list (dict "name" "K8S_CLUSTER_NAME" "value" $name) -}}
{{- end -}}
{{- toYaml (dict "env" $env) -}}
{{- end -}}
