// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestNicoMachineRotation_NicoMachineReadyCondition(t *testing.T) {
	conditions := []metav1.Condition{{
		Type:   "Other",
		Status: metav1.ConditionTrue,
	}, {
		Type:   clusterv1.ReadyCondition,
		Status: metav1.ConditionFalse,
		Reason: "CreateFailed",
	}}

	readyCondition := nicoMachineReadyCondition(conditions)

	if assert.NotNil(t, readyCondition) {
		assert.Equal(t, clusterv1.ReadyCondition, readyCondition.Type)
		assert.Equal(t, metav1.ConditionFalse, readyCondition.Status)
	}
	assert.Nil(t, nicoMachineReadyCondition(nil))
}

func TestNicoMachineRotation_IsNicoNetworkExhaustionMessage(t *testing.T) {
	type parameters struct {
		message     string
		isExhausted bool
	}

	tests := map[string]parameters{
		"matches subnet preflight exhaustion": {
			message:     "Subnet 11111111-1111-1111-1111-111111111111 does not have enough IP addresses: 0 of 1 IP addresses remain available",
			isExhausted: true,
		},
		"matches vpc prefix preflight exhaustion": {
			message:     "VPC Prefix 22222222-2222-2222-2222-222222222222 does not have enough IP addresses: 1 of 2 IP addresses remain available",
			isExhausted: true,
		},
		"matches allocator prefix exhaustion": {
			message:     "rpc error: code = ResourceExhausted desc = Prefix: 10.45.144.20 has exhausted all address space",
			isExhausted: true,
		},
		"matches forge ip address exhaustion": {
			message:     "Internal error: IP address exhausted: 7.243.35.0/30",
			isExhausted: true,
		},
		"ignores unrelated create failure": {
			message:     "500 Internal Server Error: failed to create instance",
			isExhausted: false,
		},
		"requires subnet or vpc prefix for preflight phrase": {
			message:     "Network does not have enough IP addresses: 0 of 1 IP addresses remain available",
			isExhausted: false,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, params.isExhausted, isNicoNetworkExhaustionMessage(params.message))
		})
	}
}

func TestActivities_ShouldRotateNicoMachineNetworkCandidate(t *testing.T) {
	vpcPrefixID := uuid.New()
	subnetExhaustionMessage := "Subnet 11111111-1111-1111-1111-111111111111 " +
		"does not have enough IP addresses: 0 of 1 IP addresses remain available, " +
		"but the 1 interface(s) in this request require 1 IP address(es)"
	vpcPrefixExhaustionMessage := "VPC Prefix 22222222-2222-2222-2222-222222222222 " +
		"does not have enough IP addresses: 1 of 2 IP addresses remain available, " +
		"but the 1 interface(s) in this request require 2 IP addresses"
	allocatorExhaustionMessage := `500 Internal Server Error: {"source":"forge","message":"` +
		`Failed to execute sync workflow to create Instance on Site: rpc error: code = ResourceExhausted desc = ` +
		`Prefix: 10.45.144.20 has exhausted all address space","data":null}`
	forgeSubnetExhaustionMessage := `500 Internal Server Error: {"source":"forge","message":"` +
		`Failed to execute sync workflow to create Instance on Site: workflow execution error: ` +
		`rpc error: code = Internal desc = Error in DHCP allocation/handling: ` +
		`Prefix: 10.45.144.20 has exhausted all address space (type: Error, retryable: true)","data":null}`
	forgeVpcPrefixExhaustionMessage := `500 Internal Server Error: {"source":"forge","message":"` +
		`Failed to execute sync workflow to create Instance on Site: workflow execution error: ` +
		`rpc error: code = Internal desc = Internal error: IP address exhausted: 7.243.35.0/30 ` +
		`(type: Error, retryable: true)","data":null}`

	type parameters struct {
		nicoMachine  *nicov1.NicoMachine
		shouldRotate bool
	}

	tests := map[string]parameters{
		"returns true for fresh network exhaustion": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: vpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            vpcPrefixExhaustionMessage,
			}),
			shouldRotate: true,
		},
		"returns true for fresh subnet exhaustion": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					SubnetID: uuid.NewString(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            subnetExhaustionMessage,
			}),
			shouldRotate: true,
		},
		"returns true for allocator exhaustion": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					SubnetID: uuid.NewString(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            allocatorExhaustionMessage,
			}),
			shouldRotate: true,
		},
		"returns true for forge subnet exhaustion": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					SubnetID: uuid.NewString(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            forgeSubnetExhaustionMessage,
			}),
			shouldRotate: true,
		},
		"returns true for forge vpc prefix exhaustion": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: vpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            forgeVpcPrefixExhaustionMessage,
			}),
			shouldRotate: true,
		},
		"returns false for non exhaustion create failure": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: vpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            "500 Internal Server Error: no available ip in subnet",
			}),
			shouldRotate: false,
		},
		"returns false for stale observed generation": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: vpcPrefixID.String(),
				},
				Generation:         2,
				ObservedGeneration: 1,
				Message:            vpcPrefixExhaustionMessage,
			}),
			shouldRotate: false,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			a := &Activities{k8sClient: newNicoMachineRotationClient(t, params.nicoMachine)}

			shouldRotate, err := a.shouldRotateNicoMachineNetworkCandidate(context.Background(), testCAPINicoMachine())
			require.NoError(t, err)
			assert.Equal(t, params.shouldRotate, shouldRotate)
		})
	}
}

