// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestValidateNodeLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		labels  map[string]string
		wantErr string
	}{
		{name: "empty"},
		{name: "bare key", labels: map[string]string{"nodegroup": "workers"}},
		{name: "empty value", labels: map[string]string{"nodegroup": ""}},
		{name: "vendor key", labels: map[string]string{"nvidia.com/gpu.product": "B300"}},
		{name: "lookalike nke domain", labels: map[string]string{"mynke.nvidia.com/rack": "a"}},
		{name: "lookalike kubernetes domain", labels: map[string]string{"notkubernetes.io/hostname": "n1"}},
		{
			name:    "kubernetes key without an allowlist entry",
			labels:  map[string]string{"node.kubernetes.io/instance-type": "h100"},
			wantErr: `nodeLabels: key "node.kubernetes.io/instance-type" is under reserved domain "kubernetes.io"; labels in this domain cannot be set`,
		},
		{
			name:    "invalid key",
			labels:  map[string]string{"bad key": "x"},
			wantErr: `nodeLabels: key "bad key" is invalid`,
		},
		{
			name:    "invalid value",
			labels:  map[string]string{"role": "not a value"},
			wantErr: `nodeLabels: value "not a value" for key "role" is invalid`,
		},
		{
			name:    "nke domain",
			labels:  map[string]string{"nke.nvidia.com/managed": "true"},
			wantErr: `nodeLabels: key "nke.nvidia.com/managed" is under reserved domain "nke.nvidia.com"; labels in this domain cannot be set`,
		},
		{
			name:    "nvke subdomain",
			labels:  map[string]string{"site.nvke.nvidia.com/rack": "a"},
			wantErr: `nodeLabels: key "site.nvke.nvidia.com/rack" is under reserved domain "nvke.nvidia.com"; labels in this domain cannot be set`,
		},
		{
			name:    "control-plane role",
			labels:  map[string]string{"node-role.kubernetes.io/control-plane": ""},
			wantErr: `nodeLabels: key "node-role.kubernetes.io/control-plane" is managed by core bootstrap components and cannot be set`,
		},
		{
			name:    "unlisted node role",
			labels:  map[string]string{"node-role.kubernetes.io/master": ""},
			wantErr: `nodeLabels: key "node-role.kubernetes.io/master" is under reserved domain "kubernetes.io"; labels in this domain cannot be set`,
		},
		{
			name:    "exact kubernetes.io",
			labels:  map[string]string{"kubernetes.io/hostname": "n1"},
			wantErr: `nodeLabels: key "kubernetes.io/hostname" is under reserved domain "kubernetes.io"; labels in this domain cannot be set`,
		},
		{
			name:    "exact k8s.io",
			labels:  map[string]string{"k8s.io/foo": "bar"},
			wantErr: `nodeLabels: key "k8s.io/foo" is under reserved domain "k8s.io"; labels in this domain cannot be set`,
		},
		{
			name:    "kubernetes.io subdomain",
			labels:  map[string]string{"topology.kubernetes.io/zone": "az1"},
			wantErr: `nodeLabels: key "topology.kubernetes.io/zone" is under reserved domain "kubernetes.io"; labels in this domain cannot be set`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNodeLabels(tt.labels, nil)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestValidateNodeLabels_ReportsFirstInvalidKeyInSortedOrder(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		"nvke.nvidia.com/managed": "true",
		"k8s.io/foo":              "bar",
		"bad key":                 "x",
	}
	// Map iteration order varies between ranges, so repeat to catch an unsorted walk.
	for range 10 {
		require.ErrorContains(t, validateNodeLabels(labels, nil), `nodeLabels: key "bad key" is invalid`)
	}
}

func TestValidateNodeLabels_ConfiguredAllowlist(t *testing.T) {
	t.Parallel()

	allowlistedLabelKeys := []string{"node.kubernetes.io/custom", "k8s.io/custom", "nke.nvidia.com/terminate", "nke.io/k8s-api-encryption-configured"}

	require.NoError(t, validateNodeLabels(map[string]string{"node.kubernetes.io/custom": ""}, allowlistedLabelKeys))
	require.NoError(t, validateNodeLabels(map[string]string{"k8s.io/custom": ""}, allowlistedLabelKeys))
	require.ErrorContains(t,
		validateNodeLabels(map[string]string{"node.kubernetes.io/instance-type": "h100"}, allowlistedLabelKeys),
		`nodeLabels: key "node.kubernetes.io/instance-type" is under reserved domain "kubernetes.io"; labels in this domain cannot be set`,
	)
	require.ErrorContains(t,
		validateNodeLabels(map[string]string{controlPlaneRoleKey: ""}, allowlistedLabelKeys),
		`nodeLabels: key "node-role.kubernetes.io/control-plane" is managed by core bootstrap components and cannot be set`,
	)
	require.ErrorContains(t,
		validateNodeLabels(map[string]string{"nke.nvidia.com/terminate": "true"}, allowlistedLabelKeys),
		`nodeLabels: key "nke.nvidia.com/terminate" is under reserved domain "nke.nvidia.com"; labels in this domain cannot be set`,
	)
	require.ErrorContains(t,
		validateNodeLabels(map[string]string{"nke.io/k8s-api-encryption-configured": "true"}, allowlistedLabelKeys),
		`nodeLabels: key "nke.io/k8s-api-encryption-configured" is under reserved domain "nke.io"; labels in this domain cannot be set`,
	)
}

func TestValidateNodeTaints(t *testing.T) {
	t.Parallel()

	valid := []api.NodeTaint{{
		Key:    "nvidia.com/dedicated",
		Value:  "team-b",
		Effect: api.NodeTaintEffectNoSchedule,
	}}

	tests := []struct {
		name         string
		taints       []api.NodeTaint
		controlPlane bool
		wantErr      string
	}{
		{name: "empty"},
		{name: "control plane with no taints", controlPlane: true},
		{name: "vendor taint", taints: valid},
		{name: "empty value", taints: []api.NodeTaint{{Key: "dedicated", Effect: api.NodeTaintEffectNoExecute}}},
		{
			name: "same key different effects",
			taints: []api.NodeTaint{
				{Key: "dedicated", Effect: api.NodeTaintEffectNoSchedule},
				{Key: "dedicated", Effect: api.NodeTaintEffectNoExecute},
			},
		},
		{
			name: "invalid key",
			taints: []api.NodeTaint{{
				Key:    "bad key",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "bad key" is invalid`,
		},
		{
			name: "control-plane role",
			taints: []api.NodeTaint{{
				Key:    "node-role.kubernetes.io/control-plane",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "node-role.kubernetes.io/control-plane" is managed by core bootstrap components and cannot be set`,
		},
		{
			name: "kubernetes.io subdomain",
			taints: []api.NodeTaint{{
				Key:    "node.kubernetes.io/instance-type",
				Value:  "h100",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "node.kubernetes.io/instance-type" is under reserved domain "kubernetes.io"; taints in this domain cannot be set`,
		},
		{
			name: "nke domain",
			taints: []api.NodeTaint{{
				Key:    "nke.nvidia.com/managed",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "nke.nvidia.com/managed" is under reserved domain "nke.nvidia.com"; taints in this domain cannot be set`,
		},
		{
			name: "k8s.io",
			taints: []api.NodeTaint{{
				Key:    "k8s.io/foo",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "k8s.io/foo" is under reserved domain "k8s.io"; taints in this domain cannot be set`,
		},
		{
			name:         "control-plane pool",
			taints:       valid,
			controlPlane: true,
			wantErr:      "nodeTaints: not allowed on a control-plane pool",
		},
		{
			name: "duplicate key and effect",
			taints: []api.NodeTaint{
				{Key: "dedicated", Effect: api.NodeTaintEffectNoSchedule},
				{Key: "dedicated", Value: "other", Effect: api.NodeTaintEffectNoSchedule},
			},
			wantErr: `nodeTaints: duplicate taint key "dedicated" with effect "NoSchedule"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNodeTaints(tt.taints, tt.controlPlane)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestValidateMachineMetadata_Inputs(t *testing.T) {
	t.Parallel()
	const sizeTestKey = "example.com/large"
	maxValueBytes := apivalidation.TotalAnnotationSizeLimitB - len(sizeTestKey)
	for key, value := range shared.MachineAnnotations(db.Node{NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true}) {
		maxValueBytes -= len(key) + len(value)
	}
	type parameters struct {
		annotations        map[string]string
		labels             map[string]string
		wantErr            string
		allowedAnnotations []string
		allowedLabels      []string
	}
	tests := map[string]parameters{
		"omitted":                                  {},
		"free-form annotations":                    {annotations: map[string]string{"nke.nvidia.com/profile-selection": "[\"gpu\", \"network\"]\n"}, allowedAnnotations: []string{"nke.nvidia.com/profile-selection"}},
		"custom labels":                            {labels: map[string]string{"example.com/pool": "gpu", "empty": ""}},
		"invalid annotation key":                   {annotations: map[string]string{"bad key": "value"}, wantErr: "machineAnnotations"},
		"annotation value too large":               {annotations: map[string]string{"example.com/large": strings.Repeat("a", 256*1024)}, wantErr: "Too long"},
		"annotations below total limit":            {annotations: map[string]string{sizeTestKey: strings.Repeat("a", maxValueBytes-1)}},
		"annotations at total limit":               {annotations: map[string]string{sizeTestKey: strings.Repeat("a", maxValueBytes)}},
		"annotations exceed total limit":           {annotations: map[string]string{sizeTestKey: strings.Repeat("a", maxValueBytes+1)}, wantErr: "Too long"},
		"caller annotations at Kubernetes limit":   {annotations: map[string]string{sizeTestKey: strings.Repeat("a", apivalidation.TotalAnnotationSizeLimitB-len(sizeTestKey))}, wantErr: "Too long"},
		"invalid label key":                        {labels: map[string]string{"bad key": "value"}, wantErr: "machineLabels"},
		"invalid label value":                      {labels: map[string]string{"pool": "not a label"}, wantErr: "machineLabels"},
		"cluster identity label":                   {labels: map[string]string{clusterv1.ClusterNameLabel: "other"}, wantErr: "must be explicitly allowlisted"},
		"control-plane role label":                 {labels: map[string]string{clusterv1.MachineControlPlaneLabel: ""}, wantErr: "must be explicitly allowlisted"},
		"annotation exception":                     {annotations: map[string]string{"nke.nvidia.com/profile": "gpu,network"}, allowedAnnotations: []string{"nke.nvidia.com/profile"}},
		"label exception":                          {labels: map[string]string{"cluster.x-k8s.io/custom": "gpu"}, allowedLabels: []string{"cluster.x-k8s.io/custom"}},
		"annotation exception cannot permit label": {labels: map[string]string{"nvke.nvidia.com/profile": "gpu"}, allowedAnnotations: []string{"nvke.nvidia.com/profile"}, wantErr: "must be explicitly allowlisted"},
		"label exception cannot permit annotation": {annotations: map[string]string{"nvke.nvidia.com/profile": "gpu"}, allowedLabels: []string{"nvke.nvidia.com/profile"}, wantErr: "must be explicitly allowlisted"},
		"exception must match exact key":           {annotations: map[string]string{"nvke.nvidia.com/other": "gpu"}, allowedAnnotations: []string{"nvke.nvidia.com/profile"}, wantErr: "must be explicitly allowlisted"},
		"lookalike domains":                        {annotations: map[string]string{"mynke.io/key": "value"}, labels: map[string]string{"notk8s.io/key": "value"}},
	}
	for _, domain := range []string{"nke.nvidia.com", "nke.io", "nvke.nvidia.com", "kubernetes.io", "k8s.io", "cluster.x-k8s.io"} {
		for _, prefix := range []string{domain, "child." + domain} {
			key := prefix + "/future-key"
			tests["reserved annotation "+key] = parameters{annotations: map[string]string{key: "value"}, wantErr: "must be explicitly allowlisted"}
			tests["reserved label "+key] = parameters{labels: map[string]string{key: "value"}, wantErr: "must be explicitly allowlisted"}
		}
	}

	// Both domains of each owned annotation: the controller still reads the
	// legacy form, so a caller setting that one would be just as damaging as
	// setting the current one.
	for _, key := range []keys.Key{
		keys.MachineHostID, keys.MachineReboot, keys.MachineRebootCompleteTimestamp,
		keys.MachineHealthIssue, keys.MachineSpireAgentSpiffeIDPath,
	} {
		for _, name := range key.All() {
			tests["reserved annotation "+name] = parameters{annotations: map[string]string{name: "value"}, wantErr: "must be explicitly allowlisted"}
		}
	}
	for _, key := range []string{
		clusterv1.ExcludeNodeDrainingAnnotation,
		clusterv1.ExcludeWaitForNodeVolumeDetachAnnotation,
	} {
		tests["reserved annotation "+key] = parameters{annotations: map[string]string{key: "value"}, wantErr: "must be explicitly allowlisted"}
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateMachineMetadata(params.annotations, params.labels, params.allowedAnnotations, params.allowedLabels)
			if params.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, params.wantErr)
			}
		})
	}
}

func TestMachineMetadata_ConnectionContractReserved(t *testing.T) {
	for _, key := range keys.MachineAgentConnection.All() {
		err := validateMachineMetadata(map[string]string{key: "forged"}, nil, []string{key}, nil)
		require.ErrorContains(t, err, "controller-owned")
	}
}
