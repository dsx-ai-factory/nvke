// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// dockerMachineTemplateManifest is a non-empty CAPI infra machine template used
// to mark a control-plane node as template-backed (rendered directly, no Forge
// instance-type selection required). Its exact contents are irrelevant to the
// scheduler; only capi.IsEmptyManifest's emptiness verdict matters.
var dockerMachineTemplateManifest = json.RawMessage(
	`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta1","kind":"DockerMachineTemplate","metadata":{"name":"cp"}}`,
)

// newCapiScheduler wires a NodeScheduler with EnableForgeCapi=true, a temporal
// client stub, and a MockDB/MockTx that persists selected instance types via
// RunInTx. It has no Forge provider: the CAPI path must never consult Forge
// capacity, so any such lookup would panic.
func newCapiScheduler() (*NodeScheduler, *temporalmocks.Client, *testutil.MockTx, *testutil.MockDB) {
	mockTx := &testutil.MockTx{}
	mockDB := &testutil.MockDB{GetTx: func() db.Tx { return mockTx }}
	temporalClient := &temporalmocks.Client{}
	scheduler := &NodeScheduler{
		cfg:               config.Config{EnableForgeCapi: true},
		logger:            slog.Default(),
		db:                mockDB,
		cweTemporalClient: temporalClient,
	}
	return scheduler, temporalClient, mockTx, mockDB
}

// expectProvisionNodeStart registers an ExecuteWorkflow expectation for the
// ProvisionNode workflow of the given node, matching the workflow ID the
// scheduler derives from the node ID.
func expectProvisionNodeStart(temporalClient *temporalmocks.Client, nodeID uuid.UUID) *mock.Call {
	return temporalClient.On(
		"ExecuteWorkflow",
		mock.Anything,
		mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
			return opts.ID == constants.ProvisionNodeWorkflowPrefix+nodeID.String()
		}),
		mock.Anything,
		mock.Anything,
	).Return(nil, nil)
}

// newSchedulerForSelection wires a NodeScheduler with a MockDB/MockTx that
// persists via RunInTx. It has no Forge provider, so it only supports paths that
// short-circuit before any Forge capacity lookup.
func newSchedulerForSelection() (*NodeScheduler, *testutil.MockTx, *testutil.MockDB) {
	mockTx := &testutil.MockTx{}
	mockDB := &testutil.MockDB{GetTx: func() db.Tx { return mockTx }}
	scheduler := &NodeScheduler{
		logger: slog.Default(),
		db:     mockDB,
	}
	return scheduler, mockTx, mockDB
}

func schedulerNicoCluster(cluster db.Cluster) db.Cluster {
	if cluster.OrgID == "" {
		cluster.OrgID = "org-1"
	}
	if cluster.SiteID == uuid.Nil {
		cluster.SiteID = uuid.New()
	}
	if cluster.VpcID == uuid.Nil {
		cluster.VpcID = uuid.New()
	}
	cluster.ProviderType = db.ProviderTypeNico
	cluster.Nico = &db.NicoClusterProvider{OrgID: cluster.OrgID, SiteID: cluster.SiteID, VpcID: cluster.VpcID}
	return cluster
}

func schedulerCAPICluster(cluster db.Cluster) db.Cluster {
	cluster.SiteID = uuid.Nil
	cluster.ProviderType = db.ProviderTypeCAPI
	cluster.Capi = &db.CAPIClusterProvider{InfraCluster: cluster.CapiInfraCluster}
	return cluster
}

// stubPinnedNodeContext makes nodes.GetNodeContext resolve a control-plane node
// pinned to cfgID: GetControlPlaneConfiguration returns a snapshot carrying the
// given placement/template, and GetCluster (also read by GetNodeContext for
// control-plane nodes) returns an empty cluster. Both are optional so a test's
// other, asserted expectations are unaffected.
func stubPinnedNodeContext(mockDB *testutil.MockDB, cfgID uuid.UUID, spec db.ControlPlaneConfigSpec) {
	mockDB.On("GetCluster", mock.Anything, mock.Anything).Return(db.Cluster{}, nil).Maybe()
	mockDB.On("GetControlPlaneConfiguration", mock.Anything, cfgID).
		Return(db.ControlPlaneConfiguration{ID: cfgID, Config: spec}, nil).Maybe()
}

