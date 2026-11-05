// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// reloadFailures returns nvke_authz_bundle_reload_failures_total.
func reloadFailures(t *testing.T) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "nvke_authz_bundle_reload_failures_total" {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("nvke_authz_bundle_reload_failures_total is not registered")
	return 0
}

// A reload that fails counts nvke_authz_bundle_reload_failures_total. One that
// succeeds does not.
func TestReloadCountsFailures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, content := range map[string]string{
		"policies.cedar": `permit(principal in NVKE::Role::"admin", action, resource);`,
		"roles.json":     `[{"uid": {"type": "NVKE::Role", "id": "admin"}}]`,
		"authorization.yaml": "apiVersion: auth.nvke.nvidia.com/v1alpha1\nkind: AuthorizationConfiguration\n" +
			"issuers:\n  idp:\n    claims:\n      - {name: role, value: admin, grants: [admin]}\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	lb, err := NewLocalBundle(dir, []string{"idp"})
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)

	before := reloadFailures(t)
	reload(lb, logger)
	require.InDelta(t, before, reloadFailures(t), 0)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "roles.json"), []byte("not json"), 0o600))
	reload(lb, logger)
	require.InDelta(t, before+1, reloadFailures(t), 0)
}
