// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	hasScope  bool
	calls     int
}

func (s *issuerStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.gotID, s.gotSecret, _ = r.BasicAuth()
		_ = r.ParseForm()
		s.gotScope = r.Form.Get("scope")
		s.hasScope = r.Form.Has("scope")
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

// A datahall minting from its own Keycloak realm is asked for no scope: the
// realm has no tokenScope and rejects a request for it with 400 invalid_scope.
func TestTokenAsksADatahallsOwnRealmForNoScope(t *testing.T) {
	t.Setenv(testClientIDVar, "realm-client")
	t.Setenv(testClientSecretVar, "realm-secret")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted"}`}
	srv := stub.server(t)
	realm := Site{Key: testSite, API: srv.URL, IssuerPath: "/realms/r/protocol/openid-connect"}
	if _, err := newSiteTokenSource(srv.Client(), srv.URL, realm).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stub.hasScope {
		t.Errorf("asked a realm for scope %q, want none", stub.gotScope)
	}

	// SSA still gets tokenScope.
	ssa := Site{Key: testSite, API: srv.URL}
	if _, err := newSiteTokenSource(srv.Client(), srv.URL, ssa).Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stub.gotScope != tokenScope {
		t.Errorf("asked SSA for scope %q, want %q", stub.gotScope, tokenScope)
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

func TestCACertEnvNameFollowsTheKey(t *testing.T) {
	if got := CACertEnvName("datahall-3"); got != "DSX_DATAHALL_3_CA_CERT" {
		t.Errorf("CACertEnvName = %q", got)
	}
}

func TestIssuerIsTheDatahallsOwnWhenItDeclaresAPath(t *testing.T) {
	t.Setenv(EnvIssuer, "https://ssa.example")

	own := Site{Key: "datahall-3", API: "https://hall3.example/", IssuerPath: "/realms/r/protocol/openid-connect/"}
	if got := issuerFor(own); got != "https://hall3.example/realms/r/protocol/openid-connect" {
		t.Errorf("issuer = %q, want the API host plus the declared path", got)
	}
	// A datahall that declares none keeps minting from SSA_ISSUER.
	if got := issuerFor(Site{Key: testSite, API: "https://hall1.example"}); got != "https://ssa.example" {
		t.Errorf("issuer = %q, want SSA_ISSUER", got)
	}
}

func TestReuseFollowsTheTokensLifetime(t *testing.T) {
	for expiresIn, want := range map[int]time.Duration{
		0:   tokenReuse,       // not reported: the old fixed window
		900: tokenReuse,       // SSA's fifteen minutes: unchanged
		300: 4 * time.Minute,  // Keycloak's five: a minute short of it
		30:  15 * time.Second, // shorter than the margin: half of it
	} {
		if got := reuseFor(expiresIn); got != want {
			t.Errorf("reuseFor(%d) = %s, want %s", expiresIn, got, want)
		}
	}
}

func TestExpiresInIsReadLeniently(t *testing.T) {
	for raw, want := range map[string]int{
		``:       0,
		`300`:    300,
		`"300"`:  300,
		`299.9`:  299,
		`"soon"`: 0,
		`null`:   0,
		`-5`:     0,
	} {
		if got := expiresSeconds([]byte(raw)); got != want {
			t.Errorf("expiresSeconds(%s) = %d, want %d", raw, got, want)
		}
	}
}

func TestTokenSurvivesAnOddExpiresIn(t *testing.T) {
	t.Setenv(testClientIDVar, "client")
	t.Setenv(testClientSecretVar, "supplied")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted","expires_in":{"odd":true}}`}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, testSite)
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("an unreadable expires_in should fall back to the default reuse, not fail the mint: %v", err)
	}
	if left := time.Until(source.expires); left > tokenReuse || left < tokenReuse-time.Minute {
		t.Errorf("reused for %s, want about %s", left, tokenReuse)
	}
}

func TestTokenHonoursExpiresIn(t *testing.T) {
	t.Setenv(testClientIDVar, "gb300-client")
	t.Setenv(testClientSecretVar, "supplied")

	stub := &issuerStub{status: http.StatusOK, body: `{"access_token":"minted","expires_in":300}`}
	srv := stub.server(t)
	source := newTokenSource(srv.Client(), srv.URL, testSite)
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(source.expires); left > 4*time.Minute || left < 3*time.Minute {
		t.Errorf("a 300s token is reused for %s, want about four minutes", left)
	}
}

// caPEM is the certificate a TLS test server presents, as a datahall's
// private CA would be supplied.
func caPEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

func TestDatahallCAIsTrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)

	get := func(client *http.Client) error {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	// Without the CA the private certificate does not verify, which is what
	// this datahall's API looks like to a client on the system roots alone.
	plain := &http.Client{Timeout: 5 * time.Second}
	if err := get(plain); err == nil {
		t.Fatal("a private CA verified without being supplied")
	}

	t.Setenv("DSX_DATAHALL_3_CA_CERT", caPEM(srv))
	client, err := withDatahallCA(plain, "datahall-3")
	if err != nil {
		t.Fatal(err)
	}
	if err := get(client); err != nil {
		t.Fatalf("the supplied CA was not trusted: %v", err)
	}
	if client.Timeout != plain.Timeout {
		t.Error("adding the CA dropped the client's timeout")
	}
}

func TestDatahallCAIsOptionalAndValidated(t *testing.T) {
	plain := &http.Client{}
	got, err := withDatahallCA(plain, "datahall-9")
	if err != nil || got != plain {
		t.Errorf("no CA supplied should return the client unchanged, got %v, %v", got, err)
	}

	t.Setenv("DSX_DATAHALL_9_CA_CERT", "not a certificate")
	if _, err := withDatahallCA(plain, "datahall-9"); err == nil || !strings.Contains(err.Error(), "DSX_DATAHALL_9_CA_CERT") {
		t.Errorf("a value holding no PEM should fail naming the variable, got %v", err)
	}
}
