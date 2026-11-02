// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

var (
	walkPrefixA = uuid.MustParse("71000000-0000-0000-0000-00000000000a")
	walkPrefixB = uuid.MustParse("71000000-0000-0000-0000-00000000000b")
	walkPrefixC = uuid.MustParse("71000000-0000-0000-0000-00000000000c")
	walkPool    = uuid.MustParse("71000000-0000-0000-0000-000000000001")
	otherPool   = uuid.MustParse("71000000-0000-0000-0000-000000000002")
)

const walkExhaustionMessage = "Internal error: IP address exhausted: 7.243.43.64/26 (type: Error, retryable: true)"

type walkNicoMachineParameters struct {
	name           string
	nodePoolID     uuid.UUID
	vpcPrefixID    uuid.UUID
	provisioned    metav1.ConditionStatus
	provisionedAt  time.Time
	walkCandidates []uuid.UUID
	walkStart      *uuid.UUID
	allFull        bool
	reason         string
	message        string
	generation     int64
	observedGen    int64
	clusterName    string
}

func testWalkNicoMachine(params walkNicoMachineParameters) *nicov1.NicoMachine {
	clusterName := params.clusterName
	if clusterName == "" {
		clusterName = "cluster-a"
	}
	annotations := map[string]string{}
	if len(params.walkCandidates) > 0 {
		annotations[keys.VPCPrefixWalkCandidates.String()] = joinVpcPrefixIDs(params.walkCandidates)
		annotations[keys.VPCPrefixWalkStart.String()] = params.walkCandidates[0].String()
	}
	if params.walkStart != nil {
		annotations[keys.VPCPrefixWalkStart.String()] = params.walkStart.String()
	}
	if params.allFull {
		annotations[keys.VPCPrefixesAllFull.String()] = "2026-10-29T00:00:00Z"
	}
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: nicov1.GroupVersion.String(), Kind: "NicoMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        params.name,
			Namespace:   constants.LegacyManagementNamespace,
			Labels:      shared.MachineLabels(clusterName, false),
			Annotations: annotations,
			Generation:  params.generation,
		},
		Spec: nicov1.NicoMachineSpec{
			Interfaces: []nicov1.NicoMachineInterface{{VPCPrefixID: params.vpcPrefixID.String()}},
			Labels:     map[string]string{keys.NodePoolID.String(): params.nodePoolID.String()},
		},
		Status: nicov1.NicoMachineStatus{
			Conditions: []metav1.Condition{{
				Type:               nicov1.MachineProvisionedCondition,
				Status:             params.provisioned,
				Reason:             params.reason,
				Message:            params.message,
				ObservedGeneration: params.observedGen,
				LastTransitionTime: metav1.NewTime(params.provisionedAt),
			}},
		},
	}
}

