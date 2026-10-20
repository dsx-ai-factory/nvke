// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
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
				Kind:       "NicoMachine",
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
				Kind:     "NicoMachine",
				Name:     "worker-0",
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "10.0.0.10", ipAddress)
}

// TestActivities_CreateCAPIInfraMachineForNode_TemplatelessNico_Idempotent
// exercises the real CreateCAPIInfraMachineForNode activity end-to-end for a
// NICo-provider cluster, verifying Activities wires clients.GetMachineMaterialization
// and the resulting client correctly: factory picks nico.Client, which drives
// the whole create (NicoMachine, bootstrap secret, generic Machine).
func TestActivities_CreateCAPIInfraMachineForNode_TemplatelessNico_Idempotent(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.MustParse("70000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("70000000-0000-0000-0000-000000000002")
	nodePoolID := uuid.MustParse("70000000-0000-0000-0000-000000000003")
	nodeSetID := uuid.MustParse("70000000-0000-0000-0000-000000000004")
	instanceTypeID := uuid.MustParse("70000000-0000-0000-0000-000000000005")
	vpcID := uuid.MustParse("70000000-0000-0000-0000-000000000006")
	vpcPrefixID := uuid.MustParse("70000000-0000-0000-0000-000000000007")
	siteID := uuid.MustParse("70000000-0000-0000-0000-000000000008")
	node := db.Node{
		ID:         nodeID,
		NodePoolID: nodePoolID,
		NodeSetID:  nodeSetID,
		ClusterID:  clusterID,
		Name:       "worker-0",
		Nico:       &db.NicoNodeProvider{},
	}
	nodeSet := db.NodeSet{
		ID:                nodeSetID,
		NodePoolID:        nodePoolID,
		KubernetesVersion: "1.32.1",
	}
	nodeSet.SetNico(db.NicoNodePoolProvider{
		Release:               "test-os",
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	nodePool := db.NodePool{
		ID:        nodePoolID,
		ClusterID: clusterID,
	}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: instanceTypeID,
		VpcPrefixIDs:   []uuid.UUID{vpcPrefixID},
	})
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "cluster-a",
		CniPlugin:    db.CNICalico,
		ProviderType: db.ProviderTypeNico,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID})
	namespace := constants.LegacyManagementNamespace
	k8sClient := newCAPIMachineCreateClient(t, testNicoWorkerRelease("test-os", "#!ipxe\n{{ .DiskSelectionStrategy }}", "#cloud-config\nhostname: {{ .Hostname }}\n")...)

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	mockDB.On("GetNodeSet", mock.Anything, nodeSetID).Return(nodeSet, nil)
	mockDB.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	a := &Activities{
		db:        mockDB,
		cfg:       testTemplateLessForgeCAPICfg(t),
		k8sClient: k8sClient,
	}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.CreateCAPIInfraMachineForNode)

	input := CreateCAPIInfraMachineForNodeInput{NodeID: nodeID}
	_, err := env.ExecuteActivity(a.CreateCAPIInfraMachineForNode, input)
	require.NoError(t, err)
	_, err = env.ExecuteActivity(a.CreateCAPIInfraMachineForNode, input)
	require.NoError(t, err)

	machineList := &clusterv1.MachineList{}
	require.NoError(t, k8sClient.List(ctx, machineList, client.InNamespace(namespace)))
	require.Len(t, machineList.Items, 1)
	assert.Equal(t, node.Name, machineList.Items[0].Name)
	assert.Equal(t, "NicoMachine", machineList.Items[0].Spec.InfrastructureRef.Kind)

	nicoMachineList := &nicov1.NicoMachineList{}
	require.NoError(t, k8sClient.List(ctx, nicoMachineList, client.InNamespace(namespace)))
	require.Len(t, nicoMachineList.Items, 1)
	require.Len(t, nicoMachineList.Items[0].Spec.Interfaces, 1)
	assert.Equal(t, vpcPrefixID.String(), nicoMachineList.Items[0].Spec.Interfaces[0].VPCPrefixID)

	secretList := &corev1.SecretList{}
	require.NoError(t, k8sClient.List(ctx, secretList, client.InNamespace(namespace)))
	require.Len(t, secretList.Items, 1)
	assert.Equal(t, "worker-0-bootstrap-data", secretList.Items[0].Name)
	assert.Contains(t, string(secretList.Items[0].Data["value"]), "worker-0")
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
		ID:           nodeID,
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         "worker-0",
		Nico: &db.NicoNodeProvider{
			InstanceTypeID: nodeInstanceTypeID,
		},
	}
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "cluster-a",
		ProviderType: db.ProviderTypeNico,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				KubernetesVersion: "1.32.1",
			},
		},
	}
	cluster.ControlPlaneConfig.ControlPlaneConfigSpec.SetNico(db.NicoControlPlaneProvider{
		VpcPrefixIDs: []uuid.UUID{firstVpcPrefixID, secondVpcPrefixID},
	})

	t.Run("missing node is non-retryable", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, db.ErrNotFound).Once()

		a := &Activities{db: mockDB}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.Error(t, err)
		assert.Empty(t, ipAddress)
		var applicationErr *temporal.ApplicationError
		require.ErrorAs(t, err, &applicationErr)
		assert.True(t, applicationErr.NonRetryable())
		mockDB.AssertExpectations(t)
	})

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
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(node, nil).Once()
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
			// A payload with no type in it: every stored Nico node has one, and
			// the type is only settled by the infra provider.
			Nico: &db.NicoNodeProvider{},
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
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(typelessNode, nil).Once()
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == typelessNode.ID &&
				nodeForUpdate.IP == "10.0.0.10" &&
				nodeForUpdate.NicoInstanceTypeID() == instanceTypeID
		})).Return(typelessNode, nil)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ipAddress)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("stamps Forge instance and machine IDs from NicoMachine status", func(t *testing.T) {
		instanceID := "8f9efd7f-8f3b-4bb3-b523-ee179e7dca22"
		machineID := "fm100ht9b2uffrr8uj92siejras651v9143ev6ikit22mj7lfh2i22vema0"
		k8sClient := newCAPIMachineReadyClient(t,
			testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: "10.0.0.10",
			}}),
			testNicoMachineForCAPIMachineReady(nicoMachineRotationParameters{
				InstanceID: instanceID,
				MachineID:  machineID,
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
			}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, node, cluster)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(node, nil).Once()
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.IP == "10.0.0.10" &&
				nodeForUpdate.Nico != nil &&
				nodeForUpdate.Nico.ForgeInstanceID == instanceID &&
				nodeForUpdate.Nico.ForgeMachineID == machineID
		})).Return(node, nil)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ipAddress)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("does not clobber an already-recorded Forge identity", func(t *testing.T) {
		existing := node
		existing.Nico = &db.NicoNodeProvider{
			ForgeInstanceID: "existing-instance",
			ForgeMachineID:  "existing-machine",
			InstanceTypeID:  nodeInstanceTypeID,
		}
		k8sClient := newCAPIMachineReadyClient(t,
			testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: "10.0.0.10",
			}}),
			testNicoMachineForCAPIMachineReady(nicoMachineRotationParameters{
				InstanceID: "observed-instance",
				MachineID:  "observed-machine",
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
			}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, existing, cluster)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, existing.ID).Return(existing, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == existing.ID &&
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(existing, nil).Once()
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == existing.ID &&
				nodeForUpdate.IP == "10.0.0.10" &&
				nodeForUpdate.Nico != nil &&
				nodeForUpdate.Nico.ForgeInstanceID == "existing-instance" &&
				nodeForUpdate.Nico.ForgeMachineID == "existing-machine"
		})).Return(existing, nil)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ipAddress)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("empty observed Forge IDs are a no-op", func(t *testing.T) {
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
			}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, node, cluster)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(node, nil).Once()
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			// The instance-type stamp legitimately populates the nico payload, so
			// assert that no Forge identity was invented rather than no payload.
			forgeInstanceID, forgeMachineID := forgeIdentityFromNode(*nodeForUpdate)
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.IP == "10.0.0.10" &&
				forgeInstanceID == "" &&
				forgeMachineID == ""
		})).Return(node, nil)

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.10", ipAddress)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("identity lookup failure does not block ready", func(t *testing.T) {
		k8sClient := newCAPIMachineReadyClient(t,
			testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{
				Type:    clusterv1.MachineInternalIP,
				Address: "10.0.0.10",
			}}),
		)
		mockDB, mockTx := newCAPIMachineReadyDB(t, node, cluster)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(node, nil).Once()
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			// The instance-type stamp legitimately populates the nico payload, so
			// assert that no Forge identity was invented rather than no payload.
			forgeInstanceID, forgeMachineID := forgeIdentityFromNode(*nodeForUpdate)
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.IP == "10.0.0.10" &&
				forgeInstanceID == "" &&
				forgeMachineID == ""
		})).Return(node, nil)

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
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
			return nodeForUpdate.ID == node.ID &&
				nodeForUpdate.Conditions.Pending.Status == db.ConditionStatusTrue &&
				nodeForUpdate.Conditions.Provisioning.Status == db.ConditionStatusFalse
		})).Return(node, nil).Once()

		a := &Activities{db: mockDB, k8sClient: k8sClient}

		ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
		require.ErrorContains(t, err, "has no IP addresses in status or infrastructure ref")
		assert.Empty(t, ipAddress)

		nicoMachine := getTestNicoMachine(t, k8sClient)
		require.Len(t, nicoMachine.Spec.Interfaces, 1)
		assert.Equal(t, secondVpcPrefixID.String(), nicoMachine.Spec.Interfaces[0].VPCPrefixID)
		assert.Empty(t, nicoMachine.Spec.Interfaces[0].SubnetID)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})
}

