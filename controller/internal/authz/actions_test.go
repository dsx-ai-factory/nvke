// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// The generated table and the spec must agree in both directions. A route the
// router serves and the table does not know is a route no policy can name, and
// the middleware denies it — which is safe, and is also an outage nobody
// predicted. This test is what makes regenerating actions.gen.go non-optional
// when api/openapi.yaml changes.
//
// Both sides here come from the same document, so this cannot see a route the
// document does not describe. router_test.go covers that direction, by reading
// what api/server.gen.go registered.

// specOperations reads the operations out of the embedded OpenAPI document,
// keyed by the route pattern the generated router registers for each.
//
// It reads them with kin-openapi, and enumerates PathItem.Operations() rather
// than a list of methods written here: a list written here is a list that can
// omit one, and an operation this test cannot see is one it cannot require.
func specOperations(t *testing.T) map[string]string {
	t.Helper()

	doc, err := openapi3.NewLoader().LoadFromData([]byte(api.OpenapiYAML))
	require.NoError(t, err)
	require.NotNil(t, doc.Paths, "the embedded OpenAPI document declares no paths")

	out := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			require.NotEmptyf(t, op.OperationID, "%s %s has no operationId", method, path)
			out[method+" "+api.BasePath+path] = op.OperationID
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
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})

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
