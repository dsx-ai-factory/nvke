// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The datahall these tests mint for, and the variables its client is supplied
// under. Spelled out rather than computed, so a change to the derivation shows
// up here as a failure rather than agreeing with itself.
const (
	testSite            = "datahall-1"
	testClientIDVar     = "DSX_DATAHALL_1_SSA_CLIENT_ID"
	testClientSecretVar = "DSX_DATAHALL_1_SSA_CLIENT_SECRET" // #nosec G101 -- a variable name
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

func TestTokenIsReusedUntilItExpires(t *testing.T) {
	t.Setenv(testClientIDVar, "nvssa-prd-supplied")
	t.Setenv(testClientSecretVar, "ssap-supplied")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted"}`}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, testSite)

	for range 3 {
		if _, err := source.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if stub.calls != 1 {
		t.Errorf("exchanged %d times, want 1 -- a cached token should be reused", stub.calls)
	}
}

func TestTokenRequiresAnIssuer(t *testing.T) {
	t.Setenv(testClientIDVar, "nvssa-prd-supplied")
	t.Setenv(testClientSecretVar, "ssap-supplied")

	source := newTokenSource(http.DefaultClient, "", testSite)
	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("no issuer should fail before anything is presented")
	}
	if !strings.Contains(err.Error(), EnvIssuer) {
		t.Errorf("error should name %s, got %v", EnvIssuer, err)
	}
}

func TestClientEnvNamesFollowTheDatahall(t *testing.T) {
	// The whole point of deriving the name: a credential scoped to one
	// datahall cannot be picked up by a run against another.
	for site, want := range map[string]string{
		"datahall-1": "DSX_DATAHALL_1_SSA_CLIENT_ID",
		"datahall-2": "DSX_DATAHALL_2_SSA_CLIENT_ID",
		// Still derived from whatever the key is, so a key that is not of the
		// datahall-N shape keeps working.
		"other.key-3": "DSX_OTHER_KEY_3_SSA_CLIENT_ID",
	} {
		id, secret := ClientEnvNames(site)
		if id != want {
			t.Errorf("ClientEnvNames(%q) id = %q, want %q", site, id, want)
		}
		if secret != strings.TrimSuffix(want, "_ID")+"_SECRET" {
			t.Errorf("ClientEnvNames(%q) secret = %q, want it paired with the id", site, secret)
		}
	}
	// And it is nothing a LaunchLayer credential would be found under.
	id, _ := ClientEnvNames("datahall-1")
	if id == "SSA_CLIENT_ID" {
		t.Error("the derived name collides with ll-check-creds.sh's bare variable")
	}
}

func TestTokenPresentsTheDatahallsClient(t *testing.T) {
	t.Setenv(testClientIDVar, "nvssa-prd-supplied")
	t.Setenv(testClientSecretVar, "ssap-supplied")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted"}`}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, testSite)

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "minted" {
		t.Errorf("token = %q, want the minted one", token)
	}
	if stub.gotID != "nvssa-prd-supplied" || stub.gotSecret != "ssap-supplied" {
		t.Errorf("presented %q, want the datahall's client", stub.gotID)
	}
	if stub.gotScope != tokenScope {
		t.Errorf("scope = %q, want %q", stub.gotScope, tokenScope)
	}
}

func TestTokenRequiresTheDatahallsClient(t *testing.T) {
	// Asserted rather than discovered. A datahall keeps a bootstrap client of
	// its own, but that identity belongs to the site and bypasses the org
	// scoping a run is supposed to stay inside, so a lane asks for its own.
	t.Setenv(testClientIDVar, "")
	t.Setenv(testClientSecretVar, "")

	source := newTokenSource(http.DefaultClient, "https://issuer.example", testSite)
	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("a run with no client should fail rather than reach for the site's own")
	}
	// Both names, because setting one of the pair is the likely mistake.
	for _, want := range []string{testSite, testClientIDVar, testClientSecretVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
}

func TestTokenRejectionNamesTheIssuerAndTheClient(t *testing.T) {
	// A LaunchLayer client presented here looks exactly like a wrong one: both
	// are nvssa-prd-* from the same issuer, so the rejection has to say which
	// API refused and which identity it refused.
	t.Setenv(testClientIDVar, "nvssa-prd-launchlayer")
	t.Setenv(testClientSecretVar, "ssap-launchlayer")

	stub := &issuerStub{status: http.StatusUnauthorized}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, testSite)

	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("a rejected client should fail the run")
	}
	for _, want := range []string{"nvssa-prd-launchlayer", srv.URL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "ssap-launchlayer") {
		t.Error("the client secret reached the error")
	}
}