func TestActivities_UpdateCAPIMachineWaitNodeCondition(t *testing.T) {
	nodeID := uuid.MustParse("73000000-0000-0000-0000-000000000001")
	baseNode := db.Node{
		ID:   nodeID,
		Name: "worker-0",
		Conditions: db.NodeConditions{
			ProvisioningClaimed: db.NewCondition(true, "Provisioning workflow claimed node"),
		},
	}

	tests := map[string]struct {
		machine        *clusterv1.Machine
		expectedStatus db.ConditionStatus
		expectedType   string
		expectedMsg    string
	}{
		"keeps node pending until providerID exists": {
			machine: &clusterv1.Machine{
				Spec: clusterv1.MachineSpec{},
				Status: clusterv1.MachineStatus{
					Conditions: []metav1.Condition{{
						Type:    clusterv1.MachineInfrastructureReadyCondition,
						Status:  metav1.ConditionFalse,
						Message: "instance type has no unused usable allocations",
					}},
				},
			},
			expectedStatus: db.ConditionStatusTrue,
			expectedType:   "pending",
			expectedMsg:    "instance type has no unused usable allocations",
		},
		"marks node provisioning after providerID exists": {
			machine: &clusterv1.Machine{
				Spec: clusterv1.MachineSpec{
					ProviderID: "nico://instance-1",
				},
			},
			expectedStatus: db.ConditionStatusTrue,
			expectedType:   "provisioning",
			expectedMsg:    "Waiting for CAPI machine to be ready",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			mockTx := &testutil.MockTx{}
			mockDB.MockTx = mockTx
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			mockTx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(baseNode, nil)
			mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
				switch tt.expectedType {
				case "pending":
					return updated.Conditions.Pending.Status == tt.expectedStatus &&
						updated.Conditions.Pending.Message == tt.expectedMsg &&
						updated.Conditions.ProvisioningClaimed.Status == db.ConditionStatusTrue &&
						updated.Conditions.Provisioning.Status == db.ConditionStatusFalse
				case "provisioning":
					return updated.Conditions.Provisioning.Status == tt.expectedStatus &&
						updated.Conditions.Provisioning.Message == tt.expectedMsg &&
						updated.Conditions.ProvisioningClaimed.Status == db.ConditionStatusTrue &&
						updated.Conditions.Pending.Status == db.ConditionStatusFalse
				default:
					return false
				}
			})).Return(baseNode, nil)

			a := &Activities{db: mockDB}
			err := a.updateCAPIMachineWaitNodeCondition(context.Background(), nodeID, tt.machine, "Waiting for CAPI machine to be ready")

			require.NoError(t, err)
			mockDB.AssertExpectations(t)
			mockTx.AssertExpectations(t)
		})
	}
}

