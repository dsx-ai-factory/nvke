// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestSecurityGroups_SnapshotRecoveryAndRendering(t *testing.T) {
	for name, tt := range map[string]struct {
		groups db.LaunchLayerSecurityGroups
		refs   db.LaunchLayerSecurityGroupRefs
	}{
		"omitted":          {},
		"managed":          {refs: db.LaunchLayerSecurityGroupRefs{PrimaryNetworkInterface: []string{"managed-primary"}, GPUNetworkInterface: []string{"managed-gpu"}}},
		"external":         {groups: db.LaunchLayerSecurityGroups{PrimaryNetworkInterface: []string{"external-primary"}, GPUNetworkInterface: []string{"external-gpu"}}},
		"mixed interfaces": {groups: db.LaunchLayerSecurityGroups{GPUNetworkInterface: []string{"external-gpu"}}, refs: db.LaunchLayerSecurityGroupRefs{PrimaryNetworkInterface: []string{"managed-primary"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var snapshot db.ControlPlaneConfigSpec
			snapshot.SetLaunchLayer(db.LaunchLayerControlPlaneProvider{InstanceType: "bare-metal", HostOSProfile: "example-os", SecurityGroupRefs: tt.refs, SecurityGroups: tt.groups})
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			var recovered db.ControlPlaneConfigSpec
			require.NoError(t, json.Unmarshal(raw, &recovered))
			var nodeSet db.NodeSet
			var nodePool db.NodePool
			New().ApplyControlPlaneConfigPayloads(db.Node{}, recovered, &nodeSet, &nodePool)
			require.Equal(t, tt.groups, nodePool.LaunchLayer.SecurityGroups)
			require.Equal(t, tt.refs, nodePool.LaunchLayer.SecurityGroupRefs)
			require.Equal(t, nodePool.LaunchLayer, nodeSet.LaunchLayer)
			cluster := db.Cluster{Name: "example-cluster"}
			cluster.SetLaunchLayerProvider(db.LaunchLayerClusterProvider{VpcID: "existing-vpc", SiteID: "example-site"})
			cluster.ControlPlaneConfig.ControlPlaneConfigSpec = recovered
			for _, controlPlane := range []bool{true, false} {
				machine, err := buildInfraMachine(db.Node{Name: "example-node"}, nodeSet, cluster, controlPlane)
				require.NoError(t, err)
				for field, interfaces := range map[string]map[string][]string{
					"securityGroups":    {"primaryNetworkInterface": tt.groups.PrimaryNetworkInterface, "gpuNetworkInterface": tt.groups.GPUNetworkInterface},
					"securityGroupRefs": {"primaryNetworkInterface": tt.refs.PrimaryNetworkInterface, "gpuNetworkInterface": tt.refs.GPUNetworkInterface},
				} {
					for iface, expected := range interfaces {
						actual, found, err := unstructured.NestedStringSlice(machine.Object, "spec", field, iface)
						require.NoError(t, err)
						require.Equal(t, len(expected) > 0, found)
						require.Equal(t, expected, actual)
					}
				}
			}
			// External IDs belong only to machine attachments. Cluster securityGroups
			// are CAPLL-managed creation/deletion targets and must remain absent.
			scaffold, err := New().GetClusterScaffoldManifest(cluster)
			require.NoError(t, err)
			var manifest map[string]any
			require.NoError(t, json.Unmarshal(scaffold, &manifest))
			_, found, err := unstructured.NestedFieldNoCopy(manifest, "spec", "securityGroups")
			require.NoError(t, err)
			require.False(t, found)
			if len(tt.groups.GPUNetworkInterface) > 0 {
				nodeSet.LaunchLayer.SecurityGroups.GPUNetworkInterface[0] = "mutated"
				require.Equal(t, "external-gpu", nodePool.LaunchLayer.SecurityGroups.GPUNetworkInterface[0])
				require.Equal(t, "external-gpu", recovered.LaunchLayer.SecurityGroups.GPUNetworkInterface[0])
			}
		})
	}
}

func TestSecurityGroups_RecoveryRejectsInvalidAttachmentsBeforeApplying(t *testing.T) {
	for _, scenario := range []string{"primary conflict", "gpu conflict", "empty ID"} {
		t.Run(scenario, func(t *testing.T) {
			provider := db.LaunchLayerNodePoolProvider{InstanceType: "bare-metal"}
			wantError := "mutually exclusive"
			switch scenario {
			case "primary conflict":
				provider.SecurityGroups.PrimaryNetworkInterface = []string{"external-primary"}
				provider.SecurityGroupRefs.PrimaryNetworkInterface = []string{"managed-primary"}
			case "gpu conflict":
				provider.SecurityGroups.GPUNetworkInterface = []string{"external-gpu"}
				provider.SecurityGroupRefs.GPUNetworkInterface = []string{"managed-gpu"}
			case "empty ID":
				provider.SecurityGroups.PrimaryNetworkInterface = []string{""}
				wantError = "must not contain empty IDs"
			}
			var nodeSet db.NodeSet
			nodeSet.SetLaunchLayer(provider)
			deps := testDeps(config.Config{}, "example.org", "bundle")
			deps.K8sClient = fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					t.Fatal("invalid attachments must be rejected before applying objects")
					return nil
				},
			}).Build()
			err := New().CreateMachine(t.Context(), deps, db.Node{}, nodeSet, db.NodePool{}, db.Cluster{})
			require.ErrorIs(t, err, shared.ErrUnsupportedMachineConfig)
			require.ErrorContains(t, err, wantError)
		})
	}
}
