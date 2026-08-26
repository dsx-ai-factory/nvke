// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An Allow that arrives beside an evaluation error is not an Allow.
//
// Cedar reports a decision and any evaluation errors together. A forbid whose
// guard errors is skipped, and the permit it existed to override then wins — so
// the engine answers Allow, with the error alongside, and a request that policy
// meant to block is served.
//
// The in-process engine has always refused that. The sidecar client decoded
// diagnostics.errors and ignored it, which made apiAuthz.engine an observable
// behavior switch: the same bundle and the same request, served on one
// deployment and refused on the other. AZ-4 says nothing outside an engine knows
// which engine is running.
//
// This is a table because the asymmetry matters: Deny-with-errors stays a Deny.
// Only an Allow reached while something errored is untrustworthy.
func TestAnAllowBesideAnEvaluationErrorIsRefused(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantErr   bool
		wantAllow bool
	}{
		{
			name:      "Allow with an evaluation error",
			body:      `{"decision":"Allow","diagnostics":{"reason":["p"],"errors":["entity does not exist"]}}`,
			wantErr:   true,
			wantAllow: false,
		},
		{
			name:      "Deny with an evaluation error stays a Deny",
			body:      `{"decision":"Deny","diagnostics":{"reason":[],"errors":["entity does not exist"]}}`,
			wantErr:   false,
			wantAllow: false,
		},
		{
			name:      "Allow with no errors",
			body:      `{"decision":"Allow","diagnostics":{"reason":["p"],"errors":[]}}`,
			wantErr:   false,
			wantAllow: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			agent, err := NewCedarAgent(server.URL, 0)
			require.NoError(t, err)

			decision, err := agent.Authorize(context.Background(), validQuery())
			if tt.wantErr {
				require.Error(t, err, "an Allow reached while a policy errored must not be served")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantAllow, decision.Allow)
		})
	}
}

// The sidecar half of the classification. Local”'s half is
// TestLocalRefusesAnAllowReachedWhileAPolicyErrored — the two must agree,
// because the metric reason and the log line are what an operator reads during a
// permissive rollout, and they must not depend on which engine is deployed.
func TestCedarAgentTreatsAnErroringAllowAsAnEngineFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"Allow","diagnostics":{"reason":["p"],"errors":["boom"]}}`))
	}))
	defer server.Close()

	agent, err := NewCedarAgent(server.URL, 0)
	require.NoError(t, err)

	_, agentErr := agent.Authorize(context.Background(), validQuery())
	require.Error(t, agentErr)

	// Not ErrInvalidQuery: the question was fine, the engine could not answer it.
	// That distinction drives engine_error versus invalid_query in the metric.
	assert.NotErrorIs(t, agentErr, ErrInvalidQuery,
		"an erroring evaluation is the engine's failure, not a malformed question")
}

// Policy names in a decision come from the @id annotation on both engines. They
// are what appears in the log when a request is allowed, and a positional
// "policy0" changes meaning the moment a policy is inserted above it.
func TestBothEnginesReportAnnotatedPolicyNames(t *testing.T) {
	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	decision, err := local.Authorize(context.Background(), Query{
		Principal: subjectReader, Issuer: "kube-apiserver", Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
	})
	require.NoError(t, err)
	require.True(t, decision.Allow)
	require.NotEmpty(t, decision.Reasons)

	assert.Contains(t, decision.Reasons, "readers-may-read",
		"the in-process engine must report the @id the source carries, as the sidecar does")
	for _, reason := range decision.Reasons {
		assert.NotRegexp(t, `^policy\d+$`, reason,
			"a positional policy id is not a name a reader can find in the source")
	}
}