func testCAPIMachineWithNicoInfraRef(addresses clusterv1.MachineAddresses) *clusterv1.Machine {
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: constants.LegacyManagementNamespace,
		},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     "NicoMachine",
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
	InstanceID         string
	MachineID          string
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
			Kind:       "NicoMachine",
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
			InstanceID: params.InstanceID,
			MachineID:  params.MachineID,
			Conditions: []metav1.Condition{{
				Type:               nicov1.MachineProvisionedCondition,
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
	nicoMachine.Namespace = constants.LegacyManagementNamespace
	return nicoMachine
}

func newCAPIMachineCreateClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

// testNicoWorkerRelease is the least a NICo worker node needs to provision: a
// NodePoolRelease and the MachineImage it boots.
func testNicoWorkerRelease(name, ipxe, userData string) []client.Object {
	return []client.Object{
		&enginev1.MachineImage{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-image"},
			Spec: enginev1.MachineImageSpec{
				OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: "24.04"},
				Architecture:    "amd64",
				BuildDate:       "2026-01-01",
				NICo:            enginev1.NICoBoot{IPXEScript: ipxe},
			},
		},
		&enginev1.NodePoolRelease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					enginev1.MachineInitKubernetesVersionAnnotation:         "",
					enginev1.MachineInitKubernetesVersionInjectedAnnotation: "true",
				},
			},
			Spec: enginev1.NodePoolReleaseSpec{
				MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(name + "-image")},
				MachineInit:     enginev1.MachineInit{UserDataTemplate: userData},
			},
		},
	}
}

