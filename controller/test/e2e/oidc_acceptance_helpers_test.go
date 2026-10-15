// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

func TestOIDCCleanupKeepsOnlyUndeletedClusters(t *testing.T) {
	t.Parallel()
	first := &api.Cluster{ID: uuid.New(), Name: "tenant-a"}
	second := &api.Cluster{ID: uuid.New(), Name: "tenant-b"}
	waitErr := errors.New("cluster still exists")
	lifecycle := &oidcCleanupLifecycle{
		deleteErrors: map[uuid.UUID]error{first.ID: errors.New("delete response lost")},
		waitErrors:   map[uuid.UUID]error{second.ID: waitErr},
	}
	clusters := []*api.Cluster{first, second}

	err := cleanupOIDCAcceptance(t.Context(), tenantTest{lifecycle: lifecycle}, &clusters, false)
	require.ErrorIs(t, err, waitErr)
	require.Equal(t, []uuid.UUID{first.ID, second.ID}, lifecycle.deleted)
	require.Equal(t, lifecycle.deleted, lifecycle.waited)
	require.Equal(t, []*api.Cluster{second}, clusters)

	delete(lifecycle.waitErrors, second.ID)
	require.NoError(t, cleanupOIDCAcceptance(t.Context(), tenantTest{lifecycle: lifecycle}, &clusters, false))
	require.Equal(t, []uuid.UUID{first.ID, second.ID, second.ID}, lifecycle.deleted)
	require.Empty(t, clusters)
	require.NoError(t, cleanupOIDCAcceptance(t.Context(), tenantTest{lifecycle: lifecycle}, &clusters, false))
	require.Len(t, lifecycle.deleted, 3)
	require.Equal(t, lifecycle.deleted, lifecycle.waited)
}

func TestOIDCCleanupRespectsLifecycleConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		skipDelete        bool
		preserveOnFailure bool
		failed            bool
		preserved         bool
	}{
		{name: "successful run deletes"},
		{name: "failed run deletes by default", failed: true},
		{name: "skip delete preserves successful run", skipDelete: true, preserved: true},
		{name: "skip delete preserves failed run", skipDelete: true, failed: true, preserved: true},
		{name: "preserve on failure deletes successful run", preserveOnFailure: true},
		{name: "preserve on failure keeps failed run", preserveOnFailure: true, failed: true, preserved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := &api.Cluster{ID: uuid.New(), Name: "tenant-a"}
			clusters := []*api.Cluster{cluster}
			lifecycle := &oidcCleanupLifecycle{}
			test := tenantTest{lifecycle: lifecycle}
			test.cfg.Lifecycle.SkipDelete = tc.skipDelete
			test.cfg.Lifecycle.PreserveOnFailure = tc.preserveOnFailure

			require.NoError(t, cleanupOIDCAcceptance(t.Context(), test, &clusters, tc.failed))
			if tc.preserved {
				require.Equal(t, []*api.Cluster{cluster}, clusters)
				require.Empty(t, lifecycle.deleted)
				require.Empty(t, lifecycle.waited)
				return
			}
			require.Empty(t, clusters)
			require.Equal(t, []uuid.UUID{cluster.ID}, lifecycle.deleted)
			require.Equal(t, []uuid.UUID{cluster.ID}, lifecycle.waited)
		})
	}
}

func cleanupOIDCAcceptance(ctx context.Context, test tenantTest, clusters *[]*api.Cluster, failed bool) error {
	if test.cfg.Lifecycle.SkipDelete || (test.cfg.Lifecycle.PreserveOnFailure && failed) {
		return nil
	}
	var failures []error
	// Deleting each owned cluster also deletes its probe namespaces and RBAC.
	remainingClusters := make([]*api.Cluster, 0, len(*clusters))
	for _, cluster := range *clusters {
		deleteErr := test.lifecycle.DeleteCluster(ctx, cluster.ID)
		waitErr := test.lifecycle.WaitForClusterDeleted(ctx, cluster.ID)
		if waitErr != nil {
			if deleteErr != nil {
				failures = append(failures, fmt.Errorf("delete OIDC cluster %s: %w", cluster.Name, deleteErr))
			}
			failures = append(failures, fmt.Errorf("wait for OIDC cluster %s deletion: %w", cluster.Name, waitErr))
			remainingClusters = append(remainingClusters, cluster)
		}
	}
	*clusters = remainingClusters
	return errors.Join(failures...)
}

type oidcCleanupLifecycle struct {
	clusterLifecycle
	deleteErrors map[uuid.UUID]error
	waitErrors   map[uuid.UUID]error
	deleted      []uuid.UUID
	waited       []uuid.UUID
}

func (l *oidcCleanupLifecycle) DeleteCluster(_ context.Context, id uuid.UUID) error {
	l.deleted = append(l.deleted, id)
	return l.deleteErrors[id]
}

func (l *oidcCleanupLifecycle) WaitForClusterDeleted(_ context.Context, id uuid.UUID) error {
	l.waited = append(l.waited, id)
	return l.waitErrors[id]
}
