// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCedarAgentValidatesItsEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{"a loopback endpoint", "http://127.0.0.1:8180", false},
		{"a trailing slash", "http://127.0.0.1:8180/", false},
		{"https", "https://127.0.0.1:8180", false},
		{"empty", "", true},
		{"no scheme", "127.0.0.1:8180", true},
		{"a scheme that is not http", "unix:///var/run/cedar.sock", true},
		{"no host", "http://", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewCedarAgent(tt.endpoint, 0)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// A quote landing unescaped in `NKE::Principal::"<id>"` closes the literal, and
// the rest of the identifier becomes Cedar syntax. Object identifiers come from
// URL path segments the mux has already percent-decoded, so a caller does
// control these bytes. Refusing is checkable by reading it; escaping is not.
func TestEntityUIDRefusesWhatWouldEscapeTheLiteral(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"a uuid", "11111111-1111-1111-1111-111111111111", false},
		{"a spiffe id", "spiffe://nvidia.com/ns/nke-system/sa/cwe", false},
		{"a service account subject", "system:serviceaccount:nke-system:cli", false},
		{"a quote", `abc" || true || "`, true},
		{"a backslash", `abc\"`, true},
		{"a newline", "abc\ndef", true},
		{"a null byte", "abc\x00def", true},
		{"a delete character", "abc\x7f", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := entityUID(PrincipalType, tt.id)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, `NKE::Principal::"`+tt.id+`"`, got)
		})
	}
}

func TestEntityUIDRefusesAnEmptyType(t *testing.T) {
	_, err := entityUID("", "abc")
	require.Error(t, err)
}

// An identifier that cannot be rendered is refused before any request is sent:
// the engine is never asked a question this code could not write down.
func TestAuthorizeRefusesAnUnsafeIdentifierWithoutCallingTheEngine(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	agent, err := NewCedarAgent(server.URL, 0)
	require.NoError(t, err)

	_, err = agent.Authorize(context.Background(), Query{
		Principal: `evil" || true || "`, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: "abc",
	})
	require.ErrorIs(t, err, ErrUnsafeEntityID)
	assert.False(t, called, "the engine must not be asked a question that could not be rendered")
}

// The wire shape the sidecar expects, pinned. It is the one contract this
// package has with a component it does not build.
func TestAuthorizeSendsTheExpectedQuery(t *testing.T) {
	var got authorizationCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, authorizePath, r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		// assert, not require: a failed require in a handler goroutine calls
		// runtime.Goexit and the test hangs rather than failing.
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_, _ = w.Write([]byte(`{"decision":"Allow","diagnostics":{"reason":["readers"],"errors":[]}}`))
	}))
	defer server.Close()

	agent, err := NewCedarAgent(server.URL, 0)
	require.NoError(t, err)

	decision, err := agent.Authorize(context.Background(), Query{
		Principal:    "system:serviceaccount:nke-system:cli",
		Issuer:       "kube-apiserver",
		Action:       "getCluster",
		ResourceType: "NKE::Cluster",
		ResourceID:   "abc",
		Method:       http.MethodGet,
	})
	require.NoError(t, err)

	assert.True(t, decision.Allow)
	assert.Equal(t, []string{"readers"}, decision.Reasons)

	assert.Equal(t, `NKE::Principal::"system:serviceaccount:nke-system:cli"`, got.Principal)
	assert.Equal(t, `NKE::Action::"getCluster"`, got.Action)
	assert.Equal(t, `NKE::Cluster::"abc"`, got.Resource)
	assert.Equal(t, "kube-apiserver", got.Context["issuer"])
	assert.Equal(t, http.MethodGet, got.Context["method"])

	// The resource travels with the query rather than living in the bundle, so
	// the bundle does not have to mirror the database.
	require.Len(t, got.AdditionalEntities, 1)
	assert.Equal(t, "NKE::Cluster", got.AdditionalEntities[0].UID.Type)
	assert.Equal(t, "abc", got.AdditionalEntities[0].UID.ID)
}

// Anything but the literal "Allow" is a denial. A response this code does not
// understand must never become a pass.
func TestOnlyAllowAllows(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  bool
		wantPass bool
	}{
		{"Allow", `{"decision":"Allow","diagnostics":{"reason":[],"errors":[]}}`, false, true},
		{"Deny", `{"decision":"Deny","diagnostics":{"reason":[],"errors":[]}}`, false, false},
		{"a lowercase allow", `{"decision":"allow","diagnostics":{"reason":[],"errors":[]}}`, false, false},
		{"an empty decision", `{"diagnostics":{"reason":[],"errors":[]}}`, false, false},
		{"an unexpected value", `{"decision":"Maybe","diagnostics":{"reason":[],"errors":[]}}`, false, false},
		{"not JSON", `<html>gateway</html>`, true, false},
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
				require.Error(t, err)
				assert.False(t, decision.Allow)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPass, decision.Allow)
		})
	}
}

func TestAuthorizeTreatsANon200AsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"reason":"You have malformed a bad request"}`))
	}))
	defer server.Close()

	agent, err := NewCedarAgent(server.URL, 0)
	require.NoError(t, err)

	decision, err := agent.Authorize(context.Background(), validQuery())
	require.Error(t, err)
	assert.False(t, decision.Allow, "an engine error is a denial")
}

func TestAuthorizeTimesOutRatherThanHanging(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	// Close waits for the in-flight handler, so the handler has to be released
	// first. Two defers would run in the other order and deadlock the test.
	defer func() {
		close(release)
		server.Close()
	}()

	agent, err := NewCedarAgent(server.URL, 50*time.Millisecond)
	require.NoError(t, err)

	start := time.Now()
	_, err = agent.Authorize(context.Background(), validQuery())
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "a wedged engine must not hold the request open")
}

// Ready is the gate on cedar-agent's best-effort file loading: a bundle that did
// not load leaves the store empty and the agent answering 200 OK with Deny to
// everything, which at request time is indistinguishable from a policy nobody
// wrote.
func TestReadyRefusesAnEmptyStore(t *testing.T) {
	tests := []struct {
		name     string
		policies string
		data     string
		wantErr  bool
	}{
		{"both loaded", `[{"id":"p"}]`, `[{"uid":{"type":"NKE::Role","id":"reader"}}]`, false},
		{"no policies", `[]`, `[{"uid":{"type":"NKE::Role","id":"reader"}}]`, true},
		{"no entities", `[{"id":"p"}]`, `[]`, true},
		{"neither", `[]`, `[]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case policiesPath:
					_, _ = w.Write([]byte(tt.policies))
				case dataPath:
					_, _ = w.Write([]byte(tt.data))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			agent, err := NewCedarAgent(server.URL, 0)
			require.NoError(t, err)

			err = agent.Ready(context.Background())
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestReadyReportsAnUnreachableEngine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close() // nothing is listening now

	agent, err := NewCedarAgent(endpoint, 100*time.Millisecond)
	require.NoError(t, err)

	require.Error(t, agent.Ready(context.Background()))
}

func validQuery() Query {
	return Query{
		Principal: "system:serviceaccount:nke-system:cli", Issuer: "kube-apiserver",
		Action: "getCluster", ResourceType: "NKE::Cluster", ResourceID: "abc", Method: http.MethodGet,
	}
}
