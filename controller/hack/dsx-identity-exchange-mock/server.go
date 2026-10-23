// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	grantTypeTokenExchange  = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- public RFC 8693 identifier, not a credential.
	tokenTypeJWT            = "urn:ietf:params:oauth:token-type:jwt"            // #nosec G101 -- public RFC 8693 identifier, not a credential.
	contentTypeFormEncoded  = "application/x-www-form-urlencoded"
	maxExchangeRequestBytes = 1 << 20
	discoveryCacheControl   = "public, max-age=60"
)

var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

type tokenExchangeResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

type tokenExchangeError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

type validatedToken struct {
	ID       spiffeid.ID
	Audience []string
	Expiry   time.Time
	Claims   map[string]any
}

type tokenValidator interface {
	Validate(token string) (validatedToken, error)
}

type jwtSVIDValidator struct {
	bundles jwtbundle.Source
}

func (v jwtSVIDValidator) Validate(raw string) (validatedToken, error) {
	svid, err := jwtsvid.ParseAndValidate(raw, v.bundles, nil)
	if err != nil {
		return validatedToken{}, err
	}
	return validatedToken{
		ID:       svid.ID,
		Audience: append([]string(nil), svid.Audience...),
		Expiry:   svid.Expiry,
		Claims:   svid.Claims,
	}, nil
}

// exchangeServer models the identity boundary between the management-cluster
// SPIRE deployment and a tenant apiserver. It accepts a locally issued
// JWT-SVID, verifies that credential before trusting its claims, rewrites the
// source SPIFFE ID into the DSX identity shape, and signs a replacement JWT
// that the tenant apiserver can validate through this server's OIDC endpoints.
type exchangeServer struct {
	// validator verifies the source JWT-SVID against the local SPIRE JWT bundle.
	// Subject rewriting and re-signing happen only after this succeeds.
	validator tokenValidator

	// signer contains the ephemeral ES256 private key used for replacement
	// tokens. publicKey is the matching verification key returned by the JWKS
	// endpoint. A restart deliberately rotates this test-only key pair.
	signer    jose.Signer
	publicKey jose.JSONWebKey

	// issuer becomes the replacement token's iss claim. jwksURL is advertised
	// by OIDC discovery and must be reachable from the tenant apiserver; the two
	// URLs are separate because the issuer is an identity while JWKS describes
	// where that identity's current verification keys can be fetched.
	issuer  string
	jwksURL string

	// region and partition form the rewritten DSX trust domain. For example,
	// spiffe://capd.nke.local/tenant/nke/service/nvke-controller/bm/cwe
	// becomes spiffe://nke.capd.nvke-controller.nvid.id/bm/cwe when region
	// is "capd" and partition is "nvid.id".
	region    string
	partition string

	// now is injected so expires_in calculations are deterministic in tests.
	now func() time.Time
}

func newExchangeServer(issuer, jwksURL, region, partition string, validator tokenValidator) (*exchangeServer, error) {
	if validator == nil {
		return nil, errors.New("token validator is required")
	}
	if err := validateHTTPSURL("issuer", issuer, true); err != nil {
		return nil, err
	}
	if err := validateHTTPSURL("JWKS URL", jwksURL, false); err != nil {
		return nil, err
	}
	if len(validation.IsDNS1123Label(region)) != 0 {
		return nil, fmt.Errorf("invalid region %q", region)
	}
	if len(validation.IsDNS1123Subdomain(partition)) != 0 {
		return nil, fmt.Errorf("invalid partition %q", partition)
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	publicKey := jose.JSONWebKey{Key: &privateKey.PublicKey, Algorithm: string(jose.ES256), Use: "sig"}
	thumbprint, err := publicKey.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("calculate signing key thumbprint: %w", err)
	}
	publicKey.KeyID = base64.RawURLEncoding.EncodeToString(thumbprint)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: privateKey},
		(&jose.SignerOptions{}).
			WithType("JWT").
			WithHeader(jose.HeaderKey("kid"), publicKey.KeyID),
	)
	if err != nil {
		return nil, fmt.Errorf("create JWT signer: %w", err)
	}

	return &exchangeServer{
		validator: validator,
		signer:    signer,
		publicKey: publicKey,
		issuer:    issuer,
		jwksURL:   jwksURL,
		region:    region,
		partition: partition,
		now:       time.Now,
	}, nil
}

func validateHTTPSURL(name, raw string, hostOnly bool) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an HTTPS URL", name)
	}
	if hostOnly && (parsed.Port() != "" || parsed.Path != "" || parsed.RawQuery != "") {
		return fmt.Errorf("%s must contain only an HTTPS DNS hostname", name)
	}
	return nil
}

func (s *exchangeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/exchange", s.handleTokenExchange)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /live", handleHealthy)
	mux.HandleFunc("GET /ready", handleHealthy)
	return mux
}

