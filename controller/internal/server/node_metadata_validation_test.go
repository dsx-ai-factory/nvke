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
		{name: "empty value", labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
		{name: "vendor key", labels: map[string]string{"nvidia.com/gpu.product": "B300"}},
		{name: "lookalike nke domain", labels: map[string]string{"mynke.nvidia.com/rack": "a"}},
		{name: "lookalike kubernetes domain", labels: map[string]string{"notkubernetes.io/hostname": "n1"}},
		{name: "allowlisted label key from reserved domain", labels: map[string]string{"node.kubernetes.io/instance-type": "h100"}},
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
			wantErr: `nodeLabels: key "nke.nvidia.com/managed" is reserved`,
		},
		{
			name:    "nke subdomain",
			labels:  map[string]string{"site.nke.io/rack": "a"},
			wantErr: `nodeLabels: key "site.nke.io/rack" is reserved`,
		},
		{
			name:    "control-plane role",
			labels:  map[string]string{"node-role.kubernetes.io/control-plane": ""},
			wantErr: `nodeLabels: key "node-role.kubernetes.io/control-plane" is forbidden`,
		},
		{
			name:    "unlisted node role",
			labels:  map[string]string{"node-role.kubernetes.io/master": ""},
			wantErr: `nodeLabels: key "node-role.kubernetes.io/master" is reserved`,
		},
		{
			name:    "exact kubernetes.io",
			labels:  map[string]string{"kubernetes.io/hostname": "n1"},
			wantErr: `nodeLabels: key "kubernetes.io/hostname" is reserved`,
		},
		{
			name:    "exact k8s.io",
			labels:  map[string]string{"k8s.io/foo": "bar"},
			wantErr: `nodeLabels: key "k8s.io/foo" is reserved`,
		},
		{
			name:    "kubernetes.io subdomain",
			labels:  map[string]string{"topology.kubernetes.io/zone": "az1"},
			wantErr: `nodeLabels: key "topology.kubernetes.io/zone" is reserved`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateNodeLabels(tt.labels)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
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
			name: "control-plane role",
			taints: []api.NodeTaint{{
				Key:    "node-role.kubernetes.io/control-plane",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "node-role.kubernetes.io/control-plane" is forbidden`,
		},
		{
			name: "allowlisted label keys do not allow taints",
			taints: []api.NodeTaint{{
				Key:    "node.kubernetes.io/instance-type",
				Value:  "h100",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "node.kubernetes.io/instance-type" is reserved`,
		},
		{
			name: "nke domain",
			taints: []api.NodeTaint{{
				Key:    "nke.nvidia.com/managed",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "nke.nvidia.com/managed" is reserved`,
		},
		{
			name: "k8s.io",
			taints: []api.NodeTaint{{
				Key:    "k8s.io/foo",
				Effect: api.NodeTaintEffectNoSchedule,
			}},
			wantErr: `nodeTaints: key "k8s.io/foo" is reserved`,
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
