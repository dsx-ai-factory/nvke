{{/*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/}}
{{- define "nvke-crds.retention" -}}
helm.sh/resource-policy: keep
argocd.argoproj.io/sync-wave: "-2"
argocd.argoproj.io/sync-options: Prune=false
{{- end -}}
