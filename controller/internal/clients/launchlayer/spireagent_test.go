// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_SpireAgentPath(t *testing.T) {
	t.Parallel()

	dsxIMDS := db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS}
	tests := map[string]struct {
		node        db.Node
		instanceID  string
		orgID       string
		wantPath    string
		wantPending bool
	}{
		"builds the dsx_imds path from the organization and instance ID": {
			node: dsxIMDS, instanceID: "instance-123", orgID: "org-1",
			wantPath: "/spire/agent/dsx_imds/tenant/org-1/bm/instance-123",
		},
		"supplies no path for another attestation method": {
			node: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM}, instanceID: "instance-123", orgID: "org-1",
		},
		"supplies no path without SPIFFE": {
			node: db.Node{SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS}, instanceID: "instance-123", orgID: "org-1",
		},
		"pends until CAPLL records the instance ID": {
			node: dsxIMDS, orgID: "org-1", wantPending: true,
		},
		"pends until CAPLL records the organization": {
			node: dsxIMDS, instanceID: "instance-123", wantPending: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "cluster-test"},
				Spec: clusterv1.MachineSpec{
					ClusterName: "tenant",
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: infrastructureGroup, Kind: "LaunchLayerMachine", Name: "worker-0",
					},
				},
			}
			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "cluster-test"},
				Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrastructureGroup, Kind: "LaunchLayerCluster", Name: "tenant-infra",
				}},
			}
			k8sClient := newSpireAgentPathTestClient(t, machine, cluster,
				capllObject("LaunchLayerMachine", "worker-0", map[string]any{"instanceID": tt.instanceID}),
				capllObject("LaunchLayerCluster", "tenant-infra", map[string]any{"vpc": map[string]any{"orgID": tt.orgID}}),
			)

			path, err := New().SpireAgentPath(t.Context(), k8sClient, tt.node, machine)

			switch {
			case tt.wantPending:
				require.ErrorIs(t, err, shared.ErrSpireAgentPathPending)
			default:
				require.NoError(t, err)
				require.Equal(t, tt.wantPath, path)
			}
		})
	}
}

func capllObject(kind, name string, status map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"status": status}}
	obj.SetAPIVersion(infrastructureAPIVersion)
	obj.SetKind(kind)
	obj.SetNamespace("cluster-test")
	obj.SetName(name)
	return obj
}

func newSpireAgentPathTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
