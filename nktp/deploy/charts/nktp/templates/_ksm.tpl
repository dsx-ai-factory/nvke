{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}
{{/* API groups and scope for the built-in KSM resource collectors. */}}
{{- define "nktp.ksm.resources" -}}
certificatesigningrequests: {group: "certificates.k8s.io", scope: cluster}
clusterrolebindings: {group: "rbac.authorization.k8s.io", scope: cluster}
clusterroles: {group: "rbac.authorization.k8s.io", scope: cluster}
configmaps: {group: "", scope: namespaced}
cronjobs: {group: "batch", scope: namespaced}
daemonsets: {group: "apps", scope: namespaced}
deployments: {group: "apps", scope: namespaced}
endpoints: {group: "", scope: namespaced}
endpointslices: {group: "discovery.k8s.io", scope: namespaced}
horizontalpodautoscalers: {group: "autoscaling", scope: namespaced}
ingressclasses: {group: "networking.k8s.io", scope: cluster}
ingresses: {group: "networking.k8s.io", scope: namespaced}
jobs: {group: "batch", scope: namespaced}
leases: {group: "coordination.k8s.io", scope: namespaced}
limitranges: {group: "", scope: namespaced}
mutatingadmissionpolicies: {group: "admissionregistration.k8s.io", scope: cluster}
mutatingadmissionpolicybindings: {group: "admissionregistration.k8s.io", scope: cluster}
mutatingwebhookconfigurations: {group: "admissionregistration.k8s.io", scope: cluster}
namespaces: {group: "", scope: cluster}
networkpolicies: {group: "networking.k8s.io", scope: namespaced}
nodes: {group: "", scope: cluster}
persistentvolumeclaims: {group: "", scope: namespaced}
persistentvolumes: {group: "", scope: cluster}
poddisruptionbudgets: {group: "policy", scope: namespaced}
pods: {group: "", scope: namespaced}
replicasets: {group: "apps", scope: namespaced}
replicationcontrollers: {group: "", scope: namespaced}
resourcequotas: {group: "", scope: namespaced}
rolebindings: {group: "rbac.authorization.k8s.io", scope: namespaced}
roles: {group: "rbac.authorization.k8s.io", scope: namespaced}
secrets: {group: "", scope: namespaced}
serviceaccounts: {group: "", scope: namespaced}
services: {group: "", scope: namespaced}
statefulsets: {group: "apps", scope: namespaced}
storageclasses: {group: "storage.k8s.io", scope: cluster}
validatingadmissionpolicies: {group: "admissionregistration.k8s.io", scope: cluster}
validatingadmissionpolicybindings: {group: "admissionregistration.k8s.io", scope: cluster}
validatingwebhookconfigurations: {group: "admissionregistration.k8s.io", scope: cluster}
volumeattachments: {group: "storage.k8s.io", scope: cluster}
{{- end -}}