func testTemplateLessForgeCAPICfg(t *testing.T) config.Config {
	t.Helper()

	return config.Config{
		DgxcTenantUser:                     "nvidia",
		DgxcTenantPassword:                 "password",
		NodeAgent:                          config.NodeAgentConfig{Port: "15005"},
		SpireServerAddress:                 "spire-server",
		SpireServerPort:                    "9091",
		NVKESiteControllerName:             "site-a",
		FieldManagerName:                   "cloud-workflow-engine",
		FieldManagerRebootMachineName:      "cloud-workflow-engine-reboot-machine",
		FieldManagerMachineHealthIssueName: "cloud-workflow-engine-machine-health-issue",
	}
}

// A LaunchLayer node records CAPLL's reported identifiers in its launchLayer
// payload, never under nico.
func TestActivities_WaitForCAPIMachineReady_StampsLaunchLayerIdentity(t *testing.T) {
	nodeID := uuid.MustParse("71000000-0000-0000-0000-000000000011")
	clusterID := uuid.MustParse("71000000-0000-0000-0000-000000000012")
	node := db.Node{ID: nodeID, ClusterID: clusterID, ControlPlane: true, Name: "worker-0"}
	node.SetLaunchLayer(db.LaunchLayerNodeProvider{})
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a", ProviderType: db.ProviderTypeLaunchLayer}
	cluster.ControlPlaneConfig.KubernetesVersion = "1.32.1"
	cluster.ControlPlaneConfig.SetLaunchLayer(db.LaunchLayerControlPlaneProvider{InstanceType: "gpu.b200.8x"})

	launchLayerMachine := &unstructured.Unstructured{}
	launchLayerMachine.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha1")
	launchLayerMachine.SetKind("LaunchLayerMachine")
	launchLayerMachine.SetNamespace(constants.NamespaceNVKESystem)
	launchLayerMachine.SetName("worker-0")
	launchLayerMachine.Object["status"] = map[string]any{
		"instanceID": "instance-123",
		"machineID":  "machine-456",
		"rackID":     "rack-7",
	}
	machine := testCAPIMachineWithNicoInfraRef(clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.10"}})
	machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "LaunchLayerMachine",
		Name:     "worker-0",
	}
	k8sClient := newCAPIMachineReadyClient(t, machine, launchLayerMachine, &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "launchlayermachines.infrastructure.cluster.x-k8s.io",
			Labels: map[string]string{"cluster.x-k8s.io/v1beta2": "v1alpha1"},
		},
	})

	mockDB, mockTx := newCAPIMachineReadyDB(t, node, cluster)
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
		return nodeForUpdate.IP == ""
	})).Return(node, nil).Maybe()
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(nodeForUpdate *db.Node) bool {
		return nodeForUpdate.IP == "10.0.0.10" &&
			nodeForUpdate.Nico == nil &&
			nodeForUpdate.LaunchLayer != nil &&
			*nodeForUpdate.LaunchLayer == db.LaunchLayerNodeProvider{InstanceID: "instance-123", MachineID: "machine-456", RackID: "rack-7"}
	})).Return(node, nil).Once()

	a := &Activities{db: mockDB, k8sClient: k8sClient}

	ipAddress, err := executeWaitForCAPIMachineReadyActivity(t, a, WaitForCAPIMachineReadyInput{NodeID: nodeID})
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.10", ipAddress)
	mockTx.AssertExpectations(t)
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

