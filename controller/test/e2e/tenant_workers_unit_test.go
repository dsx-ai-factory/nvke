// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"flag"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

// capacityRecorder keeps the counts requireRunCapacity asks the provider for.
type capacityRecorder struct {
	infra.Provider
	cpu, workers int
}

func (c *capacityRecorder) RequireCapacity(_ context.Context, _ map[string]string, cpu, workers int) error {
	c.cpu, c.workers = cpu, workers
	return nil
}

func TestTenantWorkers(t *testing.T) {
	t.Log("Testing: -e2e.tenant-workers defaults to the one worker the lanes always created")
	require.Equal(t, "1", flag.Lookup("e2e.tenant-workers").DefValue)

	for _, workers := range []int{1, 3} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			t.Log("Testing: the tenant spec carries one worker pool of that many nodes")
			pools, err := tenantWorkerPools(workers)
			require.NoError(t, err)
			require.Len(t, pools, 1)
			require.Equal(t, workers, pools[0].DesiredNodeCount)

			t.Log("Testing: the capacity check asks for the same workers beside the two CPU machines")
			var checker capacityRecorder
			require.NoError(t, requireRunCapacity(t.Context(), &checker, siteStageFull, workers))
			require.Equal(t, 2, checker.cpu)
			require.Equal(t, workers, checker.workers)
		})
	}

	for _, workers := range []int{0, -1} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			t.Log("Testing: a tenant of fewer than one worker is refused before the provider is asked")
			_, err := tenantWorkerPools(workers)
			require.ErrorContains(t, err, "-e2e.tenant-workers")
			var checker capacityRecorder
			require.ErrorContains(t, requireRunCapacity(t.Context(), &checker, siteStageFull, workers), "-e2e.tenant-workers")
			require.Zero(t, checker.cpu, "the provider should not have been asked for capacity")

			t.Log("Testing: a deployment run builds no tenant, so the worker count does not stop it")
			require.NoError(t, requireRunCapacity(t.Context(), &checker, siteStageDeployment, workers))
		})
	}
}