func TestActivities_RotateNicoMachineNetworkCandidate(t *testing.T) {
	firstVpcPrefixID := uuid.New()
	secondVpcPrefixID := uuid.New()
	missingVpcPrefixID := uuid.New()
	firstSubnetID := uuid.New()
	secondSubnetID := uuid.New()
	subnetExhaustionMessage := "Subnet " + firstSubnetID.String() +
		" does not have enough IP addresses: 0 of 1 IP addresses remain available, " +
		"but the 1 interface(s) in this request require 1 IP address(es)"
	vpcPrefixExhaustionMessage := "VPC Prefix " + firstVpcPrefixID.String() +
		" does not have enough IP addresses: 1 of 2 IP addresses remain available, " +
		"but the 1 interface(s) in this request require 2 IP addresses"

	type parameters struct {
		nicoMachine       *nicov1.NicoMachine
		nodeSet           db.NodeSet
		wantVpcPrefixID   string
		wantSubnetID      string
		wantErrorContains []string
	}

	nicoNodeSet := func(subnetIDs, vpcPrefixIDs []uuid.UUID) db.NodeSet {
		var nodeSet db.NodeSet
		nodeSet.SetNico(db.NicoNodePoolProvider{SubnetIDs: subnetIDs, VpcPrefixIDs: vpcPrefixIDs})
		return nodeSet
	}

	tests := map[string]parameters{
		"rotates vpc prefix to next candidate": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            vpcPrefixExhaustionMessage,
			}),
			nodeSet:         nicoNodeSet(nil, []uuid.UUID{firstVpcPrefixID, secondVpcPrefixID}),
			wantVpcPrefixID: secondVpcPrefixID.String(),
		},
		"rotates subnet to next candidate": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					SubnetID: firstSubnetID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            subnetExhaustionMessage,
			}),
			nodeSet:      nicoNodeSet([]uuid.UUID{firstSubnetID, secondSubnetID}, nil),
			wantSubnetID: secondSubnetID.String(),
		},
		"ignores vpc prefix when no vpc prefix candidates are configured": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: firstVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            vpcPrefixExhaustionMessage,
			}),
			nodeSet:         nicoNodeSet(nil, nil),
			wantVpcPrefixID: firstVpcPrefixID.String(),
		},
		"ignores subnet when no subnet candidates are configured": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					SubnetID: firstSubnetID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            subnetExhaustionMessage,
			}),
			nodeSet:      nicoNodeSet(nil, nil),
			wantSubnetID: firstSubnetID.String(),
		},
		"errors when vpc prefix is not configured candidate": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: missingVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            vpcPrefixExhaustionMessage,
			}),
			nodeSet:         nicoNodeSet(nil, []uuid.UUID{firstVpcPrefixID, secondVpcPrefixID}),
			wantVpcPrefixID: missingVpcPrefixID.String(),
			wantErrorContains: []string{
				"current NicoMachine VPC prefix",
				"is not in configured VPC prefix candidates",
			},
		},
		"errors when last subnet candidate is exhausted": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					SubnetID: secondSubnetID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            subnetExhaustionMessage,
			}),
			nodeSet:      nicoNodeSet([]uuid.UUID{firstSubnetID, secondSubnetID}, nil),
			wantSubnetID: secondSubnetID.String(),
			wantErrorContains: []string{
				"all 2 subnets are exhausted",
			},
		},
		"errors when last vpc prefix candidate is exhausted": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Interface: nicov1.NicoMachineInterface{
					VPCPrefixID: secondVpcPrefixID.String(),
				},
				Generation:         1,
				ObservedGeneration: 1,
				Message:            vpcPrefixExhaustionMessage,
			}),
			nodeSet:         nicoNodeSet(nil, []uuid.UUID{firstVpcPrefixID, secondVpcPrefixID}),
			wantVpcPrefixID: secondVpcPrefixID.String(),
			wantErrorContains: []string{
				"all 2 VPC prefixes are exhausted",
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			k8sClient := newNicoMachineRotationClient(t, params.nicoMachine)
			a := &Activities{k8sClient: k8sClient}

			err := a.rotateNicoMachineNetworkCandidate(context.Background(), params.nodeSet, testCAPINicoMachine())
			for _, errMessage := range params.wantErrorContains {
				require.ErrorContains(t, err, errMessage)
			}
			if len(params.wantErrorContains) == 0 {
				require.NoError(t, err)
			}

			nicoMachine := &nicov1.NicoMachine{}
			require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
				Namespace: "capi-cluster-a",
				Name:      "worker-0",
			}, nicoMachine))
			require.Len(t, nicoMachine.Spec.Interfaces, 1)
			assert.Equal(t, params.wantVpcPrefixID, nicoMachine.Spec.Interfaces[0].VPCPrefixID)
			assert.Equal(t, params.wantSubnetID, nicoMachine.Spec.Interfaces[0].SubnetID)
		})
	}
}

