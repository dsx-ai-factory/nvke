// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestVerifyControlPlaneRoleLabel(t *testing.T) {
	tests := []struct {
		name     string
		kubeNode *corev1.Node
		wantErr  string
	}{
		{
			name:     "role label present",
			kubeNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}},
		},
		{
			name:     "role label missing",
			kubeNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Labels: map[string]string{"example.com/team": "ml"}}},
			wantErr:  "control-plane node/cp-0 is missing expected label node-role.kubernetes.io/control-plane",
		},
		{
			name:     "no labels",
			kubeNode: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-0"}},
			wantErr:  "control-plane node/cp-0 is missing expected label node-role.kubernetes.io/control-plane",
		},
		{
			name:    "node not found in tenant cluster",
			wantErr: `failed to get Kubernetes node cp-0: nodes "cp-0" not found`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", ControlPlane: true}, nil)
			database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil)

			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if test.kubeNode != nil {
				builder = builder.WithObjects(test.kubeNode)
			}
			a := &Activities{db: database, tenantClientProvider: &fakeTenantClientProvider{tenantClient: builder.Build()}}

			err := a.VerifyControlPlaneRoleLabel(context.Background(), nodeID)

			if test.wantErr != "" {
				require.EqualError(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
			database.AssertExpectations(t)
		})
	}
}
