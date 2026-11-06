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

// writeBundle writes a bundle with an admin role, and authorization.yaml
// granting it from idp after the header and issuer settings in config.
func writeBundle(t *testing.T, dir, config string) {
	t.Helper()

	for name, content := range map[string]string{
		"policies.cedar": `permit(principal in NVKE::Role::"admin", action, resource);`,
		"roles.json":     `[{"uid": {"type": "NVKE::Role", "id": "admin"}}]`,
		"authorization.yaml": "apiVersion: auth.nvke.nvidia.com/v1alpha1\nkind: AuthorizationConfiguration\n" + config +
			"issuers:\n  idp:\n    claims:\n      - {name: role, value: admin, grants: [admin]}\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
}

// A reload that fails counts nvke_authz_bundle_reload_failures_total. One that
// succeeds does not.
func TestReloadCountsFailures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, "")
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

// The API has no tenants, so the load and a reload both refuse
// authorization.yaml's tenancy keys.
func TestRefusesTenancyKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundle(t, dir, "operatorTenant: nca-0000000000000000\n")
	_, err := NewLocalBundle(dir, []string{"idp"})
	require.ErrorContains(t, err, "authorization.yaml: operatorTenant is set, but this API authorizes by role only")

	writeBundle(t, dir, "")
	lb, err := NewLocalBundle(dir, []string{"idp"})
	require.NoError(t, err)
	tenancy := "apiVersion: auth.nvke.nvidia.com/v1alpha1\nkind: AuthorizationConfiguration\n" +
		"issuers:\n  idp:\n    tenant: {claim: org}\n    operators: true\n" +
		"    claims:\n      - {name: role, value: admin, grants: [admin]}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "authorization.yaml"), []byte(tenancy), 0o600))
	_, err = lb.Reload()
	require.ErrorContains(t, err, "authorization.yaml: issuers.idp.tenant is set, but this API authorizes by role only")
	require.ErrorContains(t, err, "authorization.yaml: issuers.idp.operators is set, but this API authorizes by role only")
}
