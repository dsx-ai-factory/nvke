// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	// tokenMargin is how long before the issuer's own expires_in a token
	// stops being reused. A Keycloak datahall issues five-minute tokens, which
	// tokenReuse alone would present to the last second.
	tokenMargin = time.Minute
)

var nonAlphanumeric = regexp.MustCompile(`[^A-Z0-9]+`)

// datahallEnvName is the variable a datahall-scoped setting is supplied under.
func datahallEnvName(siteKey, suffix string) string {
	return envPrefix + nonAlphanumeric.ReplaceAllString(strings.ToUpper(siteKey), "_") + suffix
}

// CACertEnvName names the PEM certificate of the private CA a datahall serves
// its API under. Unset means the system roots verify it.
func CACertEnvName(siteKey string) string { return datahallEnvName(siteKey, "_CA_CERT") }

// issuerFor is the issuer base a datahall mints from: its own API host plus
// its issuerPath when it declares one, else SSA_ISSUER.
func issuerFor(site Site) string {
	if path := strings.Trim(strings.TrimSpace(site.IssuerPath), "/"); path != "" {
		return site.Endpoint() + "/" + path
	}
	return os.Getenv(EnvIssuer)
}

// withDatahallCA returns client trusting the datahall's private CA as well as
// the roots it already trusts, or client itself when the datahall supplies none.
func withDatahallCA(client *http.Client, siteKey string) (*http.Client, error) {
	name := CACertEnvName(siteKey)
	pemData := os.Getenv(name)
	if strings.TrimSpace(pemData) == "" {
		return client, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(pemData)) {
		return nil, fmt.Errorf("%s holds no PEM certificate", name)
	}
	var transport *http.Transport
	switch base := client.Transport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("the default HTTP transport is not an *http.Transport")
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		transport = base.Clone()
	default:
		return nil, fmt.Errorf("%s cannot be added to a client with a custom transport", name)
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// Extend a supplied pool, on a copy: the cloned config still shares it.
	if supplied := transport.TLSClientConfig.RootCAs; supplied != nil {
		pool = supplied.Clone()
		pool.AppendCertsFromPEM([]byte(pemData))
	}
	transport.TLSClientConfig.RootCAs = pool
	withCA := *client
	withCA.Transport = transport
	return &withCA, nil
}

// DatahallCA is the PEM of the private CA the datahall serves its API under,
// for a client this package does not build, such as CAPNICo's. Empty when the
// system roots verify the API.
func DatahallCA(siteKey string) (string, error) {
	name := CACertEnvName(siteKey)
	pemData := os.Getenv(name)
	if strings.TrimSpace(pemData) == "" {
		return "", nil
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(pemData)) {
		return "", fmt.Errorf("%s holds no PEM certificate", name)
	}
	return pemData, nil
}

// reuseFor is how long a token the issuer says lives expiresIn seconds is
// reused: tokenReuse, or less when the token lives shorter.
func reuseFor(expiresIn int) time.Duration {
	if expiresIn <= 0 {
		return tokenReuse
	}
	lifetime := time.Duration(expiresIn) * time.Second
	window := lifetime - tokenMargin
	if window <= 0 {
		window = lifetime / 2
	}
	return min(tokenReuse, window)
}

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
	return datahallEnvName(siteKey, "_SSA_CLIENT_ID"), datahallEnvName(siteKey, "_SSA_CLIENT_SECRET")
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
	// scope is asked for only when set.
	scope string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newTokenSource(client *http.Client, issuer, siteKey string) *tokenSource {
	return &tokenSource{
		client:  client,
		issuer:  strings.TrimRight(strings.TrimSpace(issuer), "/"),
		siteKey: siteKey,
		scope:   tokenScope,
	}
}

// newSiteTokenSource is newTokenSource asking for the scope site's issuer knows.
func newSiteTokenSource(client *http.Client, issuer string, site Site) *tokenSource {
	source := newTokenSource(client, issuer, site.Key)
	source.scope = scopeFor(site)
	return source
}

// scopeFor is the scope a datahall's tokens are asked for: tokenScope from SSA,
// none from a datahall's own realm, which has no such scope and answers a
// request for it with 400 invalid_scope.
func scopeFor(site Site) string {
	if strings.Trim(strings.TrimSpace(site.IssuerPath), "/") != "" {
		return ""
	}
	return tokenScope
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
		return "", fmt.Errorf("%s is not set and datahall %s declares no issuerPath, so no token can be minted", EnvIssuer, s.siteKey)
	}
	credentials, err := s.credentials()
	if err != nil {
		return "", err
	}
	// From before the request: the token's lifetime starts when it is minted.
	start := time.Now()
	token, reuse, err := s.exchange(ctx, credentials)
	if err != nil {
		return "", err
	}
	s.token, s.expires = token, start.Add(reuse)
	return s.token, nil
}

// exchange trades a client for a bearer token, and says how long to reuse it.
func (s *tokenSource) exchange(ctx context.Context, credentials clientCredentials) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if s.scope != "" {
		form.Set("scope", s.scope)
	}
	// #nosec G704 -- the issuer is declared configuration for this run, which is
	// what makes it overridable per deployment; it is not reached from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("building the token request: %w", err)
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
		return "", 0, fmt.Errorf("exchanging the client for a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("reading the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it is an auth service's reply to a
		// credential, and a run log is not where that belongs. The issuer
		// rejects an unknown client with an EMPTY 401 body in any case, so
		// there would be nothing to echo -- which is why the issuer and the
		// client id are what a reader gets. A client issued for a different
		// API looks exactly like a wrong one here, so both are named.
		return "", 0, fmt.Errorf("client %s was rejected by %s: %s", credentials.ID, s.issuer, resp.Status)
	}
	var out struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", 0, errors.New("the token response carried no access_token")
	}
	return out.AccessToken, reuseFor(expiresSeconds(out.ExpiresIn)), nil
}

// expiresSeconds reads expires_in leniently: a number, or a number in a
// string. Anything else is 0, which reuseFor treats as not reported, so an
// odd issuer cannot fail a mint that otherwise succeeded.
func expiresSeconds(raw json.RawMessage) int {
	var n json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return 0
	}
	if v, err := n.Int64(); err == nil && v > 0 {
		return int(v)
	}
	if v, err := n.Float64(); err == nil && v >= 1 {
		return int(v)
	}
	return 0
}

// MintToken returns a bearer token for a datahall, for a caller that needs the
// credential itself rather than a provider.
//
// For the e2e process's own API calls. The site's CAPNICo is given the client
// instead -- see SSAClient -- because a token lives fifteen minutes and a site
// controller outlives the run that built it.
func MintToken(ctx context.Context, site Site) (string, error) {
	client, err := withDatahallCA(&http.Client{Timeout: time.Minute}, site.Key)
	if err != nil {
		return "", fmt.Errorf("dsx: %w", err)
	}
	source := newSiteTokenSource(client, issuerFor(site), site)
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
	issuer := strings.TrimRight(strings.TrimSpace(issuerFor(site)), "/")
	if issuer == "" {
		return SSAClient{}, fmt.Errorf("dsx: %s is not set and datahall %s declares no issuerPath, so CAPNICo has nothing to mint tokens with", EnvIssuer, site.Key)
	}
	credentials, err := newTokenSource(nil, issuer, site.Key).credentials()
	if err != nil {
		return SSAClient{}, fmt.Errorf("dsx: %w", err)
	}
	return SSAClient{
		TokenURL:     issuer + "/token",
		ClientID:     credentials.ID,
		ClientSecret: credentials.Secret,
		Scope:        scopeFor(site),
	}, nil
}
