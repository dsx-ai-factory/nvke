// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// A denial has to be actionable by the operator who reads it.
//
// The response body says nothing about policy on purpose, so the log is the only
// place a refusal is explained — and behind a Service the remote address is the
// proxy's, so without the subject there is nothing in either place that
// identifies who was refused.
//
// The two ordinary outcomes also have to be distinguishable. The shipped bundle
// carries a forbid, so "a rule refused this" and "nothing permitted it" are
// both routine, they call for different fixes, and they used to produce the same
// line.

// denyLine drives the real middleware over the shipped bundle and returns the
// refusal it logged.
func denyLine(t *testing.T, subject, method, path string) map[string]any {
	t.Helper()

	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})
	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	var log bytes.Buffer
	middleware, err := NewMiddleware(local, resolver, ModeEnforcing,
		slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})), nil)
	require.NoError(t, err)

	handler := middleware.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran for a request that should have been refused")
	}))

	r := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
	r = r.WithContext(authn.ContextWithPrincipal(r.Context(), &authn.Principal{
		IssuerName: "site-apiserver",
		Issuer:     "https://kubernetes.default.svc.cluster.local",
		Subject:    subject,
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	require.Equal(t, http.StatusForbidden, rec.Code)

	var line map[string]any
	require.NoError(t, json.Unmarshal(log.Bytes(), &line), "expected exactly one log line: %s", log.String())
	return line
}

func TestARefusalNamesWhoWasRefused(t *testing.T) {
	line := denyLine(t, subjectOperator, http.MethodDelete, api.BasePath+"/nodes/"+someNodeID)

	assert.Equal(t, subjectOperator, line["subject"], "a refusal with no subject is not attributable to anyone")
	assert.Equal(t, "site-apiserver", line["issuer"],
		"the subject is only unique within its issuer, so the issuer belongs beside it")
	assert.Equal(t, "deleteNode", line["action"])
	assert.Equal(t, "NKE::Node/"+someNodeID, line["resource"])
}

func TestARefusalNamesTheRuleThatRefused(t *testing.T) {
	forbidden := denyLine(t, subjectOperator, http.MethodDelete, api.BasePath+"/nodes/"+someNodeID)
	assert.Equal(t, "only-node-operators-change-node-lifecycle", forbidden["policies"],
		"a forbid fired, and which one is the whole of what the operator needs to know")

	unmatched := denyLine(t, subjectStranger, http.MethodGet, api.BasePath+"/clusters")
	assert.Empty(t, unmatched["policies"],
		"nothing matched, so naming a policy would be inventing one")

	assert.NotEqual(t, forbidden["policies"], unmatched["policies"],
		"a rule refusing and nothing permitting call for different fixes and must not read the same")
}

// A request refused before it names an operation is still attributable. Probing
// for endpoints that do not exist is the case an operator most wants a subject
// for, and it is the one that resolves to no query at all.
func TestARefusalBeforeAnOperationStillNamesTheCaller(t *testing.T) {
	line := denyLine(t, subjectOperator, http.MethodGet, api.BasePath+"/no-such-endpoint")

	assert.Equal(t, string(ReasonNoRoute), line["reason"])
	assert.Equal(t, subjectOperator, line["subject"])
	assert.Equal(t, "site-apiserver", line["issuer"])
	assert.Empty(t, line["action"], "no route matched, so there is no operation to name")
	assert.Empty(t, line["resource"])
}
