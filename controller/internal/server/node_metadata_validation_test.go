// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
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