func TestDeleteCAPIMachineForNode_WaitFalseReturnsWhileObjectsFinalize(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.MustParse("74000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("74000000-0000-0000-0000-000000000002")
	node := db.Node{
		ID:        nodeID,
		ClusterID: clusterID,
		Name:      "worker-0",
	}

	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "worker-0",
			Namespace:  constants.LegacyManagementNamespace,
			Finalizers: []string{"test.finalizer"},
		},
	}
	bootstrapSecret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "worker-0-bootstrap-data",
			Namespace:  constants.LegacyManagementNamespace,
			Finalizers: []string{"test.finalizer"},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, bootstrapSecret).
		Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()

	a := &Activities{db: mockDB, k8sClient: k8sClient}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.DeleteCAPIMachineForNode)

	_, err := env.ExecuteActivity(a.DeleteCAPIMachineForNode, DeleteCAPIMachineForNodeInput{NodeID: nodeID})
	require.NoError(t, err)

	storedMachine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.LegacyManagementNamespace,
		Name:      "worker-0",
	}, storedMachine))
	require.NotNil(t, storedMachine.DeletionTimestamp)

	storedSecret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.LegacyManagementNamespace,
		Name:      "worker-0-bootstrap-data",
	}, storedSecret))
	require.NotNil(t, storedSecret.DeletionTimestamp)

	mockDB.AssertExpectations(t)
}

func TestDeleteCAPIMachineForNode_ClassifiesNodeLookupErrors(t *testing.T) {
	transientErr := errors.New("database connection reset")
	tests := []struct {
		name             string
		nodeErr          error
		wantNonRetryable bool
	}{
		{name: "missing node", nodeErr: db.ErrNotFound, wantNonRetryable: true},
		{name: "transient database failure", nodeErr: transientErr, wantNonRetryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeID := uuid.New()
			mockDB := &testutil.MockDB{}
			mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, tt.nodeErr)

			a := &Activities{db: mockDB}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(a.DeleteCAPIMachineForNode)

			_, err := env.ExecuteActivity(a.DeleteCAPIMachineForNode, DeleteCAPIMachineForNodeInput{NodeID: nodeID})
			require.Error(t, err)
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.Equal(t, tt.wantNonRetryable, applicationErr.NonRetryable())
			mockDB.AssertExpectations(t)
		})
	}
}

func TestCreateCAPIInfraMachineForNode_ClassifiesNodeLookupErrors(t *testing.T) {
	transientErr := errors.New("database connection reset")
	tests := []struct {
		name             string
		nodeErr          error
		wantNonRetryable bool
	}{
		{name: "missing node", nodeErr: db.ErrNotFound, wantNonRetryable: true},
		{name: "transient database failure", nodeErr: transientErr, wantNonRetryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeID := uuid.New()
			mockDB := &testutil.MockDB{}
			mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, tt.nodeErr)

			a := &Activities{db: mockDB}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(a.CreateCAPIInfraMachineForNode)

			_, err := env.ExecuteActivity(a.CreateCAPIInfraMachineForNode, CreateCAPIInfraMachineForNodeInput{NodeID: nodeID})
			require.Error(t, err)
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.Equal(t, tt.wantNonRetryable, applicationErr.NonRetryable())
			mockDB.AssertExpectations(t)
		})
	}
}

