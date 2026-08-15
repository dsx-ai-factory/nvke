// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestFirstInfrastructureRefIPAddress_NicoMachine(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, nicov1.AddToScheme(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nicomachines.infrastructure.cluster.x-k8s.io",
				Labels: map[string]string{
					"cluster.x-k8s.io/v1beta2": nicov1.GroupVersion.Version,
				},
			},
		}).
		WithObjects(&nicov1.NicoMachine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: nicov1.GroupVersion.String(),
				Kind:       nicoMachineConst,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-0",
				Namespace: "capi-cluster-a",
			},
			Status: nicov1.NicoMachineStatus{
				Addresses: clusterv1.MachineAddresses{{
					Type:    clusterv1.MachineInternalIP,
					Address: "10.0.0.10",
				}},
			},
		}).
		Build()

	ipAddress, err := firstInfrastructureRefIPAddress(context.Background(), k8sClient, &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "capi-cluster-a",
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     nicoMachineConst,
				Name:     "worker-0",
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "10.0.0.10", ipAddress)
}

func TestActivities_CreateTemplateLessForgeCAPIMachineForNode_Idempotent(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.MustParse("70000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("70000000-0000-0000-0000-000000000002")
	nodePoolID := uuid.MustParse("70000000-0000-0000-0000-000000000003")
	nodeSetID := uuid.MustParse("70000000-0000-0000-0000-000000000004")
	instanceTypeID := uuid.MustParse("70000000-0000-0000-0000-000000000005")
	vpcID := uuid.MustParse("70000000-0000-0000-0000-000000000006")
	vpcPrefixID := uuid.MustParse("70000000-0000-0000-0000-000000000007")
	siteID := uuid.MustParse("70000000-0000-0000-0000-000000000008")
	namespace := "cluster-cluster-a"
	node := db.Node{
		ID:           nodeID,
		NodePoolID:   nodePoolID,
		NodeSetID:    nodeSetID,
		ClusterID:    clusterID,
		Name:         "worker-0",
		TpmEKPubHash: "tpm-hash",
		Nico:         &db.NicoNodeProvider{},
	}
	nodeSet := db.NodeSet{
		ID:                         nodeSetID,
		NodePoolID:                 nodePoolID,
		OSProfile:                  "test-os",
		DiskSelectionStrategy:      db.DiskSelectionStrategySmallest,
		KubernetesVersion:          "1.32.1",
		CapiSpireAttestationMethod: db.SpireAttestationMethodTPM,
	}
	nodePool := db.NodePool{
		ID:        nodePoolID,
		ClusterID: clusterID,
	}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: instanceTypeID,
		VpcPrefixIDs:   []uuid.UUID{vpcPrefixID},
	})
	cluster := db.Cluster{
		ID:        clusterID,
		Name:      "cluster-a",
		CniPlugin: db.CNICalico,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID})
	k8sClient := newCAPIMachineCreateClient(t)
	a := &Activities{
		cfg:       testTemplateLessForgeCAPICfg(t),
		k8sClient: k8sClient,
	}
	createCtx := &capiMachineCreateContext{
		node:      node,
		nodeSet:   nodeSet,
		nodePool:  nodePool,
		cluster:   cluster,
		namespace: namespace,
	}

	require.NoError(t, a.createTemplateLessForgeCAPIMachineForNode(ctx, createCtx))
	require.NoError(t, a.createTemplateLessForgeCAPIMachineForNode(ctx, createCtx))

	machineList := &clusterv1.MachineList{}
	require.NoError(t, k8sClient.List(ctx, machineList, client.InNamespace(namespace)))
	require.Len(t, machineList.Items, 1)
	assert.Equal(t, node.Name, machineList.Items[0].Name)
	assert.Equal(t, nicoMachineConst, machineList.Items[0].Spec.InfrastructureRef.Kind)

	nicoMachineList := &nicov1.NicoMachineList{}
	require.NoError(t, k8sClient.List(ctx, nicoMachineList, client.InNamespace(namespace)))
	require.Len(t, nicoMachineList.Items, 1)
	require.Len(t, nicoMachineList.Items[0].Spec.Interfaces, 1)
	assert.Equal(t, vpcPrefixID.String(), nicoMachineList.Items[0].Spec.Interfaces[0].VPCPrefixID)

	secretList := &corev1.SecretList{}
	require.NoError(t, k8sClient.List(ctx, secretList, client.InNamespace(namespace)))
	require.Len(t, secretList.Items, 1)
	assert.Equal(t, "worker-0-bootstrap-data", secretList.Items[0].Name)
	assert.Contains(t, string(secretList.Items[0].Data[valueLabel]), "worker-0")
}

