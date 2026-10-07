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
Environment variables in env (name to fieldRef path) are added to the sidecar's
env when the list omits them, as daemonset.yaml does for the node Collector.
Every sidecar configuration uses POD_NAME, so it is always added.

Arguments: name (values key), values (workload values), root (chart root),
env (optional, more variables to add when missing).
*/}}
{{- define "nktp.withSidecar" -}}
{{- $sidecar := .values.sidecar | default dict -}}
{{- $otelcol := $sidecar.otelcol | default dict -}}
{{- if not (kindIs "map" $otelcol.config) -}}
{{- fail (printf "%s.sidecar.otelcol.config must be a native Collector YAML map" .name) -}}
{{- end -}}
{{- $image := include "nktp.image" (dict "imageRegistry" .root.Values.global.imageRegistry "image" ($otelcol.image | default .root.Values.otelcol_image)) -}}
{{- $container := mustMergeOverwrite (deepCopy ($sidecar.kubernetes | default dict)) (dict "name" "otc-sidecar" "image" $image "restartPolicy" "Always" "args" (list (printf "--config=yaml:%s" (toJson $otelcol.config)))) -}}
{{- $env := $container.env | default list -}}
{{- range $name, $field := merge (dict "POD_NAME" "metadata.name") (.env | default dict) -}}
  {{- $found := false -}}
  {{- range $env -}}
    {{- if eq .name $name -}}{{- $found = true -}}{{- end -}}
  {{- end -}}
  {{- if not $found -}}
    {{- $env = append $env (dict "name" $name "valueFrom" (dict "fieldRef" (dict "fieldPath" $field))) -}}
  {{- end -}}
{{- end -}}
{{- $_ := set $container "env" $env -}}
{{- $workload := deepCopy .values.kubernetes -}}
{{- $_ := set $workload "initContainers" (append ($workload.initContainers | default list) $container) -}}
{{- toYaml $workload -}}
{{- end -}}