func TestStartVpcPrefix(t *testing.T) {
	earlier := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	candidates := []uuid.UUID{walkPrefixA, walkPrefixB, walkPrefixC}
	newWalk := joinVpcPrefixIDs(candidates)

	provisionedSibling := func(name string, pool, prefix uuid.UUID, at time.Time) *nicov1.NicoMachine {
		return testWalkNicoMachine(walkNicoMachineParameters{
			name: name, nodePoolID: pool, vpcPrefixID: prefix, provisioned: metav1.ConditionTrue, provisionedAt: at,
		})
	}

	tests := map[string]struct {
		objects        []ctrlclient.Object
		wantPrefix     uuid.UUID
		wantWalkStart  string
		wantCandidates string
	}{
		"starts on the first candidate with no siblings": {
			wantPrefix:     walkPrefixA,
			wantWalkStart:  walkPrefixA.String(),
			wantCandidates: newWalk,
		},
		"starts where the node pool's most recent node landed": {
			objects: []ctrlclient.Object{
				provisionedSibling("w-1", walkPool, walkPrefixB, earlier),
				provisionedSibling("w-2", walkPool, walkPrefixC, later),
			},
			wantPrefix:     walkPrefixC,
			wantWalkStart:  walkPrefixC.String(),
			wantCandidates: newWalk,
		},
		"ignores siblings from another node pool": {
			objects: []ctrlclient.Object{
				provisionedSibling("w-1", walkPool, walkPrefixB, earlier),
				provisionedSibling("w-2", otherPool, walkPrefixC, later),
			},
			wantPrefix:     walkPrefixB,
			wantWalkStart:  walkPrefixB.String(),
			wantCandidates: newWalk,
		},
		"ignores siblings from another cluster": {
			objects: []ctrlclient.Object{
				testWalkNicoMachine(walkNicoMachineParameters{
					name: "w-1", nodePoolID: walkPool, vpcPrefixID: walkPrefixC, provisioned: metav1.ConditionTrue,
					provisionedAt: later, clusterName: "cluster-b",
				}),
			},
			wantPrefix:     walkPrefixA,
			wantWalkStart:  walkPrefixA.String(),
			wantCandidates: newWalk,
		},
		"ignores siblings that are not provisioned": {
			objects: []ctrlclient.Object{
				testWalkNicoMachine(walkNicoMachineParameters{
					name: "w-1", nodePoolID: walkPool, vpcPrefixID: walkPrefixC, provisioned: metav1.ConditionFalse,
					provisionedAt: later, reason: nicov1.InstanceCreateFailedReason, message: walkExhaustionMessage,
				}),
			},
			wantPrefix:     walkPrefixA,
			wantWalkStart:  walkPrefixA.String(),
			wantCandidates: newWalk,
		},
		"ignores siblings on a prefix that is no longer a candidate": {
			objects: []ctrlclient.Object{
				provisionedSibling("w-1", walkPool, uuid.MustParse("71000000-0000-0000-0000-0000000000ff"), later),
			},
			wantPrefix:     walkPrefixA,
			wantWalkStart:  walkPrefixA.String(),
			wantCandidates: newWalk,
		},
		"an existing NicoMachine keeps its rotated prefix and walk record": {
			objects: []ctrlclient.Object{
				testWalkNicoMachine(walkNicoMachineParameters{
					name: "node-0", nodePoolID: walkPool, vpcPrefixID: walkPrefixC, provisioned: metav1.ConditionFalse,
					walkCandidates: []uuid.UUID{walkPrefixA, walkPrefixB}, walkStart: &walkPrefixB,
				}),
				provisionedSibling("w-1", walkPool, walkPrefixA, later),
			},
			wantPrefix:     walkPrefixC,
			wantWalkStart:  walkPrefixB.String(),
			wantCandidates: walkPrefixA.String() + "," + walkPrefixB.String(),
		},
		"an existing NicoMachine with no walk record gets none, so its next rotation starts one": {
			objects: []ctrlclient.Object{
				testWalkNicoMachine(walkNicoMachineParameters{
					name: "node-0", nodePoolID: walkPool, vpcPrefixID: walkPrefixB, provisioned: metav1.ConditionFalse,
				}),
			},
			wantPrefix: walkPrefixB,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			k8sClient := newMachineCreateClient(t, tt.objects...)
			rendered := testWalkNicoMachine(walkNicoMachineParameters{name: "node-0", nodePoolID: walkPool, vpcPrefixID: walkPrefixA})

			prefix, annotations, err := startVpcPrefix(context.Background(), k8sClient, rendered, walkPool, candidates)
			require.NoError(t, err)
			assert.Equal(t, tt.wantPrefix.String(), prefix)
			assert.Equal(t, tt.wantWalkStart, keys.VPCPrefixWalkStart.Value(annotations))
			assert.Equal(t, tt.wantCandidates, keys.VPCPrefixWalkCandidates.Value(annotations))
		})
	}
}

