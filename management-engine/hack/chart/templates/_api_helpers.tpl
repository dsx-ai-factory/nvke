{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{- define "management-engine.apiServiceAccount" -}}
{{- default (include "management-engine.resourceName" (dict "suffix" (printf "%s-api" .surface) "context" .root)) .api.serviceAccount.name -}}
{{- end -}}

{{- define "management-engine.apiArgs" -}}
- --listen=:8080
{{ if eq .surface "tenant" -}}
- {{ printf "--namespace=%s" .api.namespace | quote }}
{{ end -}}
{{ if eq .api.stateAccess.mode "remote" -}}
- --kubeconfig=/etc/cluster-service/kubeconfig
{{ with .api.stateAccess.context -}}
- {{ printf "--context=%s" . | quote }}
{{ end -}}
{{ end -}}
{{ range .api.args -}}
- {{ . | quote }}
{{ end -}}
{{- end -}}

{{- define "management-engine.apiMount" -}}
{{- if eq .stateAccess.mode "remote" }}
- name: api-datastore
  mountPath: /etc/cluster-service
  readOnly: true
{{- end }}
{{- end -}}

{{- define "management-engine.apiVolume" -}}
{{- if eq .stateAccess.mode "remote" }}
- name: api-datastore
  secret:
    secretName: {{ .stateAccess.kubeconfigSecret.name | quote }}
    defaultMode: 292
    items:
    - key: {{ .stateAccess.kubeconfigSecret.key | quote }}
      path: kubeconfig
{{- end }}
{{- end -}}
