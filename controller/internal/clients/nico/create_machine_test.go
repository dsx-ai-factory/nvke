// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"testing"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_CreateMachine_MissingRelease(t *testing.T) {
	nodeSet := db.NodeSet{KubernetesVersion: "1.32.1"}
	nodeSet.SetNico(db.NicoNodePoolProvider{Release: "test-os", DiskSelectionStrategy: db.DiskSelectionStrategySmallest})
	nodePool := db.NodePool{}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: uuid.MustParse("70000000-0000-0000-0000-000000000005"),
		VpcPrefixIDs:   []uuid.UUID{uuid.MustParse("70000000-0000-0000-0000-000000000007")},
	})
	cluster := db.Cluster{Name: "cluster-a", CniPlugin: db.CNICalico}
	cluster.SetNicoProvider(db.NicoClusterProvider{VpcID: uuid.MustParse("70000000-0000-0000-0000-000000000006")})

	k8sClient := newMachineCreateClient(t)
	err := New().CreateMachine(
		context.Background(),
		testMachineCreateDeps(k8sClient),
		db.Node{Name: "worker-0", Nico: &db.NicoNodeProvider{}},
		nodeSet,
		nodePool,
		cluster,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `NodePoolRelease "test-os" is not installed`)
}

// A control-plane node reads a ClusterRelease and a worker a NodePoolRelease,
// with no fallback between them, so a release of the right name but the wrong
// kind is not found at all.
func TestClient_CreateMachine_ResolvesByRole(t *testing.T) {
	const name = "shared-os"
	controlPlane := testClusterRelease(name, "#!ipxe\n# control-plane image {{ .DiskSelectionStrategy }}\n", "#cloud-config\n# control-plane user data for {{ .Hostname }}\n", false)
	worker := testNodePoolRelease(name, "#!ipxe\n# worker image {{ .DiskSelectionStrategy }}\n", "#cloud-config\n# worker user data for {{ .Hostname }}\n")

	tests := []struct {
		name         string
		controlPlane bool
		objects      []ctrlclient.Object
		wantIPXE     string
		wantUserData string
		wantErr      string
	}{
		{
			name:         "control-plane node uses the ClusterRelease",
			controlPlane: true,
			objects:      append(append([]ctrlclient.Object{}, controlPlane...), worker...),
			wantIPXE:     "# control-plane image smallest",
			wantUserData: "# control-plane user data for node-0",
		},
		{
			name:         "worker node uses the NodePoolRelease",
			controlPlane: false,
			objects:      append(append([]ctrlclient.Object{}, controlPlane...), worker...),
			wantIPXE:     "# worker image smallest",
			wantUserData: "# worker user data for node-0",
		},
		{
			name:         "deprecated ClusterRelease still provisions an existing control plane",
			controlPlane: true,
			objects:      testClusterRelease(name, "#!ipxe\n# deprecated image {{ .DiskSelectionStrategy }}\n", "#cloud-config\n# deprecated user data for {{ .Hostname }}\n", true),
			wantIPXE:     "# deprecated image smallest",
			wantUserData: "# deprecated user data for node-0",
		},
		{
			name:         "control-plane node does not fall back to a NodePoolRelease",
			controlPlane: true,
			objects:      worker,
			wantErr:      `ClusterRelease "shared-os" is not installed`,
		},
		{
			name:         "worker node does not fall back to a ClusterRelease",
			controlPlane: false,
			objects:      controlPlane,
			wantErr:      `NodePoolRelease "shared-os" is not installed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			nodeSet := db.NodeSet{KubernetesVersion: "1.32.1"}
			nodeSet.SetNico(db.NicoNodePoolProvider{Release: name, DiskSelectionStrategy: db.DiskSelectionStrategySmallest})
			nodePool := db.NodePool{
				ControlPlane:       tt.controlPlane,
				MachineAnnotations: map[string]string{"example.com/setup": "gpu,network"},
				MachineLabels:      map[string]string{"example.com/pool": "gpu"},
			}
			nodePool.SetNico(db.NicoNodePoolProvider{
				InstanceTypeID: uuid.MustParse("70000000-0000-0000-0000-000000000005"),
				VpcPrefixIDs:   []uuid.UUID{uuid.MustParse("70000000-0000-0000-0000-000000000007")},
			})
			cluster := db.Cluster{Name: "cluster-a", CniPlugin: db.CNICalico}
			cluster.SetNicoProvider(db.NicoClusterProvider{VpcID: uuid.MustParse("70000000-0000-0000-0000-000000000006")})

			k8sClient := newMachineCreateClient(t, tt.objects...)
			err := New().CreateMachine(
				ctx,
				testMachineCreateDeps(k8sClient),
				db.Node{Name: "node-0", ControlPlane: tt.controlPlane, Nico: &db.NicoNodeProvider{}},
				nodeSet,
				nodePool,
				cluster,
			)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)

			machines := &clusterv1.MachineList{}
			require.NoError(t, k8sClient.List(ctx, machines))
			require.Len(t, machines.Items, 1)
			assert.Equal(t, "gpu,network", machines.Items[0].Annotations["example.com/setup"])
			assert.Equal(t, "gpu", machines.Items[0].Labels["example.com/pool"])
			nicoMachines := &nicov1.NicoMachineList{}
			require.NoError(t, k8sClient.List(ctx, nicoMachines, ctrlclient.InNamespace(constants.LegacyManagementNamespace)))
			require.Len(t, nicoMachines.Items, 1)
			assert.Contains(t, nicoMachines.Items[0].Spec.IpxeScript, tt.wantIPXE)

			secrets := &corev1.SecretList{}
			require.NoError(t, k8sClient.List(ctx, secrets, ctrlclient.InNamespace(constants.LegacyManagementNamespace)))
			require.Len(t, secrets.Items, 1)
			assert.Contains(t, string(secrets.Items[0].Data["value"]), tt.wantUserData)
		})
	}
}

func newMachineCreateClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// testMachineCreateDeps is the least Activities has to supply for the NICo
// path: it never mints a SPIRE join token, and the instance type a
// control-plane node already carries needs no seeding.
func testMachineCreateDeps(k8sClient ctrlclient.Client) shared.MachineCreateDeps {
	cfg := config.Config{
		DgxcTenantUser:     "nvidia",
		DgxcTenantPassword: "password",
		NodeAgent:          config.NodeAgentConfig{Port: "15005"},
	}

	return shared.MachineCreateDeps{
		K8sClient:        k8sClient,
		FieldManagerName: "cloud-workflow-engine",
		Namespace:        constants.LegacyManagementNamespace,
		Config:           cfg,
		NodeAgentBootstrapFiles: func(db.Node) ([]db.CloudInitFile, error) {
			return nil, nil
		},
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return "example.com", "trust-bundle", nil
		},
		SeedControlPlaneInstanceType: func(_ context.Context, _ db.Node, nodePool db.NodePool) (db.NodePool, error) {
			return nodePool, nil
		},
	}
}

func testNodePoolRelease(name, ipxe, userData string) []ctrlclient.Object {
	return []ctrlclient.Object{
		testMachineImage(name+"-image", ipxe),
		&enginev1.NodePoolRelease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: injectedVersionAnnotations()},
			Spec: enginev1.NodePoolReleaseSpec{
				MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(name + "-image")},
				MachineInit:     enginev1.MachineInit{UserDataTemplate: userData},
			},
		},
	}
}

// testClusterRelease gives its image its own name, so a NodePoolRelease of the
// same release name does not share it and the two cannot be confused.
func testClusterRelease(name, ipxe, userData string, deprecated bool) []ctrlclient.Object {
	return []ctrlclient.Object{
		testMachineImage(name+"-control-plane-image", ipxe),
		&enginev1.ClusterRelease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: injectedVersionAnnotations()},
			Spec: enginev1.ClusterReleaseSpec{
				MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(name + "-control-plane-image")},
				MachineInit:     enginev1.MachineInit{UserDataTemplate: userData},
				Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
			},
		},
	}
}

func testMachineImage(name, ipxe string) *enginev1.MachineImage {
	return &enginev1.MachineImage{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: enginev1.MachineImageSpec{
			OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: "24.04"},
			Architecture:    "amd64",
			BuildDate:       "2026-01-01",
			NICo:            enginev1.NICoBoot{IPXEScript: ipxe},
		},
	}
}

func injectedVersionAnnotations() map[string]string {
	return map[string]string{
		enginev1.MachineInitKubernetesVersionAnnotation:         "",
		enginev1.MachineInitKubernetesVersionInjectedAnnotation: "true",
	}
}