// The start choice has to reach the applied NicoMachine, and a retried create
// has to leave a rotated NicoMachine where the rotation put it.
func TestClient_CreateMachine_VpcPrefixStart(t *testing.T) {
	const release = "test-os"
	sibling := testWalkNicoMachine(walkNicoMachineParameters{
		name: "w-1", nodePoolID: walkPool, vpcPrefixID: walkPrefixC, provisioned: metav1.ConditionTrue,
		provisionedAt: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC),
	})
	objects := append([]ctrlclient.Object{sibling}, testNodePoolRelease(release, "#!ipxe\n", "#cloud-config\n")...)
	k8sClient := newMachineCreateClient(t, objects...)

	nodeSet := db.NodeSet{KubernetesVersion: "1.32.1"}
	nodeSet.SetNico(db.NicoNodePoolProvider{Release: release, DiskSelectionStrategy: db.DiskSelectionStrategySmallest})
	nodePool := db.NodePool{ID: walkPool}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID: uuid.MustParse("70000000-0000-0000-0000-000000000005"),
		VpcPrefixIDs:   []uuid.UUID{walkPrefixA, walkPrefixB, walkPrefixC},
	})
	cluster := db.Cluster{Name: "cluster-a", CniPlugin: db.CNICalico}
	cluster.SetNicoProvider(db.NicoClusterProvider{VpcID: uuid.MustParse("70000000-0000-0000-0000-000000000006")})
	node := db.Node{Name: "node-0", Nico: &db.NicoNodeProvider{}}

	ctx := context.Background()
	getNode := func() *nicov1.NicoMachine {
		nicoMachine := &nicov1.NicoMachine{}
		require.NoError(t, k8sClient.Get(ctx, ctrlclient.ObjectKey{Namespace: constants.LegacyManagementNamespace, Name: "node-0"}, nicoMachine))
		return nicoMachine
	}

	require.NoError(t, New().CreateMachine(ctx, testMachineCreateDeps(k8sClient), node, nodeSet, nodePool, cluster))
	created := getNode()
	assert.Equal(t, walkPrefixC.String(), created.Spec.Interfaces[0].VPCPrefixID)
	assert.Equal(t, walkPrefixC.String(), keys.VPCPrefixWalkStart.Value(created.Annotations))
	assert.Equal(t, joinVpcPrefixIDs(nodePool.Nico.VpcPrefixIDs), keys.VPCPrefixWalkCandidates.Value(created.Annotations))

	patch := ctrlclient.MergeFrom(created.DeepCopy())
	created.Spec.Interfaces[0].VPCPrefixID = walkPrefixA.String()
	require.NoError(t, k8sClient.Patch(ctx, created, patch))

	require.NoError(t, New().CreateMachine(ctx, testMachineCreateDeps(k8sClient), node, nodeSet, nodePool, cluster))
	retried := getNode()
	assert.Equal(t, walkPrefixA.String(), retried.Spec.Interfaces[0].VPCPrefixID)
	assert.Equal(t, walkPrefixC.String(), keys.VPCPrefixWalkStart.Value(retried.Annotations))
}

