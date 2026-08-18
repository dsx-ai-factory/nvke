// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func hydrateTenants(ce *ForgeCacheEngine, ids ...string) {
	tenants := make([]*forgesdk.Tenant, 0, len(ids))
	for _, id := range ids {
		tenants = append(tenants, &forgesdk.Tenant{Id: forgesdk.PtrString(id)})
	}

	ce.GetTenantsCache().Hydrate(tenants)
}

func TestRecordTenantSyncAuth(t *testing.T) {
	t.Run("does not warn while outcomes are incomplete", func(t *testing.T) {
		ce := NewForgeCacheEngine(config.Config{}, nil, slog.New(slog.DiscardHandler))
		hydrateTenants(ce, "a", "b")

		ce.RecordTenantSyncAuth("a", false)

		require.False(t, ce.tenantSyncAuthWarned, "should not warn before every tenant has reported")
	})

	t.Run("warns once when no tenant is accessible", func(t *testing.T) {
		ce := NewForgeCacheEngine(config.Config{}, nil, slog.New(slog.DiscardHandler))
		hydrateTenants(ce, "a", "b")

		ce.RecordTenantSyncAuth("a", false)
		ce.RecordTenantSyncAuth("b", false)

		require.True(t, ce.tenantSyncAuthWarned, "should warn once all tenants are unauthorized")
	})

	t.Run("does not warn when at least one tenant is accessible", func(t *testing.T) {
		ce := NewForgeCacheEngine(config.Config{}, nil, slog.New(slog.DiscardHandler))
		hydrateTenants(ce, "a", "b")

		ce.RecordTenantSyncAuth("a", true)
		ce.RecordTenantSyncAuth("b", false)

		require.False(t, ce.tenantSyncAuthWarned)
	})

	t.Run("prunes stale entries so a removed tenant's auth does not suppress the warning", func(t *testing.T) {
		ce := NewForgeCacheEngine(config.Config{}, nil, slog.New(slog.DiscardHandler))
		hydrateTenants(ce, "a", "b")

		// "a" is accessible, "b" is not — no warning while both are present.
		ce.RecordTenantSyncAuth("a", true)
		ce.RecordTenantSyncAuth("b", false)
		require.False(t, ce.tenantSyncAuthWarned)

		// Tenant "a" is removed on rehydration; its stale `true` must not keep the
		// warning suppressed for the remaining (all-unauthorized) tenant set.
		hydrateTenants(ce, "b")
		ce.RecordTenantSyncAuth("b", false)

		require.True(t, ce.tenantSyncAuthWarned, "stale auth for a removed tenant should not suppress the warning")
		require.NotContains(t, ce.tenantSyncAuth, "a", "stale entry should be pruned")
	})

	t.Run("resets the warning once a tenant becomes accessible again", func(t *testing.T) {
		ce := NewForgeCacheEngine(config.Config{}, nil, slog.New(slog.DiscardHandler))
		hydrateTenants(ce, "a", "b")

		ce.RecordTenantSyncAuth("a", false)
		ce.RecordTenantSyncAuth("b", false)
		require.True(t, ce.tenantSyncAuthWarned)

		ce.RecordTenantSyncAuth("a", true)
		require.False(t, ce.tenantSyncAuthWarned, "warning should reset so it can fire again later")
	})
}
