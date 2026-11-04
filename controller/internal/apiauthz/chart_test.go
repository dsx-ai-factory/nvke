// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz_test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/apiauthz"
)

// chartValues is the controller chart's values file.
const chartValues = "../../deploy/charts/controller/values.yaml"

// chartAuthorization grants role=admin and role=viewer callers from idp the
// chart's admin and viewer roles, in the tenant their tenant claim names.
const chartAuthorization = `apiVersion: auth.nvke.nvidia.com/v1alpha1
kind: AuthorizationConfiguration
issuers:
  idp:
    tenant: {claim: tenant}
    claims:
      - {name: role, value: admin, grants: [admin]}
      - {name: role, value: viewer, grants: [viewer]}
`

// The chart's default policies and roles load for the API's actions. In its
// own tenant, admin may call every operation and viewer every GET.
func TestChartDefaultBundle(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(chartValues)
	require.NoError(t, err)
	var values struct {
		APIAuthz struct {
			Policies string `json:"policies"`
			Roles    string `json:"roles"`
		} `json:"apiAuthz"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &values))
	dir := t.TempDir()
	for name, content := range map[string]string{
		"policies.cedar":     values.APIAuthz.Policies,
		"roles.json":         values.APIAuthz.Roles,
		"authorization.yaml": chartAuthorization,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	lb, err := apiauthz.NewLocalBundle(dir, []string{"idp"})
	require.NoError(t, err, "the chart's default bundle does not load")
	logger := slog.New(slog.DiscardHandler)
	client, err := authz.NewClient(lb, authn.ModeEnforcing, logger, nil)
	require.NoError(t, err)
	m := apiauthz.NewMiddleware(client, &stubTenants{}, nil, logger)

	spec, err := openapi3.NewLoader().LoadFromData([]byte(api.OpenapiYAML))
	require.NoError(t, err)
	for _, op := range operations(t) {
		viewer := http.StatusForbidden
		if get := spec.Paths.Find(op.path).Get; get != nil && get.OperationID == op.id {
			viewer = http.StatusOK
		}
		request := op.request(objectID(checks[op.id].lookup, ownedByA))
		for role, want := range map[string]int{"admin": http.StatusOK, "viewer": viewer} {
			t.Run(op.id+"/"+role, func(t *testing.T) {
				t.Parallel()

				ctx := authn.ContextWithPrincipal(context.Background(), &authn.Principal{
					IssuerName: "idp",
					Subject:    role + "-in-" + tenantA,
					Claims:     map[string]any{"role": role, "tenant": tenantA},
				})
				require.Equal(t, want, serve(t, m, ctx, op.method.Name, request))
			})
		}
	}
}