func TestSelectControlPlaneInstanceType_CAPIUsesFirstPreference(t *testing.T) {
	t.Parallel()

	typeA := uuid.New()
	typeB := uuid.New()
	cluster := schedulerCAPICluster(db.Cluster{})

	scheduler := &NodeScheduler{logger: slog.Default()}
	acct := newControlPlaneCapacityAccount(scheduler, cluster, nil, slog.Default())

	got, err := scheduler.selectControlPlaneInstanceType(context.Background(), cluster, []uuid.UUID{typeA, typeB}, ForgePlacementSpec{}, acct)
	require.NoError(t, err)
	assert.Equal(t, typeA, got)
}

func TestSelectControlPlaneInstanceType_EmptyPreference(t *testing.T) {
	t.Parallel()

	cluster := schedulerNicoCluster(db.Cluster{})
	scheduler := &NodeScheduler{logger: slog.Default()}
	acct := newControlPlaneCapacityAccount(scheduler, cluster, nil, slog.Default())

	_, err := scheduler.selectControlPlaneInstanceType(context.Background(), cluster, nil, ForgePlacementSpec{}, acct)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no instance type preference configured")
}

func TestNicoControlPlaneInstanceTypePreference_RequiresProviderPayload(t *testing.T) {
	t.Parallel()

	instanceTypeID := uuid.New()

	got, err := nicoControlPlaneInstanceTypePreference(nicoControlPlaneConfigSpec([]uuid.UUID{instanceTypeID}))
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{instanceTypeID}, got)

	_, err = nicoControlPlaneInstanceTypePreference(db.ControlPlaneConfigSpec{
		InstanceTypePreference: []uuid.UUID{instanceTypeID},
	})
	require.ErrorContains(t, err, "nico control-plane provider payload is required")
}

// TestPersistControlPlaneInstanceType_KeepsExistingStickyType verifies the
// sticky guarantee at the persistence layer: a node that already carries a type
// keeps it and is never overwritten.
func TestPersistControlPlaneInstanceType_KeepsExistingStickyType(t *testing.T) {
	t.Parallel()

	existing := uuid.New()
	requested := uuid.New()

	mockTx := &testutil.MockTx{}
	mockDB := &testutil.MockDB{GetTx: func() db.Tx { return mockTx }}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	mockTx.On("GetNodeForUpdate", mock.Anything, mock.Anything).Return(db.Node{InstanceTypeID: existing}, nil).Once()

	scheduler := &NodeScheduler{logger: slog.Default(), db: mockDB}
	got, err := scheduler.persistControlPlaneInstanceType(context.Background(), uuid.New(), requested)
	require.NoError(t, err)
	assert.Equal(t, existing, got)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

// TestScheduleForgeControlPlaneNodes_NonNicoClusterIsSkipped asserts the
// defensive guard: only NICo-backed clusters use direct Forge scheduling.
func TestScheduleForgeControlPlaneNodes_NonNicoClusterIsSkipped(t *testing.T) {
	t.Parallel()

	scheduler, mockTx, mockDB := newSchedulerForSelection()
	cluster := schedulerCAPICluster(db.Cluster{ID: uuid.New()})
	node := db.Node{ID: uuid.New(), ControlPlane: true}

	require.NoError(t, scheduler.scheduleForgeControlPlaneNodes(context.Background(), slog.Default(), cluster, []db.Node{node}))
	mockDB.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

func TestScheduleControlPlaneNodesForCluster_NoPendingNodesReturns(t *testing.T) {
	t.Parallel()

	scheduler, mockTx, mockDB := newSchedulerForSelection()
	cluster := db.Cluster{ID: uuid.New(), ProviderType: db.ProviderType("")}

	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == cluster.ID &&
			filters.ControlPlane != nil && *filters.ControlPlane &&
			filters.Pending != nil && *filters.Pending
	})).Return([]db.Node{}, nil).Once()

	require.NoError(t, scheduler.scheduleControlPlaneNodesForCluster(context.Background(), db.Site{ID: uuid.New()}, cluster))
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
}

func TestScheduleNodePoolNodes_CAPIClusterStartsProvisionNode(t *testing.T) {
	t.Parallel()

	scheduler, temporalClient, mockTx, mockDB := newCapiScheduler()
	cluster := schedulerCAPICluster(db.Cluster{ID: uuid.New()})
	nodePool := db.NodePool{ID: uuid.New(), ClusterID: cluster.ID}
	node := db.Node{ID: uuid.New(), NodePoolID: nodePool.ID}

	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.NodePoolID != nil && *filters.NodePoolID == nodePool.ID &&
			filters.Pending != nil && *filters.Pending
	})).Return([]db.Node{node}, nil).Once()
	expectProvisionNodeStart(temporalClient, node.ID).Once()

	require.NoError(t, scheduler.scheduleNodePoolNodes(context.Background(), db.Site{ID: uuid.New()}, nodePool))
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
	temporalClient.AssertExpectations(t)
}