func TestCreateCAPIInfraMachineForNode_UnsupportedMachineConfigIsNonRetryable(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	node := db.Node{
		ID:                     nodeID,
		NodePoolID:             nodePoolID,
		NodeSetID:              nodeSetID,
		ClusterID:              clusterID,
		Name:                   "worker",
		SpiffeEnabled:          true,
		SpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	}

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	mockDB.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
	mockDB.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID, ClusterID: clusterID}, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeLaunchLayer}, nil)

	a := &Activities{db: mockDB}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.CreateCAPIInfraMachineForNode)

	_, err := env.ExecuteActivity(a.CreateCAPIInfraMachineForNode, CreateCAPIInfraMachineForNodeInput{NodeID: nodeID})
	require.Error(t, err)
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationErr)
	require.True(t, applicationErr.NonRetryable(),
		"a node configured for an attestation method its provider rejects cannot succeed by retrying")
	mockDB.AssertExpectations(t)
}

// TestApplyCAPIMachineRebootAnnotation_LaunchLayerUsesCAPLLKey proves the
// reboot annotation key is provider-dispatched, not always
// constants.MachineAnnotationKeyReboot: a LaunchLayer-backed node gets
// CAPLL's own fixed key, since CAPLL has no configurable override the way
// CAPNico does.
func TestApplyCAPIMachineRebootAnnotation_LaunchLayerUsesCAPLLKey(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.MustParse("75000000-0000-0000-0000-000000000003")
	clusterID := uuid.MustParse("75000000-0000-0000-0000-00000000000e")
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-2"}

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeLaunchLayer}, nil)

	a := &Activities{db: mockDB, k8sClient: k8sClient, cfg: config.Config{FieldManagerRebootMachineName: "cloud-workflow-engine-reboot-machine"}}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ApplyCAPIMachineRebootAnnotation)

	_, err := env.ExecuteActivity(a.ApplyCAPIMachineRebootAnnotation, ApplyCAPIMachineRebootAnnotationInput{NodeID: nodeID})
	require.NoError(t, err)

	storedMachine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.NamespaceNVKESystem,
		Name:      "worker-2",
	}, storedMachine))
	require.Contains(t, storedMachine.Annotations, "infrastructure.cluster.x-k8s.io/reboot-requested")
	require.False(t, keys.MachineReboot.Has(storedMachine.Annotations), "LaunchLayer nodes must not get NVKE's reboot key in either domain")
	mockDB.AssertExpectations(t)
}

func TestApplyCAPIMachineRebootAnnotation(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.MustParse("75000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("75000000-0000-0000-0000-00000000000c")
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeCAPI}, nil)

	a := &Activities{db: mockDB, k8sClient: k8sClient, cfg: config.Config{FieldManagerRebootMachineName: "cloud-workflow-engine-reboot-machine"}}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ApplyCAPIMachineRebootAnnotation)

	value, err := env.ExecuteActivity(a.ApplyCAPIMachineRebootAnnotation, ApplyCAPIMachineRebootAnnotationInput{NodeID: nodeID})
	require.NoError(t, err)
	var result ApplyCAPIMachineRebootAnnotationResult
	require.NoError(t, value.Get(&result))
	require.False(t, result.RebootStartedTime.IsZero())

	storedMachine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.LegacyManagementNamespace,
		Name:      "worker-0",
	}, storedMachine))
	// Current domain only: cluster-api-provider-nico is pointed at this key by
	// its --reboot-annotation flag, and a legacy copy would be dead weight.
	require.Contains(t, storedMachine.Annotations, keys.MachineReboot.String())
	require.NotContains(t, storedMachine.Annotations, keys.MachineReboot.Legacy())
	mockDB.AssertExpectations(t)
}

func TestApplyCAPIMachineRebootAnnotation_ClassifiesNodeLookupErrors(t *testing.T) {
	transientErr := errors.New("database connection reset")
	tests := []struct {
		name             string
		nodeErr          error
		wantNonRetryable bool
	}{
		{name: "missing node", nodeErr: db.ErrNotFound, wantNonRetryable: true},
		{name: "transient database failure", nodeErr: transientErr, wantNonRetryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeID := uuid.New()
			mockDB := &testutil.MockDB{}
			mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, tt.nodeErr)

			a := &Activities{db: mockDB}
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestActivityEnvironment()
			env.RegisterActivity(a.ApplyCAPIMachineRebootAnnotation)

			_, err := env.ExecuteActivity(a.ApplyCAPIMachineRebootAnnotation, ApplyCAPIMachineRebootAnnotationInput{NodeID: nodeID})
			require.Error(t, err)
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.Equal(t, tt.wantNonRetryable, applicationErr.NonRetryable())
			mockDB.AssertExpectations(t)
		})
	}
}