func TestActivities_WaitForCAPIMachineReady_RetryBehavior(t *testing.T) {
	nodeID := uuid.MustParse("71000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("71000000-0000-0000-0000-000000000002")
	firstVpcPrefixID := uuid.MustParse("71000000-0000-0000-0000-000000000003")
	secondVpcPrefixID := uuid.MustParse("71000000-0000-0000-0000-000000000004")
	nodeInstanceTypeID := uuid.MustParse("71000000-0000-0000-0000-000000000005")
	exhaustionMessage := "VPC Prefix " + firstVpcPrefixID.String() +
		" does not have enough IP addresses: 1 of 2 IP addresses remain available, " +
		"but the 1 interface(s) in this request require 2 IP addresses"
	node := db.Node{
		ID:             nodeID,
		ClusterID:      clusterID,
		ControlPlane:   true,
		Name:           "worker-0",
		InstanceTypeID: nodeInstanceTypeID,
	}
	cluster := db.Cluster{
		ID:   clusterID,
		Name: "cluster-a",
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				KubernetesVersion: "1.32.1",
				VpcPrefixIDs:      []uuid.UUID{firstVpcPrefixID, secondVpcPrefixID},
			},
		},
	}

	t.Run("persists existing machine IP without rotating stale NicoMachine exhaustion", func(t *testing.T) {
		k8sClient := newCAPIMachineReadyClient(t,
			testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: "10.0.0.10",
			}}),
			testNicoMachineForCAPIMachineReady(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            exhaustionMessage,
			}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, node, cluster)
		updatedNode := node
		updatedNode.IP = "10.0.0.10"
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == node.ID && nodeForUpdate.IP == "10.0.0.10"
		})).Return(updatedNode, nil)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ipAddress)

		nicoMachine := getTestNicoMachine(t, k8sClient)
		require.Len(t, nicoMachine.Spec.Interfaces, 1)
		assert.Equal(t, firstVpcPrefixID.String(), nicoMachine.Spec.Interfaces[0].VPCPrefixID)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("stamps effective instance type for a templateless node without a type", func(t *testing.T) {
		instanceTypeID := uuid.MustParse("71000000-0000-0000-0000-000000000006")
		typelessNode := db.Node{
			ID:           nodeID,
			ClusterID:    clusterID,
			ControlPlane: true,
			Name:         "worker-0",
		}
		k8sClient := newCAPIMachineReadyClient(t,
			testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: "10.0.0.10",
			}}),
			testNicoMachineForCAPIMachineReady(nicoMachineRotationParameters{
				InstanceTypeID: instanceTypeID.String(),
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
			}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, typelessNode, cluster)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, typelessNode.ID).Return(typelessNode, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == typelessNode.ID &&
				nodeForUpdate.IP == "10.0.0.10" &&
				nodeForUpdate.InstanceTypeID == instanceTypeID
		})).Return(typelessNode, nil)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ipAddress)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("rotates one network candidate and waits when IP is absent", func(t *testing.T) {
		k8sClient := newCAPIMachineReadyClient(t,
			testCAPIMachineWithNicoInfraRef(nil),
			testNicoMachineForCAPIMachineReady(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            exhaustionMessage,
			}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, node, cluster)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.ErrorContains(t, err, "has no IP addresses in status or infrastructure ref")
		assert.Empty(t, ipAddress)

		nicoMachine := getTestNicoMachine(t, k8sClient)
		require.Len(t, nicoMachine.Spec.Interfaces, 1)
		assert.Equal(t, secondVpcPrefixID.String(), nicoMachine.Spec.Interfaces[0].VPCPrefixID)
		assert.Empty(t, nicoMachine.Spec.Interfaces[0].SubnetID)
		mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})
}

