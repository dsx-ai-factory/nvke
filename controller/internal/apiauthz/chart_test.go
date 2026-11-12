// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
// chart's admin and viewer roles.
const chartAuthorization = `apiVersion: auth.nvke.nvidia.com/v1alpha1
kind: AuthorizationConfiguration
issuers:
  idp:
    claims:
      - {name: role, value: admin, grants: [admin]}
      - {name: role, value: viewer, grants: [viewer]}
`

// newClient loads the chart's default policies and roles, with
// chartAuthorization, into a Client in mode.
func newClient(t *testing.T, mode authn.Mode) *authz.Client {
	t.Helper()

	raw, err := os.ReadFile(chartValues)
	require.NoError(t, err)
	var values struct {
		APIAuthz struct {
			Authorizer struct {
				LocalBundle struct {
					Policies string `json:"policies"`
					Roles    string `json:"roles"`
				} `json:"localBundle"`
			} `json:"authorizer"`
		} `json:"apiAuthz"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &values))
	dir := t.TempDir()
	for name, content := range map[string]string{
		"policies.cedar":     values.APIAuthz.Authorizer.LocalBundle.Policies,
		"roles.json":         values.APIAuthz.Authorizer.LocalBundle.Roles,
		"authorization.yaml": chartAuthorization,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	lb, err := apiauthz.NewLocalBundle(dir, []string{"idp"})
	require.NoError(t, err, "the chart's default bundle does not load")
	client, err := authz.NewClient(lb, mode, slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)
	return client
}

// caller returns a context carrying a principal from idp with role, or with
// no role claim if role is empty.
func caller(role string) context.Context {
	claims := map[string]any{}
	if role != "" {
		claims["role"] = role
	}
	return authn.ContextWithPrincipal(context.Background(), &authn.Principal{IssuerName: "idp", Subject: "user", Claims: claims})
}

// In the chart's default bundle, admin may call every operation in the spec,
// viewer every GET, and a caller with no role none.
func TestChartDefaultBundle(t *testing.T) {
	t.Parallel()

	authorize := apiauthz.Middleware(newClient(t, authn.ModeEnforcing), slog.New(slog.DiscardHandler))
	spec, err := openapi3.NewLoader().LoadFromData([]byte(api.OpenapiYAML))
	require.NoError(t, err)
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			// The generated handler names each operation by its Go name.
			goName := strings.ToUpper(op.OperationID[:1]) + op.OperationID[1:]
			viewer := http.StatusForbidden
			if method == http.MethodGet {
				viewer = http.StatusOK
			}
			for role, want := range map[string]int{"admin": http.StatusOK, "viewer": viewer, "": http.StatusForbidden} {
				t.Run(op.OperationID+"/"+role, func(t *testing.T) {
					t.Parallel()

					next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
						return "handled", nil
					}
					w := httptest.NewRecorder()
					r := httptest.NewRequestWithContext(caller(role), method, path, nil)
					response, err := authorize(next, goName)(r.Context(), w, r, nil)
					require.NoError(t, err)
					if want == http.StatusOK {
						require.Equal(t, "handled", response)
						return
					}
					require.Nil(t, response)
					require.Equal(t, want, w.Code)
				})
			}
		}
	}
}
