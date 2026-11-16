// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
)

// The bodies are the ones auth/authn and auth/authz write.
func TestControllerRefusalsSayWhatToDo(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantList   string
		wantDelete string
	}{
		{
			name:   "authentication rejects the token",
			status: http.StatusUnauthorized,
			body:   `{"message":"Authentication failed: expired."}` + "\n",
			wantList: "listing clusters: 401 Unauthorized: Authentication failed: expired: " +
				"the NVKE API did not accept nvkectl's credential; check the management cluster kubeconfig " +
				"('nvkectl config set-site --kubeconfig <path>') and log in again if it uses OIDC " +
				"(see docs/operate/api-authentication.md)",
			wantDelete: "deleting cluster: 401 Unauthorized: Authentication failed: expired: " +
				"the NVKE API did not accept nvkectl's credential; check the management cluster kubeconfig " +
				"('nvkectl config set-site --kubeconfig <path>') and log in again if it uses OIDC " +
				"(see docs/operate/api-authentication.md)",
		},
		{
			name:   "authentication does not admit the identity",
			status: http.StatusForbidden,
			body:   `{"message":"Not admitted: the authenticated identity matches no admission rule."}` + "\n",
			wantList: "listing clusters: 403 Forbidden: Not admitted: the authenticated identity matches no admission rule: " +
				"the NVKE API authenticated this identity but admits no such caller; " +
				"ask an NVKE administrator to admit it (see docs/operate/api-authentication.md)",
			wantDelete: "deleting cluster: 403 Forbidden: Not admitted: the authenticated identity matches no admission rule: " +
				"the NVKE API authenticated this identity but admits no such caller; " +
				"ask an NVKE administrator to admit it (see docs/operate/api-authentication.md)",
		},
		{
			name:   "authorization refuses the caller's roles",
			status: http.StatusForbidden,
			body:   `{"message":"Not authorized"}` + "\n",
			wantList: "listing clusters: 403 Forbidden: Not authorized: " +
				"no role granted to this identity allows this operation; " +
				"ask an NVKE administrator to grant one (see docs/operate/api-authorization.md). " +
				"If nvkectl sent no credential, check the management cluster kubeconfig " +
				"('nvkectl config set-site --kubeconfig <path>')",
			wantDelete: "deleting cluster: 403 Forbidden: Not authorized: " +
				"no role granted to this identity allows this operation; " +
				"ask an NVKE administrator to grant one (see docs/operate/api-authorization.md). " +
				"If nvkectl sent no credential, check the management cluster kubeconfig " +
				"('nvkectl config set-site --kubeconfig <path>')",
		},
		{
			name:       "a 403 from something in front of the API gets no hint",
			status:     http.StatusForbidden,
			body:       "Forbidden by proxy\n",
			wantList:   "listing clusters: 403 Forbidden: Forbidden by proxy",
			wantDelete: "deleting cluster: 403 Forbidden: Forbidden by proxy",
		},
		{
			name:       "any other refusal is reported as before",
			status:     http.StatusBadRequest,
			body:       `{"message":"name is required"}` + "\n",
			wantList:   "listing clusters: 400 Bad Request: name is required",
			wantDelete: "deleting cluster: 400 Bad Request: name is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			client, err := newClient(config.SiteConfig{URL: srv.URL}, nil)
			require.NoError(t, err)

			_, err = client.ListClusters(context.Background(), nil)
			require.Error(t, err)
			assert.Equal(t, tt.wantList, err.Error())
			var status *StatusError
			require.True(t, errors.As(err, &status), "ListClusters error is not a *StatusError")
			assert.Equal(t, tt.status, status.StatusCode)

			err = client.DeleteCluster(context.Background(), openapi_types.UUID{})
			require.Error(t, err)
			assert.Equal(t, tt.wantDelete, err.Error())
		})
	}
}