func TestScheduleForgeNodes_CAPIClusterIsSkipped(t *testing.T) {
	t.Parallel()

	scheduler, mockTx, mockDB := newSchedulerForSelection()
	cluster := schedulerCAPICluster(db.Cluster{ID: uuid.New()})
	node := db.Node{ID: uuid.New()}

	require.NoError(t, scheduler.scheduleForgeNodes(context.Background(), slog.Default(), cluster, []db.Node{node}, uuid.New(), ForgePlacementSpec{}))
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestScheduleForgeControlPlaneNodes_SkipsNodeWithUnresolvedContext asserts a
// node whose context cannot be resolved is skipped this pass without selection,
// persistence, or an aggregate error.
func TestScheduleForgeControlPlaneNodes_SkipsNodeWithUnresolvedContext(t *testing.T) {
	t.Parallel()

	cErr := uuid.New()
	scheduler, mockTx, mockDB := newSchedulerForSelection()
	mockDB.On("GetCluster", mock.Anything, cErr).Return(db.Cluster{}, errors.New("context lookup failed"))

	node := db.Node{ID: uuid.New(), ClusterID: cErr, ControlPlane: true}
	cluster := schedulerNicoCluster(db.Cluster{ID: uuid.New()})

	require.NoError(t, scheduler.scheduleForgeControlPlaneNodes(context.Background(), slog.Default(), cluster, []db.Node{node}))
	mockDB.AssertExpectations(t)
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestForgeNetworkPlacementKey_OrderIndependent asserts the placement key is
// canonical: the same subnet/prefix set yields one key regardless of input
// order, so nodes with the same placement share a single network counter instead
// of double-spending IP capacity through order-dependent keys.
func TestForgeNetworkPlacementKey_OrderIndependent(t *testing.T) {
	t.Parallel()

	subnetA, subnetB := uuid.New(), uuid.New()
	prefixA, prefixB := uuid.New(), uuid.New()

	forward := ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetA, subnetB}, VpcPrefixIDs: []uuid.UUID{prefixA, prefixB}}
	reversed := ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetB, subnetA}, VpcPrefixIDs: []uuid.UUID{prefixB, prefixA}}
	assert.Equal(t, forgeNetworkPlacementKey(forward), forgeNetworkPlacementKey(reversed),
		"same subnet/prefix set must share one key regardless of order")

	// A different set must still produce a different key.
	subset := ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetA}, VpcPrefixIDs: []uuid.UUID{prefixA, prefixB}}
	assert.NotEqual(t, forgeNetworkPlacementKey(forward), forgeNetworkPlacementKey(subset))
}

func TestScheduleControlPlaneNodes_CapiTemplatelessProvisionsWithoutForgePrecheck(t *testing.T) {
	t.Parallel()

	typeA := uuid.New()
	subnetID := uuid.New()
	cfgID := uuid.New()

	// On the Nico/CAPI path Nico owns instance-type availability, so CWE neither
	// pre-checks Forge capacity nor pre-fills a type. The scheduler has no Forge
	// provider, so any capacity lookup would panic; the node still provisions
	// unconditionally.
	scheduler, temporalClient, mockTx, mockDB := newCapiScheduler()
	stubPinnedNodeContext(mockDB, cfgID, db.ControlPlaneConfigSpec{SubnetIDs: []uuid.UUID{subnetID}})

	site := db.Site{ID: uuid.New()}
	cluster := schedulerNicoCluster(db.Cluster{
		ID:     uuid.New(),
		SiteID: site.ID,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: cfgID,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				InstanceTypePreference: []uuid.UUID{typeA},
			},
		},
	})
	node := db.Node{ID: uuid.New(), ControlPlane: true, ControlPlaneConfigurationID: cfgID}

	mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{cluster}, nil)
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{node}, nil)
	expectProvisionNodeStart(temporalClient, node.ID).Once()

	require.NoError(t, scheduler.scheduleControlPlaneNodes(context.Background(), site))

	// No type is selected or persisted; Nico seeds/owns it from the preference.
	assert.Equal(t, uuid.Nil, node.InstanceTypeID)
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
	temporalClient.AssertExpectations(t)
}

