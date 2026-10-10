// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// issuerStub answers the token endpoint, recording what it was presented.
type issuerStub struct {
	status int
	body   string

	gotID     string
	gotSecret string
	gotScope  string
	calls     int
}

func (s *issuerStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.gotID, s.gotSecret, _ = r.BasicAuth()
		_ = r.ParseForm()
		s.gotScope = r.Form.Get("scope")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenPrefersTheEnvironmentAndNeverReadsTheCluster(t *testing.T) {
	t.Setenv(EnvClientID, "nvssa-prd-supplied")
	t.Setenv(EnvClientSecret, "ssap-supplied")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted"}`}
	srv := stub.server(t)

	source := newTokenSource(srv.Client(), srv.URL, "az51-dev2-dh2")
	// A cluster read here would be a bug: the whole point of supplying a
	// client is that a runner needs no Kubernetes access.
	source.sources[1] = &clusterCredentials{Context: "az51-dev2-dh2", run: func(context.Context, ...string) ([]byte, error) {
		t.Error("the cluster was read even though a client was supplied")
		return nil, nil
	}}

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "minted" {
		t.Errorf("token = %q, want the minted one", token)
	}
	if stub.gotID != "nvssa-prd-supplied" || stub.gotSecret != "ssap-supplied" {
		t.Errorf("presented %q, want the supplied client", stub.gotID)
	}
	if stub.gotScope != tokenScope {
		t.Errorf("scope = %q, want %q", stub.gotScope, tokenScope)
	}
	if origin := source.Origin(); origin != "the environment" {
		t.Errorf("Origin() = %q, want the environment", origin)
	}
}

func TestTokenFallsBackToTheClusterAndPinsItsContext(t *testing.T) {
	// Neither variable set, so the datahall's own client is read.
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted"}`}
	srv := stub.server(t)

	var contexts []string
	source := newTokenSource(srv.Client(), srv.URL, "az51-dev2-dh2")
	source.sources[1] = &clusterCredentials{Context: "az51-dev2-dh2", run: func(_ context.Context, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "--context" && i+1 < len(args) {
				contexts = append(contexts, args[i+1])
			}
		}
		if strings.Contains(strings.Join(args, " "), "client_id") {
			return []byte("nvssa-prd-site\n"), nil
		}
		return []byte("ssap-site\n"), nil
	}}

	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stub.gotID != "nvssa-prd-site" {
		t.Errorf("presented %q, want the client read from the cluster", stub.gotID)
	}
	// Ambient context is what sends a run at the wrong datahall; every read
	// has to name the one it means.
	if len(contexts) == 0 {
		t.Fatal("the cluster was read with no --context, so it used whatever was current")
	}
	for _, kubeContext := range contexts {
		if kubeContext != "az51-dev2-dh2" {
			t.Errorf("read context %q, want the datahall's own", kubeContext)
		}
	}
	if origin := source.Origin(); !strings.Contains(origin, "az51-dev2-dh2") {
		t.Errorf("Origin() = %q, want it to name the cluster read", origin)
	}
}

func TestTokenIsReusedUntilItExpires(t *testing.T) {
	t.Setenv(EnvClientID, "nvssa-prd-supplied")
	t.Setenv(EnvClientSecret, "ssap-supplied")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted"}`}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, "")

	for range 3 {
		if _, err := source.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if stub.calls != 1 {
		t.Errorf("exchanged %d times, want 1 -- a cached token should be reused", stub.calls)
	}
}

func TestTokenDoesNotFallThroughFromARejectedClient(t *testing.T) {
	// The LaunchLayer-credential case: a client was supplied and the nico
	// issuer refused it. Falling through to the cluster would report "the
	// cluster could not be read", naming neither problem.
	t.Setenv(EnvClientID, "nvssa-prd-launchlayer")
	t.Setenv(EnvClientSecret, "ssap-launchlayer")

	stub := &issuerStub{status: http.StatusUnauthorized}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, "az51-dev2-dh2")
	source.sources[1] = &clusterCredentials{Context: "az51-dev2-dh2", run: func(context.Context, ...string) ([]byte, error) {
		t.Error("the cluster was read after a supplied client was rejected")
		return nil, nil
	}}

	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("a rejected client should fail the run")
	}
	// The issuer, because the two identities are indistinguishable by value
	// and the issuer is the only thing that says which API refused.
	for _, want := range []string{"the environment", srv.URL, "nvssa-prd-launchlayer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "ssap-launchlayer") {
		t.Error("the client secret reached the error")
	}
}

func TestTokenReportsEverySourceWhenNoneYields(t *testing.T) {
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	source := newTokenSource(http.DefaultClient, "https://issuer.example", "az51-dev2-dh2")
	source.sources[1] = &clusterCredentials{Context: "az51-dev2-dh2", run: func(context.Context, ...string) ([]byte, error) {
		return nil, fmt.Errorf("no context named az51-dev2-dh2")
	}}

	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("no available client should fail the run")
	}
	for _, want := range []string{EnvClientID, "az51-dev2-dh2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
}

func TestTokenRequiresAnIssuer(t *testing.T) {
	t.Setenv(EnvClientID, "nvssa-prd-supplied")
	t.Setenv(EnvClientSecret, "ssap-supplied")

	source := newTokenSource(http.DefaultClient, "", "")
	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("no issuer should fail before anything is presented")
	}
	if !strings.Contains(err.Error(), EnvIssuer) {
		t.Errorf("error should name %s, got %v", EnvIssuer, err)
	}
}

func TestClusterCredentialsRefuseAnEmptyContext(t *testing.T) {
	_, _, err := (&clusterCredentials{}).Credentials(context.Background())
	if err == nil {
		t.Fatal("reading the cluster with no context should fail rather than use the current one")
	}
}
