// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// envPrefix begins the name of every variable a datahall's credentials are
	// supplied under. The rest of the name is the datahall itself -- see
	// ClientEnvNames.
	envPrefix = "DSX_"

	// EnvIssuer names the SSA issuer BASE url, not the token endpoint: the
	// exchange posts to issuer + "/token". Setting it to the endpoint posts to
	// .../token/token, which answers as a rejected client and so reads as a
	// bad credential rather than a bad URL.
	//
	// It has no default: the issuer names infrastructure this repository does
	// not, so a run supplies it the same way it supplies a site id.
	EnvIssuer = "SSA_ISSUER"

	// tokenScope is what a nico token is minted for.
	tokenScope = "carbide"

	// tokenReuse is shorter than the fifteen minutes a DSX token has been seen
	// to carry, so one is never presented stale. An exchange is one small
	// request, and a token that expires mid-run is how a tenant create fails
	// twenty minutes into a bare-metal run.
	tokenReuse = 5 * time.Minute
)

var nonAlphanumeric = regexp.MustCompile(`[^A-Z0-9]+`)

// ClientEnvNames are the variables a datahall's SSA client is supplied under,
// derived from the datahall's own name.
//
// Per datahall, not global, because the client is: each site keeps its own, and
// one scoped to one datahall means nothing at another. Deriving the
// name from the key rather than declaring it also means a second datahall needs
// no code and no new convention to remember.
//
// It removes an overlap as well. ll-check-creds.sh takes a bare SSA_CLIENT_ID
// for a LaunchLayer client -- a different identity against a different API, and
// indistinguishable from this one by its value, since both are nvssa-prd-* from
// the same issuer. A name no one else uses cannot be picked up by accident.
func ClientEnvNames(siteKey string) (string, string) {
	name := envPrefix + nonAlphanumeric.ReplaceAllString(strings.ToUpper(siteKey), "_")
	return name + "_SSA_CLIENT_ID", name + "_SSA_CLIENT_SECRET"
}

// clientCredentials is an SSA client id and the secret paired with it.
//
// The secret is never logged, never put in an error, and never carried
// anywhere but into the exchange below. The id is not a secret, and naming it
// in a rejection is the only way an operator can tell which identity was
// presented -- which matters because every client this repository might hold is
// nvssa-prd-* from the same issuer, and nothing about the value says which API
// it was issued for.
type clientCredentials struct {
	ID     string
	Secret string
}

// tokenSource mints a bearer token for a datahall's nico API.
//
// The client is supplied, not discovered. A datahall also keeps a bootstrap
// client of its own in its cluster, which is what both recorded manual runs
// borrowed -- but that identity belongs to the site rather than to whoever is
// running, it needs Kubernetes access to read, and it bypasses the org scoping
// a run is supposed to stay inside. A lane asks for its own and fails without
// one.
type tokenSource struct {
	client  *http.Client
	issuer  string
	siteKey string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newTokenSource(client *http.Client, issuer, siteKey string) *tokenSource {
	return &tokenSource{
		client:  client,
		issuer:  strings.TrimRight(strings.TrimSpace(issuer), "/"),
		siteKey: siteKey,
	}
}

// credentials are the datahall's client, or an error naming the two variables
// it was looked for under.
func (s *tokenSource) credentials() (clientCredentials, error) {
	idVar, secretVar := ClientEnvNames(s.siteKey)
	id, secret := strings.TrimSpace(os.Getenv(idVar)), os.Getenv(secretVar)
	if id == "" || secret == "" {
		return clientCredentials{}, fmt.Errorf("no SSA client for datahall %s: set %s and %s", s.siteKey, idVar, secretVar)
	}
	return clientCredentials{ID: id, Secret: secret}, nil
}

// Token returns a bearer token, minting one when the cached one is spent.
func (s *tokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expires) {
		return s.token, nil
	}
	if s.issuer == "" {
		return "", fmt.Errorf("%s is not set, so no token can be minted", EnvIssuer)
	}
	credentials, err := s.credentials()
	if err != nil {
		return "", err
	}
	token, err := s.exchange(ctx, credentials)
	if err != nil {
		return "", err
	}
	s.token, s.expires = token, time.Now().Add(tokenReuse)
	return s.token, nil
}

// exchange trades a client for a bearer token.
func (s *tokenSource) exchange(ctx context.Context, credentials clientCredentials) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {tokenScope}}
	// #nosec G704 -- the issuer is declared configuration for this run, which is
	// what makes it overridable per deployment; it is not reached from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building the token request: %w", err)
	}
	req.SetBasicAuth(credentials.ID, credentials.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	// #nosec G704 -- same request, same declared issuer
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchanging the client for a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it is an auth service's reply to a
		// credential, and a run log is not where that belongs. The issuer
		// rejects an unknown client with an EMPTY 401 body in any case, so
		// there would be nothing to echo -- which is why the issuer and the
		// client id are what a reader gets. A client issued for a different
		// API looks exactly like a wrong one here, so both are named.
		return "", fmt.Errorf("client %s was rejected by %s: %s", credentials.ID, s.issuer, resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("the token response carried no access_token")
	}
	return out.AccessToken, nil
}

// MintToken returns a bearer token for a datahall, for a caller that needs the
// credential itself rather than a provider.
//
// For the e2e process's own API calls. The site's CAPNICo is given the client
// instead -- see SSAClient -- because a token lives fifteen minutes and a site
// controller outlives the run that built it.
func MintToken(ctx context.Context, site Site) (string, error) {
	source := newTokenSource(&http.Client{Timeout: time.Minute}, os.Getenv(EnvIssuer), site.Key)
	token, err := source.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("dsx: %w", err)
	}
	return token, nil
}

// SSAClient is the datahall's OAuth client, for a caller that wants tokens
// minted on demand rather than once.
//
// CAPNICo accepts either a static token or these four fields and refuses a
// Secret carrying both, so handing it the client is what turns a credential
// that expires during a run into one that lasts as long as the site.
type SSAClient struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
}

// SSAClientFor is the client a datahall's tokens are minted from.
//
// The URL is built here rather than by the caller because EnvIssuer is the
// issuer base and the exchange is at issuer + "/token"; a caller appending it
// a second time posts to .../token/token, which answers as a rejected client
// and so reads as a bad secret rather than a bad URL.
func SSAClientFor(site Site) (SSAClient, error) {
	issuer := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvIssuer)), "/")
	if issuer == "" {
		return SSAClient{}, fmt.Errorf("dsx: %s is not set, so CAPNICo has nothing to mint tokens with", EnvIssuer)
	}
	credentials, err := newTokenSource(nil, issuer, site.Key).credentials()
	if err != nil {
		return SSAClient{}, fmt.Errorf("dsx: %w", err)
	}
	return SSAClient{
		TokenURL:     issuer + "/token",
		ClientID:     credentials.ID,
		ClientSecret: credentials.Secret,
		Scope:        tokenScope,
	}, nil
}