func TestScheduleControlPlaneNodes_CapiTemplateBackedProvisionsWithoutSelection(t *testing.T) {
	t.Parallel()

	cfgID := uuid.New()
	subnetID := uuid.New()

	// Template-backed nodes render from their pinned template, so selection is
	// skipped. The scheduler has no Forge provider, so any capacity lookup would
	// panic.
	scheduler, temporalClient, mockTx, mockDB := newCapiScheduler()
	// Template-backed node: non-empty CapiInfraMachineTemplate in the snapshot.
	stubPinnedNodeContext(mockDB, cfgID, db.ControlPlaneConfigSpec{
		SubnetIDs:                       []uuid.UUID{subnetID},
		InfraMachineTemplatePreferences: []json.RawMessage{dockerMachineTemplateManifest},
	})

	site := db.Site{ID: uuid.New()}
	cluster := schedulerNicoCluster(db.Cluster{
		ID:     uuid.New(),
		SiteID: site.ID,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: cfgID,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				InstanceTypePreference: []uuid.UUID{uuid.New()},
			},
		},
	})
	node := db.Node{ID: uuid.New(), ControlPlane: true, ControlPlaneConfigurationID: cfgID}

	mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{cluster}, nil)
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{node}, nil)
	expectProvisionNodeStart(temporalClient, node.ID).Once()

	require.NoError(t, scheduler.scheduleControlPlaneNodes(context.Background(), site))

	// Template-backed CAPI node: no instance-type selection or persistence, but
	// ProvisionNode still starts (InstanceTypeID stays unset).
	assert.Equal(t, uuid.Nil, node.InstanceTypeID)
	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	temporalClient.AssertExpectations(t)
}

func TestScheduleControlPlaneNodes_CapiTemplatelessDoesNotTouchExistingType(t *testing.T) {
	t.Parallel()

	typeA := uuid.New()
	typeB := uuid.New()
	subnetID := uuid.New()
	cfgID := uuid.New()

	// A node already carrying typeA must provision without the scheduler
	// consulting Forge capacity or persisting a (re)selected type: on the CAPI
	// path Nico owns the instance type, so CWE leaves the node record untouched.
	// The scheduler has no Forge provider, so any capacity lookup would panic.
	scheduler, temporalClient, mockTx, mockDB := newCapiScheduler()
	stubPinnedNodeContext(mockDB, cfgID, db.ControlPlaneConfigSpec{SubnetIDs: []uuid.UUID{subnetID}})

	site := db.Site{ID: uuid.New()}
	cluster := schedulerNicoCluster(db.Cluster{
		ID:     uuid.New(),
		SiteID: site.ID,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: cfgID,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				InstanceTypePreference: []uuid.UUID{typeB, typeA},
			},
		},
	})
	node := db.Node{ID: uuid.New(), ControlPlane: true, ControlPlaneConfigurationID: cfgID, InstanceTypeID: typeA}

	mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{cluster}, nil)
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{node}, nil)
	expectProvisionNodeStart(temporalClient, node.ID).Once()

	require.NoError(t, scheduler.scheduleControlPlaneNodes(context.Background(), site))

	mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	temporalClient.AssertExpectations(t)
}

// TestControlPlaneCapacityAccount_ReserveDebitsBothLedgers verifies the unified
// sticky-reserve path: reserving a sticky (already type-stamped) node draws down
// BOTH the Forge compute ledger and the placement's network (IP) ledger, so a
// later type-less selection in this pass can't double-spend either. The ledgers
// are pre-seeded here so reserve's lazy seeding short-circuits and no Forge
// provider is consulted.
func TestControlPlaneCapacityAccount_ReserveDebitsBothLedgers(t *testing.T) {
	t.Parallel()

	typeA := uuid.New()
	subnetID := uuid.New()
	placement := ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetID}}
	placementKey := forgeNetworkPlacementKey(placement)

	cluster := schedulerNicoCluster(db.Cluster{ID: uuid.New()})
	scheduler := &NodeScheduler{logger: slog.Default()}
	acct := newControlPlaneCapacityAccount(scheduler, cluster, nil, slog.Default())

	// Pre-seed both ledgers so reserve's remaining(...) call short-circuits
	// without a Forge lookup (the scheduler has no Forge provider).
	acct.forgeRemaining[typeA] = 3
	acct.networkRemaining[placementKey] = 5

	acct.reserve(context.Background(), cluster, typeA, placement)

	assert.Equal(t, 2, acct.forgeRemaining[typeA], "sticky reserve must debit the Forge compute ledger")
	assert.Equal(t, 4, acct.networkRemaining[placementKey], "sticky reserve must debit the network ledger")
}

func nicoControlPlaneConfigSpec(instanceTypePreference []uuid.UUID) db.ControlPlaneConfigSpec {
	return db.ControlPlaneConfigSpec{
		Nico: &db.NicoControlPlaneProvider{
			InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
		},
		InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
	}
}