func (s *exchangeServer) handleTokenExchange(w http.ResponseWriter, r *http.Request) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != contentTypeFormEncoded {
		writeTokenError(w, http.StatusUnsupportedMediaType, "invalid_request", "Content-Type must be application/x-www-form-urlencoded")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxExchangeRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "failed to parse form data")
		return
	}
	if r.PostForm.Get("grant_type") != grantTypeTokenExchange {
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be "+grantTypeTokenExchange)
		return
	}
	subjectToken := r.PostForm.Get("subject_token")
	if subjectToken == "" {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject_token is required")
		return
	}
	if r.PostForm.Get("subject_token_type") != tokenTypeJWT {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject_token_type must be "+tokenTypeJWT)
		return
	}
	if requested := r.PostForm.Get("requested_token_type"); requested != "" && requested != tokenTypeJWT {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "requested_token_type must be "+tokenTypeJWT)
		return
	}

	validated, err := s.validator.Validate(subjectToken)
	if err != nil {
		slog.WarnContext(r.Context(), "Rejected test token exchange", "reason", "source_token_validation", "error", err)
		writeTokenError(w, http.StatusUnauthorized, "invalid_grant", "subject_token validation failed")
		return
	}
	exchangedID, err := exchangeSubject(s.region, s.partition, validated.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "Rejected test token exchange", "reason", "unsupported_subject", "error", err)
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "subject_token subject is not supported")
		return
	}
	token, err := s.resign(validated, exchangedID)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to sign test exchange token", "error", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "failed to sign JWT")
		return
	}

	expiresIn := max(int64(validated.Expiry.Sub(s.now()).Seconds()), 0)
	writeJSON(w, http.StatusOK, tokenExchangeResponse{
		AccessToken:     token,
		IssuedTokenType: tokenTypeJWT,
		TokenType:       "Bearer",
		ExpiresIn:       expiresIn,
	}, "no-store")
	slog.InfoContext(r.Context(), "Accepted test token exchange", "subject", exchangedID.String(), "audience_count", len(validated.Audience))
}

func (s *exchangeServer) resign(validated validatedToken, subject spiffeid.ID) (string, error) {
	claims := make(map[string]any, 7)
	for _, name := range []string{"aud", "iat", "jti", "nbf"} {
		if value, ok := validated.Claims[name]; ok {
			claims[name] = value
		}
	}
	claims["exp"] = validated.Expiry.Unix()
	claims["iss"] = s.issuer
	claims["sub"] = subject.String()
	token, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return token, nil
}

func exchangeSubject(region, partition string, id spiffeid.ID) (spiffeid.ID, error) {
	path := strings.TrimPrefix(id.Path(), "/")
	segments := strings.Split(path, "/")
	if len(segments) < 6 || segments[0] != "tenant" || segments[2] != "service" {
		return spiffeid.ID{}, fmt.Errorf("invalid local SPIFFE identity path %q", id.Path())
	}
	tenant, service := segments[1], segments[3]
	if len(validation.IsDNS1123Label(tenant)) != 0 || len(validation.IsDNS1123Label(service)) != 0 {
		return spiffeid.ID{}, errors.New("tenant and service must be DNS labels")
	}
	if resourceType := segments[4]; resourceType != "bm" && resourceType != "dpu" && resourceType != "vm" {
		return spiffeid.ID{}, fmt.Errorf("unsupported resource type %q", resourceType)
	}
	if !resourceIDPattern.MatchString(segments[5]) {
		return spiffeid.ID{}, fmt.Errorf("invalid resource ID %q", segments[5])
	}

	trustDomain, err := spiffeid.TrustDomainFromString(strings.Join([]string{tenant, region, service, partition}, "."))
	if err != nil {
		return spiffeid.ID{}, fmt.Errorf("build exchanged trust domain: %w", err)
	}
	exchanged, err := spiffeid.FromSegments(trustDomain, segments[4:]...)
	if err != nil {
		return spiffeid.ID{}, fmt.Errorf("build exchanged SPIFFE ID: %w", err)
	}
	return exchanged, nil
}

func (s *exchangeServer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{s.publicKey}}, discoveryCacheControl)
}

func (s *exchangeServer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                "",
		"token_endpoint":                        "",
		"device_authorization_endpoint":         "",
		"userinfo_endpoint":                     "",
		"jwks_uri":                              s.jwksURL,
		"id_token_signing_alg_values_supported": []string{string(jose.ES256)},
	}, discoveryCacheControl)
}

func handleHealthy(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func writeTokenError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, tokenExchangeError{Error: code, ErrorDescription: description}, "no-store")
}

func writeJSON(w http.ResponseWriter, status int, value any, cacheControl string) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write test exchange response", "error", err)
	}
}