func testInfrastructureRef() clusterv1.ContractVersionedObjectReference {
	return clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     nicoMachineConst,
		Name:     "worker-0",
	}
}

func testCAPIMachineWithNicoInfraRef(addresses clusterv1.MachineAddresses) *clusterv1.Machine {
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-cluster-a",
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     nicoMachineConst,
				Name:     "worker-0",
			},
		},
		Status: clusterv1.MachineStatus{
			Addresses: addresses,
		},
	}
}

type nicoMachineRotationParameters struct {
	Interface          nicov1.NicoMachineInterface
	InstanceTypeID     string
	Reason             string
	Generation         int64
	ObservedGeneration int64
	Message            string
}

func testNicoMachineForRotation(params nicoMachineRotationParameters) *nicov1.NicoMachine {
	reason := params.Reason
	if reason == "" {
		reason = nicov1.InstanceCreateFailedReason
	}
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       nicoMachineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "worker-0",
			Namespace:  "capi-cluster-a",
			Generation: params.Generation,
		},
		Spec: nicov1.NicoMachineSpec{
			InstanceTypeID: params.InstanceTypeID,
			Interfaces:     []nicov1.NicoMachineInterface{params.Interface},
		},
		Status: nicov1.NicoMachineStatus{
			Conditions: []metav1.Condition{{
				Type:               clusterv1.ReadyCondition,
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            params.Message,
				ObservedGeneration: params.ObservedGeneration,
			}},
		},
	}
}

func testNicoMachineForCAPIMachineReady(params nicoMachineRotationParameters) *nicov1.NicoMachine {
	nicoMachine := testNicoMachineForRotation(params)
	nicoMachine.Namespace = "cluster-cluster-a"
	return nicoMachine
}

func newNicoMachineRotationClient(t *testing.T, nicoMachine *nicov1.NicoMachine) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, nicov1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nicoMachine).
		Build()
}

func newCAPIMachineCreateClient(t *testing.T) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
}

func testTemplateLessForgeCAPICfg(t *testing.T) config.Config {
	t.Helper()

	profilesDir := t.TempDir()
	osProfileDir := filepath.Join(profilesDir, "osProfiles", "test-os")
	require.NoError(t, os.MkdirAll(osProfileDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(osProfileDir, "ipxeScript"), []byte("#!ipxe\n{{ .DiskSelectionStrategy }}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(osProfileDir, "userData.tmpl"), []byte("#cloud-config\nhostname: {{ .Hostname }}\n"), 0o644))

	return config.Config{
		ProfilesDir:           profilesDir,
		DgxcTenantUser:        "nvidia",
		DgxcTenantPassword:    "password",
		NodeAgentPort:         "15005",
		SpireServerAddress:    "spire-server",
		SpireServerPort:       "9091",
		NKESiteControllerName: "site-a",
	}
}

func newCAPIMachineReadyClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	objects = append(objects, &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nicomachines.infrastructure.cluster.x-k8s.io",
			Labels: map[string]string{
				"cluster.x-k8s.io/v1beta2": nicov1.GroupVersion.Version,
			},
		},
	})
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

func newCAPIMachineReadyDB(t *testing.T, node db.Node, cluster db.Cluster) (*testutil.MockDB, *testutil.MockTx) {
	t.Helper()

	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	return mockDB, mockTx
}

func executeWaitForCAPIMachineReadyActivity(t *testing.T, a *Activities, input WaitForCAPIMachineReadyInput) (string, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.waitForCAPIMachineReady)

	value, err := env.ExecuteActivity(a.waitForCAPIMachineReady, input)
	if err != nil {
		return "", err
	}

	var ipAddress string
	require.NoError(t, value.Get(&ipAddress))
	return ipAddress, nil
}

func getTestNicoMachine(t *testing.T, k8sClient client.Client) *nicov1.NicoMachine {
	t.Helper()

	nicoMachine := &nicov1.NicoMachine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "cluster-cluster-a",
		Name:      "worker-0",
	}, nicoMachine))
	return nicoMachine
}

func testCAPINicoMachine() *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "capi-cluster-a",
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     nicoMachineConst,
				Name:     "worker-0",
			},
		},
	}
}
