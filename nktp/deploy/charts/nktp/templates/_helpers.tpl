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
