// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/controller/api"
)

// The generated table and the router must agree in both directions. A route the
// router serves and the table does not know is a route no policy can name, and
// the middleware denies it — which is safe, and is also an outage nobody
// predicted. This test is what makes regenerating actions.gen.go non-optional
// when api/openapi.yaml changes.

// specOperations reads the operations out of the embedded OpenAPI document,
// keyed by the route pattern the generated router registers for each.
func specOperations(t *testing.T) map[string]string {
	t.Helper()

	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(api.OpenapiYAML), &doc))

	out := map[string]string{}
	for path, item := range doc.Paths {
		for _, method := range []string{"get", "put", "post", "patch", "delete"} {
			node, ok := item[method]
			if !ok {
				continue
			}
			var op struct {
				OperationID string `yaml:"operationId"`
			}
			require.NoError(t, node.Decode(&op))
			require.NotEmptyf(t, op.OperationID, "%s %s has no operationId", method, path)
			out[strings.ToUpper(method)+" /api"+path] = op.OperationID
		}
	}
	return out
}

func TestEveryOperationInTheSpecIsInTheTable(t *testing.T) {
	spec := specOperations(t)
	require.NotEmpty(t, spec, "the embedded OpenAPI document declares no operations")

	for pattern, operationID := range spec {
		op, ok := operations[pattern]
		require.Truef(t, ok,
			"%s is in api/openapi.yaml but not in actions.gen.go; run 'go run ./hack/gen-authz-actions'", pattern)
		assert.Equalf(t, operationID, op.Action, "%s maps to the wrong action", pattern)
	}
}

func TestEveryTableEntryIsInTheSpec(t *testing.T) {
	spec := specOperations(t)

	for pattern := range operations {
		_, ok := spec[pattern]
		assert.Truef(t, ok,
			"%s is in actions.gen.go but not in api/openapi.yaml; run 'go run ./hack/gen-authz-actions'", pattern)
	}
}

// TestEveryTableEntryRoutes closes the loop the spec cannot: the table is
// derived from openapi.yaml, and so is the router, but only this asserts that a
// pattern in the table is a pattern the router actually answers on. A mismatch
// in how the base URL or a parameter is spelled shows up here and nowhere else.
func TestEveryTableEntryRoutes(t *testing.T) {
	// A nil ServerInterface is safe here: the test matches routes with
	// mux.Handler and never dispatches one, so no method on it is ever called.
	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: "/api"})

	for pattern, op := range operations {
		method, path, ok := strings.Cut(pattern, " ")
		require.Truef(t, ok, "%q is not a %q-separated pattern", pattern, " ")

		// Substitute a concrete value for the wildcard so the request is one the
		// mux can match.
		concrete := path
		if op.IDParam != "" {
			concrete = strings.ReplaceAll(path, "{"+op.IDParam+"}", "11111111-1111-1111-1111-111111111111")
		}

		req := httptest.NewRequestWithContext(t.Context(), method, concrete, http.NoBody)
		_, matched := mux.Handler(req)
		assert.Equalf(t, pattern, matched, "%s does not route to its own pattern", pattern)
	}
}

// TestResourceTypesAreDerivedFromTheParameter pins the rule the generator uses,
// because it is the one place a wrong answer would be silent: an operation
// authorized against the wrong entity type still gets a decision.
func TestResourceTypesAreDerivedFromTheParameter(t *testing.T) {
	for pattern, op := range operations {
		if op.IDParam == "" {
			assert.Emptyf(t, op.ResourceType, "%s is a collection operation and should carry no resource type", pattern)
			assert.NotEmptyf(t, op.Collection, "%s must name a collection", pattern)
			continue
		}
		assert.Truef(t, strings.HasPrefix(op.ResourceType, "NKE::"),
			"%s resource type %q is not in the NKE namespace", pattern, op.ResourceType)
		assert.Truef(t, strings.HasSuffix(op.IDParam, "ID"),
			"%s path parameter %q does not end in ID", pattern, op.IDParam)
	}
}
