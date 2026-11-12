{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{ define "management-engine.managerArgs" }}
        - --leader-election-namespace=$(STATE_NAMESPACE)
        - {{ printf "--nvke-controller-endpoint=%s" .Values.manager.nvkeController.endpoint | quote }}
        {{- if .Values.manager.nvkeController.tokenSecret.name }}
        - --nvke-controller-token-file=/etc/nvke-controller/auth/token
        {{- end }}
        {{- if eq .Values.manager.stateAccess.mode "remote" }}
        - --kubeconfig=/etc/management-api/kubeconfig
        {{- end }}
        {{- if .Values.manager.infrastructureTemplateConfigMap }}
        - {{ printf "--infra-template-configmap=%s" .Values.manager.infrastructureTemplateConfigMap | quote }}
        {{- end }}
        {{- range .Values.manager.args }}
        - {{ . | quote }}
        {{- end }}
{{ end }}

{{ define "management-engine.managerEnv" }}
        - name: STATE_NAMESPACE
          value: {{ include "management-engine.stateNamespace" . | quote }}
        {{- range .Values.manager.env }}
        {{- if ne .name "STATE_NAMESPACE" }}
        - {{ toYaml . | nindent 10 | trim }}
        {{- end }}
        {{- end }}
        {{- range $k, $v := .Values.manager.envOverrides }}
        - name: {{ $k }}
          value: {{ $v | quote }}
        {{- end }}
{{ end }}

{{ define "management-engine.managedMounts" }}
        {{- if eq .Values.manager.stateAccess.mode "remote" }}
        - mountPath: /etc/management-api
          name: management-api
          readOnly: true
        {{- end }}
        {{- if .Values.manager.nvkeController.tokenSecret.name }}
        - mountPath: /etc/nvke-controller/auth
          name: nvke-controller-token
          readOnly: true
        {{- end }}
{{ end }}

{{ define "management-engine.managedVolumes" }}
      {{- if eq .Values.manager.stateAccess.mode "remote" }}
      - name: management-api
        secret:
          defaultMode: 292
          secretName: {{ .Values.manager.stateAccess.kubeconfigSecret.name | quote }}
          items:
          - key: {{ .Values.manager.stateAccess.kubeconfigSecret.key | quote }}
            path: kubeconfig
      {{- end }}
      {{- if .Values.manager.nvkeController.tokenSecret.name }}
      - name: nvke-controller-token
        secret:
          secretName: {{ .Values.manager.nvkeController.tokenSecret.name | quote }}
          defaultMode: 292
          items:
          - key: {{ .Values.manager.nvkeController.tokenSecret.key | quote }}
            path: token
      {{- end }}
{{ end }}