func TestActivities_ShouldRotateNicoMachineInstanceType(t *testing.T) {
	instanceTypeID := uuid.NewString()

	type parameters struct {
		nicoMachine  *nicov1.NicoMachine
		shouldRotate bool
	}

	tests := map[string]parameters{
		"returns true for instance type unavailable": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				InstanceTypeID:     instanceTypeID,
				Reason:             nicov1.InstanceTypeUnavailableReason,
				Generation:         1,
				ObservedGeneration: 1,
				Message:            "instance type has no available capacity",
			}),
			shouldRotate: true,
		},
		"returns true for instance type not found": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				InstanceTypeID:     instanceTypeID,
				Reason:             nicov1.InstanceTypeNotFoundReason,
				Generation:         1,
				ObservedGeneration: 1,
				Message:            "instance type does not exist",
			}),
			shouldRotate: true,
		},
		"returns false for transient availability check failure": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				InstanceTypeID:     instanceTypeID,
				Reason:             nicov1.AvailabilityCheckFailedReason,
				Generation:         1,
				ObservedGeneration: 1,
				Message:            "failed to reach NICo API",
			}),
			shouldRotate: false,
		},
		"returns false for network create failure": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				InstanceTypeID:     instanceTypeID,
				Reason:             nicov1.InstanceCreateFailedReason,
				Generation:         1,
				ObservedGeneration: 1,
				Message:            "IP address exhausted",
			}),
			shouldRotate: false,
		},
		"returns false when instance type is not set": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				Reason:             nicov1.InstanceTypeUnavailableReason,
				Generation:         1,
				ObservedGeneration: 1,
				Message:            "instance type has no available capacity",
			}),
			shouldRotate: false,
		},
		"returns false for stale observed generation": {
			nicoMachine: testNicoMachineForRotation(nicoMachineRotationParameters{
				InstanceTypeID:     instanceTypeID,
				Reason:             nicov1.InstanceTypeUnavailableReason,
				Generation:         2,
				ObservedGeneration: 1,
				Message:            "instance type has no available capacity",
			}),
			shouldRotate: false,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			a := &Activities{k8sClient: newNicoMachineRotationClient(t, params.nicoMachine)}

			shouldRotate, err := a.shouldRotateNicoMachineInstanceType(context.Background(), testCAPINicoMachine())
			require.NoError(t, err)
			assert.Equal(t, params.shouldRotate, shouldRotate)
		})
	}
}

