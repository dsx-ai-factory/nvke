{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{- define "nico-prereqs.database.password" -}}
{{- required "database.password is required. Set it with --set database.password=... or supply database.existingSecret." .Values.database.password -}}
{{- end -}}

{{/*
The Secret nico-api reads its database credentials from.

../nico names the same Secret in nico.nico-api.envFrom.databaseCredentials.secretName.
The two must agree: nothing checks it, and the symptom is a migration Job that
cannot start.
*/}}
{{- define "nico-prereqs.database.secretName" -}}
{{- .Values.database.secretName -}}
{{- end -}}

{{- define "nico-prereqs.database.uri" -}}
{{- $d := .Values.database -}}
{{- printf "postgres://%s:%s@%s:%v/%s?sslmode=%s" $d.user (include "nico-prereqs.database.password" . | urlquery) $d.host $d.port $d.dbName $d.sslMode -}}
{{- end -}}
