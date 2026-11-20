{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{/*
The database password. Required unless database.existingSecret points at a
Secret created out of band, in which case this chart writes no Secret at all.
*/}}
{{- define "nvke.database.password" -}}
{{- required "database.password is required. Set it with --set database.password=... or supply database.existingSecret." .Values.database.password -}}
{{- end -}}

{{/*
The connection string the NVKE controller reads. Built here rather than by the
controller chart, so that one password reaches PostgreSQL, both of Temporal's
datastores and the controller without being written three times.
*/}}
{{- define "nvke.database.connectionString" -}}
{{- $d := .Values.database -}}
{{- printf "postgres://%s:%s@%s:%v/%s?sslmode=%s" $d.user (include "nvke.database.password" . | urlquery) $d.host $d.port $d.dbName $d.sslMode -}}
{{- end -}}

{{/*
The namespaces the database Secret is written into: wherever PostgreSQL,
Temporal and the controller each read it. A Secret is namespace-scoped and
nothing copies it across.
*/}}
{{- define "nvke.database.namespaces" -}}
{{- $ns := list .Release.Namespace .Values.namespaces.controller -}}
{{- if .Values.postgresql.enabled -}}
{{- $ns = append $ns .Values.namespaces.postgres -}}
{{- end -}}
{{- $ns | uniq | toJson -}}
{{- end -}}
