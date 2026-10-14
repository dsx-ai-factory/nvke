// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// The CAPI objects live in the management namespace. Unlike labels, an object
// left in the old namespace is not read: a site that renames it moves them.
func TestReadFailureDomainsUsesConfiguredManagementNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	cluster := db.Cluster{ID: uuid.New(), Name: "test-cluster"}

	newCluster := func(namespace string) *clusterv1.Cluster {
		return &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: namespace},
			Status: clusterv1.ClusterStatus{
				Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: new(true)},
				FailureDomains: []clusterv1.FailureDomain{{Name: "fd-a", ControlPlane: new(true)}},
			},
		}
	}

	t.Run("finds the cluster in the configured namespace", func(t *testing.T) {
		t.Parallel()

		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newCluster("nvke-system")).Build()
		r := &reconciler{k8sClient: k8sClient, keys: keys.New("", "nvke-system")}

		domains, err := r.readFailureDomains(context.Background(), cluster)

		require.NoError(t, err)
		assert.Equal(t, []string{"fd-a"}, domains.eligible)
	})

	t.Run("does not look in the legacy namespace", func(t *testing.T) {
		t.Parallel()

		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newCluster(keys.LegacyManagementNamespace)).Build()
		r := &reconciler{k8sClient: k8sClient, keys: keys.New("", "nvke-system")}

		_, err := r.readFailureDomains(context.Background(), cluster)

		require.ErrorContains(t, err, "get CAPI cluster for failure-domain discovery")
	})
}