{{/* Normalize the collection groups for workloads, RBAC and discovery. */}}
{{- define "nktp.ksm.groups" -}}
{{- $ksm := index .Values "kube-state-metrics" -}}
{{- $groups := dict -}}
{{- if $ksm.enabled -}}
  {{- $catalog := include "nktp.ksm.resources" . | fromYaml -}}
  {{- $configured := deepCopy ($ksm.deployments | default dict) -}}
  {{- range $name, $config := $configured -}}
    {{- $_ := set $config "daemonset" false -}}
  {{- end -}}
  {{- if $ksm.daemonset.enabled -}}
    {{- if (index $configured "pods" | default dict).enabled -}}
      {{- fail "KSM deployment name pods conflicts with the enabled DaemonSet" -}}
    {{- end -}}
    {{- $_ := set $configured "pods" (mustMergeOverwrite (omit (deepCopy $ksm.daemonset) "resource_types" "track_unscheduled_pods") (dict "daemonset" true "shards" 1 "resource_types" (list "pods") "track_unscheduled_pods" false)) -}}
  {{- end -}}
  {{- range $name, $config := $configured -}}
    {{- if $config.enabled -}}
      {{- if or (gt (len $name) 40) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $name)) -}}
        {{- fail "KSM workload names must be DNS labels of at most 40 characters" -}}
      {{- end -}}
      {{- if or (not (regexMatch "^[1-9][0-9]*$" (toString $config.shards))) (gt (int $config.shards) 10000) -}}
        {{- fail (printf "KSM workload %s shards must be an integer between 1 and 10000" $name) -}}
      {{- end -}}
      {{- $types := $config.resource_types | default list -}}
      {{- if not $types -}}{{- fail (printf "KSM workload %s resource_types must not be empty" $name) -}}{{- end -}}
      {{- if and $config.track_unscheduled_pods (ne (toJson $types) (toJson (list "pods"))) -}}
        {{- fail (printf "KSM workload %s track_unscheduled_pods requires resource_types to be exactly [pods]" $name) -}}
      {{- end -}}
      {{- $namespaces := $ksm.namespaces | default list -}}
      {{- if hasKey $config "namespaces" -}}{{- $namespaces = $config.namespaces | default list -}}{{- end -}}
      {{- /* Match KSM namespace parsing before choosing namespaced or cluster-wide bindings. */ -}}
      {{- $normalizedNamespaces := list -}}
      {{- range $namespace := splitList "," (join "," $namespaces) -}}
        {{- if trim $namespace -}}{{- $normalizedNamespaces = append $normalizedNamespaces (trim $namespace) -}}{{- end -}}
      {{- end -}}
      {{- $namespaces = uniq $normalizedNamespaces -}}
      {{- $args := list (printf "--resources=%s" (join "," $types)) -}}
      {{- with $ksm.metricLabelsAllowlist -}}
        {{- $args = append $args (printf "--metric-labels-allowlist=%s" (join "," .)) -}}
      {{- end -}}
      {{- if $config.daemonset -}}{{- $args = append $args "--node=$(NODE_NAME)" -}}{{- end -}}
      {{- if $config.track_unscheduled_pods -}}{{- $args = append $args "--track-unscheduled-pods" -}}{{- end -}}
      {{- if $namespaces -}}{{- $args = append $args (printf "--namespaces=%s" (join "," $namespaces)) -}}{{- end -}}
      {{- /* Leave 20% headroom for memory outside Go and allocations between GC cycles. */ -}}
      {{- $args = concat $args (list "--auto-gomemlimit" "--auto-gomemlimit-ratio=0.8") -}}
      {{- $rules := dict "namespaced" (dict) "cluster" (dict) -}}
      {{- range $type := ($types | uniq) -}}
        {{- if not (hasKey $catalog $type) -}}{{- fail (printf "Unsupported KSM resource type %s for RBAC generation" $type) -}}{{- end -}}
        {{- $resource := index $catalog $type -}}
        {{- $scope := index $rules $resource.scope -}}
        {{- $_ := set $scope $resource.group (append (index $scope $resource.group | default list) $type) -}}
      {{- end -}}
      {{- $kubernetes := mustMergeOverwrite (deepCopy ($ksm.kubernetes | default dict)) ($config.kubernetes | default dict) -}}
      {{- $fullname := printf "nktp-ksm-%s" $name -}}
      {{- $_ := set $groups $name (dict "name" $fullname "shards" (int $config.shards) "args" $args "namespaces" $namespaces "daemonset" $config.daemonset "rules" $rules "kubernetes" $kubernetes "serviceAccount" ($kubernetes.serviceAccount | default $fullname)) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- toYaml $groups -}}
{{- end -}}

{{- define "nktp.ksm.labels" -}}
app.kubernetes.io/name: {{ .group.name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: kube-state-metrics
{{- end -}}

{{- define "nktp.ksm.roleName" -}}
{{- $name := printf "%s-%s-%s-%s" .group.name .root.Release.Namespace .root.Release.Name .scope -}}
{{- printf "%s-%s" ($name | trunc 52 | trimSuffix "-") ($name | sha256sum | trunc 10) -}}
{{- end -}}
