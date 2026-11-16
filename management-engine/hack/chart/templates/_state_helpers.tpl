{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{/* State namespace is an existing namespace for controller-only installations. */}}
{{- define "management-engine.stateNamespace" -}}
{{- $namespace := coalesce .Values.manager.stateNamespace .Values.state.namespace .Release.Namespace -}}
{{- range .Values.manager.env -}}
{{- if eq .name "STATE_NAMESPACE" -}}{{- $namespace = .value -}}{{- end -}}
{{- end -}}
{{- $namespace -}}
{{- end -}}

{{/* Select canonical rules from a role or mixed development RBAC bundle. */}}
{{- define "management-engine.roleRules" -}}
{{- $rules := list -}}
{{- range splitList "\n---\n" (.root.Files.Get (printf "files/%s.yaml" .file)) -}}
{{- $role := fromYaml . -}}
{{- if eq $role.kind $.kind -}}{{- $rules = $role.rules -}}{{- end -}}
{{- end -}}
{{- required (printf "missing %s rules in %s" .kind .file) $rules | toYaml -}}
{{- end -}}