func TestActivities_RotateNicoMachineInstanceType(t *testing.T) {
	firstInstanceTypeID := uuid.New()
	secondInstanceTypeID := uuid.New()
	thirdInstanceTypeID := uuid.New()
	missingInstanceTypeID := uuid.New()
	configurationID := uuid.New()
	clusterID := uuid.New()
	unavailableMessage := "instance type has no available capacity"

	type parameters struct {
		node               *db.Node
		instanceTypeID     string
		controlPlaneConfig db.ControlPlaneConfigSpec
		wantInstanceTypeID string
		wantErrorContains  []string
	}

	controlPlaneNode := func() *db.Node {
		return &db.Node{
			ID:                          uuid.New(),
			ClusterID:                   clusterID,
			ControlPlane:                true,
			ControlPlaneConfigurationID: configurationID,
			InstanceTypeID:              firstInstanceTypeID,
		}
	}

	tests := map[string]parameters{
		"rotates NicoMachine spec to next preferred instance type without touching node db": {
			node:               controlPlaneNode(),
			instanceTypeID:     firstInstanceTypeID.String(),
			controlPlaneConfig: nicoControlPlaneConfigSpec([]uuid.UUID{firstInstanceTypeID, secondInstanceTypeID, thirdInstanceTypeID}),
			wantInstanceTypeID: secondInstanceTypeID.String(),
		},
		"wraps back to the most preferred type when the list is exhausted": {
			node:               controlPlaneNode(),
			instanceTypeID:     secondInstanceTypeID.String(),
			controlPlaneConfig: nicoControlPlaneConfigSpec([]uuid.UUID{firstInstanceTypeID, secondInstanceTypeID}),
			wantInstanceTypeID: firstInstanceTypeID.String(),
		},
		"retries the same type in place for a single-entry preference": {
			node:               controlPlaneNode(),
			instanceTypeID:     firstInstanceTypeID.String(),
			controlPlaneConfig: nicoControlPlaneConfigSpec([]uuid.UUID{firstInstanceTypeID}),
			wantInstanceTypeID: firstInstanceTypeID.String(),
		},
		"errors when current instance type is not in preference": {
			node:               controlPlaneNode(),
			instanceTypeID:     missingInstanceTypeID.String(),
			controlPlaneConfig: nicoControlPlaneConfigSpec([]uuid.UUID{firstInstanceTypeID, secondInstanceTypeID}),
			wantInstanceTypeID: missingInstanceTypeID.String(),
			wantErrorContains:  []string{"is not in configured instance type preference"},
		},
		"errors when pinned control plane config has only legacy flat fields": {
			node:               controlPlaneNode(),
			instanceTypeID:     firstInstanceTypeID.String(),
			controlPlaneConfig: db.ControlPlaneConfigSpec{InstanceTypePreference: []uuid.UUID{firstInstanceTypeID, secondInstanceTypeID}},
			wantInstanceTypeID: firstInstanceTypeID.String(),
			wantErrorContains:  []string{"nico control-plane provider payload is required"},
		},
		"no-op when node has no pinned control plane configuration": {
			node: &db.Node{
				ID:             uuid.New(),
				ControlPlane:   true,
				InstanceTypeID: firstInstanceTypeID,
			},
			instanceTypeID:     firstInstanceTypeID.String(),
			wantInstanceTypeID: firstInstanceTypeID.String(),
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			nicoMachine := testNicoMachineForRotation(nicoMachineRotationParameters{
				InstanceTypeID:     params.instanceTypeID,
				Reason:             nicov1.InstanceTypeUnavailableReason,
				Generation:         1,
				ObservedGeneration: 1,
				Message:            unavailableMessage,
			})
			k8sClient := newNicoMachineRotationClient(t, nicoMachine)

			mockDB := &testutil.MockDB{}
			mockTx := &testutil.MockTx{}
			mockDB.MockTx = mockTx
			if params.node.ControlPlaneConfigurationID != uuid.Nil {
				mockDB.On("GetControlPlaneConfiguration", mock.Anything, params.node.ControlPlaneConfigurationID).
					Return(db.ControlPlaneConfiguration{
						ID:     configurationID,
						Config: params.controlPlaneConfig,
					}, nil)
			}
			a := &Activities{db: mockDB, k8sClient: k8sClient}

			err := a.rotateNicoMachineInstanceType(context.Background(), *params.node, testCAPINicoMachine())
			for _, errMessage := range params.wantErrorContains {
				require.ErrorContains(t, err, errMessage)
			}
			if len(params.wantErrorContains) == 0 {
				require.NoError(t, err)
			}

			rotated := &nicov1.NicoMachine{}
			require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
				Namespace: "capi-cluster-a",
				Name:      "worker-0",
			}, rotated))
			assert.Equal(t, params.wantInstanceTypeID, rotated.Spec.InstanceTypeID)

			// Nico owns the instance type: rotation only patches the NicoMachine
			// spec, it never writes back to the node record.
			mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockDB.AssertExpectations(t)
			mockTx.AssertExpectations(t)
		})
	}
}

