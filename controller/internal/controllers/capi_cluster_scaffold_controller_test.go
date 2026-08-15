// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers_test

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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/controllers"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

const (
	testClusterName = "test-cluster"
	testKubeconfig  = "kubeconfig-data"
)

func TestCAPIClusterScaffoldController_Reconcile(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	firstNodeID := uuid.New()
	secondNodeID := uuid.New()

	activeCluster := scaffoldCluster(t, clusterID, siteID, testClusterName)
	deletingCluster := scaffoldCluster(t, uuid.New(), siteID, "deleting-cluster")
	deletingCluster.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	emptyCluster := db.Cluster{
		ID:   uuid.New(),
		Name: "empty-cluster",
	}
	emptyCluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: json.RawMessage(`{}`)})
	firstNode := joinedCPNode(firstNodeID, clusterID)
	secondNode := joinedCPNode(secondNodeID, clusterID)

	type parameters struct {
		clusters               []db.Cluster
		nodes                  []db.Node
		kubeconfigs            map[uuid.UUID]string
		kubeconfigErrors       map[uuid.UUID]error
		expectListNodes        bool
		wantErrorContains      string
		wantKubeconfigSecret   bool
		wantNodeAgentCallOrder []uuid.UUID
	}

	tests := map[string]parameters{
		"applies scaffold and kubeconfig secret": {
			clusters:        []db.Cluster{activeCluster},
			nodes:           []db.Node{firstNode},
			kubeconfigs:     map[uuid.UUID]string{firstNodeID: testKubeconfig},
			expectListNodes: true,

			wantKubeconfigSecret:   true,
			wantNodeAgentCallOrder: []uuid.UUID{firstNodeID},
		},
		"skips deleting and empty clusters": {
			clusters: []db.Cluster{deletingCluster, emptyCluster},
		},
		"tries control plane nodes until kubeconfig succeeds": {
			clusters:         []db.Cluster{activeCluster},
			nodes:            []db.Node{firstNode, secondNode},
			kubeconfigs:      map[uuid.UUID]string{secondNodeID: testKubeconfig},
			kubeconfigErrors: map[uuid.UUID]error{firstNodeID: errors.New("temporary agent failure")},
			expectListNodes:  true,

			wantKubeconfigSecret:   true,
			wantNodeAgentCallOrder: []uuid.UUID{firstNodeID, secondNodeID},
		},
		"returns error when kubeconfig extraction fails": {
			clusters:          []db.Cluster{activeCluster},
			nodes:             []db.Node{firstNode},
			kubeconfigErrors:  map[uuid.UUID]error{firstNodeID: errors.New("agent unavailable")},
			expectListNodes:   true,
			wantErrorContains: "failed to extract kubeconfig",

			wantNodeAgentCallOrder: []uuid.UUID{firstNodeID},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			mockDB.On("ListClusters", mock.Anything, mock.MatchedBy(func(filters db.ClusterFilters) bool {
				return filters.SiteID != nil && *filters.SiteID == siteID
			})).Return(params.clusters, nil)
			if params.expectListNodes {
				mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
					return filters.ClusterID != nil && *filters.ClusterID == clusterID && filters.ControlPlane != nil && *filters.ControlPlane
				})).Return(params.nodes, nil)
			}

			k8sClient := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).Build()
			nodeAgentClient := &agenttest.FakeClient{
				Kubeconfigs:      params.kubeconfigs,
				KubeconfigErrors: params.kubeconfigErrors,
			}
			controller := controllers.NewCAPIClusterScaffoldController(
				slog.Default(),
				mockDB,
				k8sClient,
				nodeAgentClient,
			)

			err := controller.Reconcile(ctx, db.Site{ID: siteID})
			if params.wantErrorContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), params.wantErrorContains)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, params.wantNodeAgentCallOrder, nodeAgentClient.GetKubeconfigCalls)
			assertKubeconfigSecret(t, ctx, k8sClient, activeCluster, params.wantKubeconfigSecret)
			if !params.expectListNodes {
				mockDB.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
			}
			mockDB.AssertExpectations(t)
		})
	}
}

func scaffoldCluster(t *testing.T, clusterID, siteID uuid.UUID, name string) db.Cluster {
	t.Helper()

	cluster := db.Cluster{
		ID:   clusterID,
		Name: name,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: uuid.New()})
	return cluster
}

func joinedCPNode(nodeID, clusterID uuid.UUID) db.Node {
	return db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         "cp-node",
		IP:           "10.0.0.1",
		Conditions: db.NodeConditions{
			Joined:          db.Condition{Status: db.ConditionStatusTrue},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:        db.Condition{Status: db.ConditionStatusFalse},
		},
	}
}

func assertKubeconfigSecret(
	t *testing.T,
	ctx context.Context,
	k8sClient ctrlclient.Client,
	cluster db.Cluster,
	wantSecret bool,
) {
	t.Helper()

	namespace := capi.ClusterNamespace(cluster.Name)
	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cluster.Name + "-kubeconfig", Namespace: namespace}, secret)
	if !wantSecret {
		assert.True(t, apierrors.IsNotFound(err))
		return
	}

	require.NoError(t, err)
	assert.Equal(t, []byte(testKubeconfig), secret.Data["value"])
}

func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	return scheme
}