func TestDescribeVpcPrefixWalk(t *testing.T) {
	candidates := []uuid.UUID{walkPrefixA, walkPrefixB, walkPrefixC}
	allFullMessage := "All 3 configured VPC prefixes have no free IP addresses (" +
		walkPrefixA.String() + ", " + walkPrefixB.String() + ", " + walkPrefixC.String() +
		"). Attach a VPC prefix with free IP addresses to the node pool; the controller keeps retrying."

	failing := func(prefix uuid.UUID, generation, observed int64) walkNicoMachineParameters {
		return walkNicoMachineParameters{
			name: "node-0", nodePoolID: walkPool, vpcPrefixID: prefix, provisioned: metav1.ConditionFalse,
			reason: nicov1.InstanceCreateFailedReason, message: walkExhaustionMessage, generation: generation, observedGen: observed,
			walkCandidates: candidates,
		}
	}
	with := func(params walkNicoMachineParameters, edit func(*walkNicoMachineParameters)) walkNicoMachineParameters {
		edit(&params)
		return params
	}

	tests := map[string]struct {
		params     walkNicoMachineParameters
		candidates []uuid.UUID
		want       string
	}{
		"full first candidate says it is trying the second": {
			params: failing(walkPrefixA, 1, 1),
			want:   "VPC prefix " + walkPrefixA.String() + " has no free IP addresses; trying the next configured VPC prefix (2 of 3).",
		},
		"rotated and not yet retried keeps naming the full candidate": {
			params: failing(walkPrefixB, 2, 1),
			want:   "VPC prefix " + walkPrefixA.String() + " has no free IP addresses; trying the next configured VPC prefix (2 of 3).",
		},
		"rotated after a wrap names the last candidate as full": {
			params: with(failing(walkPrefixA, 2, 1), func(p *walkNicoMachineParameters) { p.walkStart = &walkPrefixC }),
			want:   "VPC prefix " + walkPrefixC.String() + " has no free IP addresses; trying the next configured VPC prefix (2 of 3).",
		},
		"rotated onto a restarted walk's first candidate says which candidate it is trying": {
			params: with(failing(walkPrefixB, 2, 1), func(p *walkNicoMachineParameters) { p.walkStart = &walkPrefixB }),
			want:   "Trying configured VPC prefix " + walkPrefixB.String() + " (1 of 3); the previous one has no free IP addresses.",
		},
		"full last candidate of the walk says all are full": {
			params: failing(walkPrefixC, 3, 3),
			want:   allFullMessage,
		},
		"position counts from the recorded walk start": {
			params: with(failing(walkPrefixC, 1, 1), func(p *walkNicoMachineParameters) { p.walkStart = &walkPrefixB }),
			want:   "VPC prefix " + walkPrefixC.String() + " has no free IP addresses; trying the next configured VPC prefix (3 of 3).",
		},
		"after a full pass the message stays all full while it retries": {
			params: with(failing(walkPrefixB, 5, 4), func(p *walkNicoMachineParameters) { p.allFull = true }),
			want:   allFullMessage,
		},
		"single candidate says that one is full": {
			params:     with(failing(walkPrefixA, 1, 1), func(p *walkNicoMachineParameters) { p.walkCandidates = []uuid.UUID{walkPrefixA} }),
			candidates: []uuid.UUID{walkPrefixA},
			want: "The configured VPC prefix " + walkPrefixA.String() +
				" has no free IP addresses. Attach a VPC prefix with free IP addresses to the node pool; the controller keeps retrying.",
		},
		"a failure that is not exhaustion keeps the raw message": {
			params: with(failing(walkPrefixA, 1, 1), func(p *walkNicoMachineParameters) {
				p.message = `Instance type "" has no unused usable allocations`
			}),
		},
		"a non-create failure keeps the raw message": {
			params: with(failing(walkPrefixA, 1, 1), func(p *walkNicoMachineParameters) { p.reason = nicov1.InstanceTypeUnavailableReason }),
		},
		"a provisioned machine has nothing to say": {
			params: with(failing(walkPrefixA, 1, 1), func(p *walkNicoMachineParameters) { p.provisioned = metav1.ConditionTrue }),
		},
		"a prefix the node pool no longer lists says where it moves": {
			params: failing(uuid.MustParse("71000000-0000-0000-0000-0000000000ff"), 1, 1),
			want: "VPC prefix 71000000-0000-0000-0000-0000000000ff is no longer configured on the node pool; " +
				"trying configured VPC prefix " + walkPrefixA.String() + " (1 of 3).",
		},
		"a changed node pool list restarts the count after the current prefix": {
			params: with(failing(walkPrefixB, 1, 1), func(p *walkNicoMachineParameters) {
				p.walkCandidates = []uuid.UUID{walkPrefixA, walkPrefixB}
				p.allFull = true
			}),
			want: "VPC prefix " + walkPrefixB.String() + " has no free IP addresses; trying the next configured VPC prefix (1 of 3).",
		},
		"a changed node pool list while waiting on a retry names the prefix without a count": {
			params: with(failing(walkPrefixB, 2, 1), func(p *walkNicoMachineParameters) {
				p.walkCandidates = []uuid.UUID{walkPrefixA, walkPrefixB}
			}),
			want: "Trying configured VPC prefix " + walkPrefixB.String() + "; the previous one has no free IP addresses.",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			testCandidates := tt.candidates
			if testCandidates == nil {
				testCandidates = candidates
			}
			assert.Equal(t, tt.want, describeVpcPrefixWalk(testWalkNicoMachine(tt.params), testCandidates))
		})
	}
}

func TestClient_DescribeStuckMachine(t *testing.T) {
	nicoMachine := testWalkNicoMachine(walkNicoMachineParameters{
		name: "worker-0", nodePoolID: walkPool, vpcPrefixID: walkPrefixA, provisioned: metav1.ConditionFalse,
		reason: nicov1.InstanceCreateFailedReason, message: walkExhaustionMessage, generation: 1, observedGen: 1,
		walkCandidates: []uuid.UUID{walkPrefixA, walkPrefixB},
	})
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: constants.LegacyManagementNamespace},
		Spec: clusterv1.MachineSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: nicov1.GroupVersion.Group, Kind: "NicoMachine", Name: "worker-0"},
		},
	}

	tests := map[string]struct {
		vpcPrefixIDs []uuid.UUID
		subnetIDs    []uuid.UUID
		want         string
	}{
		"vpc prefix node pool gets the walk message": {
			vpcPrefixIDs: []uuid.UUID{walkPrefixA, walkPrefixB},
			want:         "VPC prefix " + walkPrefixA.String() + " has no free IP addresses; trying the next configured VPC prefix (2 of 2).",
		},
		"subnet node pool keeps the raw message": {
			subnetIDs: []uuid.UUID{uuid.New()},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var nodePool db.NodePool
			nodePool.SetNico(db.NicoNodePoolProvider{SubnetIDs: tt.subnetIDs, VpcPrefixIDs: tt.vpcPrefixIDs})
			deps := shared.MachineAdvanceDeps{K8sClient: newMachineCreateClient(t, nicoMachine.DeepCopy())}

			got, err := New().DescribeStuckMachine(context.Background(), deps, nodePool, machine)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