func TestActivities_SeedControlPlaneInstanceType(t *testing.T) {
	t.Parallel()

	preferred := uuid.New()
	second := uuid.New()
	persisted := uuid.New()
	configurationID := uuid.New()
	clusterID := uuid.New()

	type parameters struct {
		node               db.Node
		nodePool           db.NodePool
		preference         []uuid.UUID
		expectDBLookup     bool
		wantInstanceTypeID uuid.UUID
	}

	tests := map[string]parameters{
		"seeds first preference for a type-less control plane node": {
			node:               db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, ControlPlaneConfigurationID: configurationID},
			nodePool:           db.NodePool{},
			preference:         []uuid.UUID{preferred, second},
			expectDBLookup:     true,
			wantInstanceTypeID: preferred,
		},
		"keeps an already-resolved instance type": {
			node:               db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, ControlPlaneConfigurationID: configurationID},
			nodePool:           db.NodePool{InstanceTypeID: persisted},
			expectDBLookup:     false,
			wantInstanceTypeID: persisted,
		},
		"no-op for a non-control-plane node": {
			node:               db.Node{ID: uuid.New()},
			nodePool:           db.NodePool{},
			expectDBLookup:     false,
			wantInstanceTypeID: uuid.Nil,
		},
		"stays nil when the pinned preference is empty": {
			node:               db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, ControlPlaneConfigurationID: configurationID},
			nodePool:           db.NodePool{},
			preference:         nil,
			expectDBLookup:     true,
			wantInstanceTypeID: uuid.Nil,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			nodePool := params.nodePool
			nodePool.SetNico(db.NicoNodePoolProvider{
				InstanceTypeID: nodePool.InstanceTypeID,
			})

			mockDB := &testutil.MockDB{}
			if params.expectDBLookup {
				mockDB.On("GetControlPlaneConfiguration", mock.Anything, params.node.ControlPlaneConfigurationID).
					Return(db.ControlPlaneConfiguration{
						ID:     configurationID,
						Config: nicoControlPlaneConfigSpec(params.preference),
					}, nil).Once()
			}
			a := &Activities{db: mockDB}

			createCtx := &capiMachineCreateContext{node: params.node, nodePool: nodePool}
			require.NoError(t, a.seedControlPlaneInstanceType(context.Background(), createCtx))

			assert.Equal(t, params.wantInstanceTypeID, createCtx.nodePool.InstanceTypeID)
			if !params.expectDBLookup {
				mockDB.AssertNotCalled(t, "GetControlPlaneConfiguration", mock.Anything, mock.Anything)
			}
			mockDB.AssertExpectations(t)
		})
	}
}

func nicoControlPlaneConfigSpec(instanceTypePreference []uuid.UUID) db.ControlPlaneConfigSpec {
	return db.ControlPlaneConfigSpec{
		Nico: &db.NicoControlPlaneProvider{
			InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
		},
		InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
	}
}