func TestApplyCAPIMachineRebootAnnotation_UnsupportedProviderIsNonRetryable(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker"}

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, ProviderType: db.ProviderType("bogus")}, nil)

	a := &Activities{db: mockDB}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ApplyCAPIMachineRebootAnnotation)

	_, err := env.ExecuteActivity(a.ApplyCAPIMachineRebootAnnotation, ApplyCAPIMachineRebootAnnotationInput{NodeID: nodeID})
	require.Error(t, err)
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationErr)
	require.True(t, applicationErr.NonRetryable(),
		"an unsupported cluster.ProviderType cannot become supported by retrying")
	mockDB.AssertExpectations(t)
}

func TestDeleteCAPIMachineRebootAnnotation(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.MustParse("75000000-0000-0000-0000-000000000002")
	clusterID := uuid.MustParse("75000000-0000-0000-0000-00000000000d")
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-1"}

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, ProviderType: db.ProviderTypeCAPI}, nil)

	a := &Activities{db: mockDB, k8sClient: k8sClient, cfg: config.Config{FieldManagerRebootMachineName: "cloud-workflow-engine-reboot-machine"}}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.DeleteCAPIMachineRebootAnnotation)

	_, err := env.ExecuteActivity(a.DeleteCAPIMachineRebootAnnotation, DeleteCAPIMachineRebootAnnotationInput{NodeID: nodeID})
	require.NoError(t, err)

	storedMachine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.LegacyManagementNamespace,
		Name:      "worker-1",
	}, storedMachine))
	require.NotContains(t, storedMachine.Annotations, keys.MachineReboot.String())
	mockDB.AssertExpectations(t)
}

func getTestNicoMachine(t *testing.T, k8sClient client.Client) *nicov1.NicoMachine {
	t.Helper()

	nicoMachine := &nicov1.NicoMachine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: constants.LegacyManagementNamespace,
		Name:      "worker-0",
	}, nicoMachine))
	return nicoMachine
}

// A site stood up without SPIFFE has no SPIRE server and no trust domain, so
// asking for a join token fails with "trust domain is missing". That error is
// not retryable, so the node never gets a CAPI machine and the cluster sits in
// Provisioning for good. The join token has to be skipped, not attempted.
func TestCreateCAPISpireJoinTokenSkippedWhenSpiffeDisabled(t *testing.T) {
	node := db.Node{ID: uuid.New()}

	a := &Activities{cfg: config.Config{
		SpiffeEnabled: false,
		NodeAgent:     config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodJoinToken},
	}}
	token, path, err := a.createCAPISpireJoinToken(context.Background(), node)
	require.NoError(t, err, "must not reach SPIRE when SPIFFE is off")
	assert.Empty(t, token)
	assert.Empty(t, path)
}

func TestForgeIdentityFromNode(t *testing.T) {
	tests := map[string]struct {
		node           db.Node
		wantInstanceID string
		wantMachineID  string
	}{
		"reads the nico payload": {
			node: db.Node{
				Nico: &db.NicoNodeProvider{
					ForgeInstanceID: "nico-instance",
					ForgeMachineID:  "nico-machine",
				},
			},
			wantInstanceID: "nico-instance",
			wantMachineID:  "nico-machine",
		},
		"reports a partially stamped payload as-is": {
			node: db.Node{
				Nico: &db.NicoNodeProvider{
					ForgeInstanceID: "nico-instance",
				},
			},
			wantInstanceID: "nico-instance",
		},
		"empty when there is no nico payload": {
			node: db.Node{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			instanceID, machineID := forgeIdentityFromNode(tt.node)
			assert.Equal(t, tt.wantInstanceID, instanceID)
			assert.Equal(t, tt.wantMachineID, machineID)
		})
	}
}
