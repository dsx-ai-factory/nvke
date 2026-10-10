// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/sync/singleflight"
)

// ErrKeyNotFound means the key set is current but holds no key with that "kid",
// so verification reports bad_signature.
var ErrKeyNotFound = errors.New("no signing key with that key ID")

const (

	// jwksRefreshThrottle bounds out-of-band refreshes to one per issuer per
	// window, so an unknown "kid" cannot turn every unauthenticated request into
	// an apiserver call. A rotation takes up to this long to be picked up.
	jwksRefreshThrottle = 30 * time.Second

	// jwksMaxStaleness is how long a cached key set stays authoritative once
	// refreshes start failing. Past it KeyFor fails with ErrKeysStale rather than
	// serving keys of unknown currency.
	jwksMaxStaleness = 30 * time.Minute

	jwksFetchTimeout = 15 * time.Second

	// jwksMaxBytes caps a JWKS response; a real key set is a few hundred bytes
	// per key.
	jwksMaxBytes = 1 << 20
)

// jwksSourceFactory builds the key source for one issuer. It takes the whole
// Issuer because the URL, CA bundle and bearer token are all per-issuer, and it
// is injectable so a test can point the source at an httptest server.
type jwksSourceFactory func(iss Issuer) (*jwksKeySource, error)

// NewKeySources builds a key source for every configured issuer, fetches once,
// and returns the map NewVerifier takes, keyed by Issuer.Name. In ModeEnforcing a
// key set that could not be obtained is a returned error, so the controller exits before the
// listener opens rather than rejecting every caller; in ModePermissive it is only
// logged. Either way every issuer gets an entry, including the failed ones, so a
// later refresh recovers without a restart.
func NewKeySources(ctx context.Context, cfg *Config, mode Mode, logger *slog.Logger) (map[string]KeySource, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return newKeySources(ctx, cfg, mode, logger, jwksSourceFromConfig(logger))
}

func newKeySources(
	ctx context.Context,
	cfg *Config,
	mode Mode,
	logger *slog.Logger,
	newSource jwksSourceFactory,
) (map[string]KeySource, error) {
	if cfg == nil {
		return nil, errors.New("authentication config is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	sources := make(map[string]KeySource, len(cfg.Issuers))

	var failures []error
	for _, iss := range cfg.Issuers {
		// A missing jwks block is a configuration error, not a runtime one, so it
		// is fatal in permissive mode too. Load rejects it first; this covers a
		// Config built in code.
		if iss.JWKS == nil {
			return nil, fmt.Errorf("issuer %q (%s): jwks is required", iss.Name, iss.Issuer)
		}

		src, err := newIssuerSource(ctx, iss, newSource, logger)
		sources[iss.Name] = src
		if err != nil {
			failures = append(failures, err)
		}
	}

	if len(failures) > 0 {
		joined := errors.Join(failures...)
		if mode != ModePermissive {
			return nil, fmt.Errorf("could not obtain signing keys for every configured issuer: %w", joined)
		}
		logger.Warn(
			"continuing without signing keys for one or more issuers because authentication is permissive; "+
				"their callers will be counted as would_reject, not rejected",
			"error", joined,
		)
	}
	return sources, nil
}

// Same failure contract as newKubernetesIssuerSource.
func newIssuerSource(
	ctx context.Context,
	iss Issuer,
	newSource jwksSourceFactory,
	logger *slog.Logger,
) (KeySource, error) {
	if newSource == nil {
		err := errors.New("no key source factory was supplied")
		return unavailableKeySource{issuer: iss.Issuer, cause: err},
			fmt.Errorf("issuer %q: build key source: %w", iss.Name, err)
	}

	src, err := newSource(iss)
	if err != nil {
		return unavailableKeySource{issuer: iss.Issuer, cause: err},
			fmt.Errorf("issuer %q: build key source: %w", iss.Name, err)
	}

	if err := src.refresh(ctx); err != nil {
		return src, fmt.Errorf("issuer %q: initial JWKS fetch: %w", iss.Name, err)
	}

	logger.Info("fetched issuer signing keys",
		"issuer", iss.Name,
		"issuer_url", iss.Issuer,
		"jwks_url", src.jwksURL,
		"keys", src.keyCount(),
	)
	return src, nil
}

// httpJWKSSourceFromConfig builds the factory for `httpJWKS` issuers. Each gets
// its own client, because each may name its own CA bundle and bearer token.
func jwksSourceFromConfig(logger *slog.Logger) jwksSourceFactory {
	return func(iss Issuer) (*jwksKeySource, error) {
		spec := iss.JWKS
		if spec == nil {
			return nil, errors.New("issuer has no jwks block")
		}

		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("http.DefaultTransport is not an *http.Transport")
		}
		cloned := transport.Clone()

		if path := strings.TrimSpace(spec.CABundlePath); path != "" {
			pem, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read caBundlePath %q: %w", path, err)
			}
			pool := x509.NewCertPool()
			// AppendCertsFromPEM only reports "nothing was added", so without this
			// a non-PEM bundle leaves an empty pool and every fetch fails with an
			// unknown-authority error naming nothing.
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("caBundlePath %q contains no PEM certificates", path)
			}
			cloned.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}

		var rt http.RoundTripper = cloned
		if path := strings.TrimSpace(spec.BearerTokenPath); path != "" {
			// Read per request: the file is usually a projected service account
			// volume, which the kubelet rotates well inside the controller's lifetime.
			rt = &fileBearerTransport{base: cloned, path: path}
		}

		return newJWKSKeySource(iss.Issuer, spec.URI, &http.Client{
			Transport: rt,
			Timeout:   jwksFetchTimeout,
		}, logger)
	}
}

// fileBearerTransport sets Authorization from a file, re-read on every request.
type fileBearerTransport struct {
	base http.RoundTripper
	path string
}

func (t *fileBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("read bearerTokenPath %q: %w", t.path, err)
	}
	// RoundTrip must not modify the request it is given.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	return t.base.RoundTrip(cloned)
}

// jwksKeySource serves one issuer's keys from a JWKS document over HTTP. The URL
// and the client are arguments, so the cluster's own JWKS and a third-party
// endpoint share one cache, throttle and staleness window.
type jwksKeySource struct {
	issuer  string
	jwksURL string
	client  *http.Client
	logger  *slog.Logger

	// refreshing collapses concurrent refreshes onto one fetch and makes the rest
	// wait for it. See maybeRefresh.
	refreshing singleflight.Group

	// now is injected so the throttle and staleness windows are testable.
	now func() time.Time

	mu sync.Mutex
	// keys is the last successfully fetched key set, indexed by "kid".
	keys map[string]crypto.PublicKey
	// haveKeys separates "never fetched" from a fetched empty result.
	haveKeys bool
	// lastSuccess drives the staleness window, lastAttempt the throttle. Separate
	// so a run of failures does not extend the window.
	lastSuccess time.Time
	lastAttempt time.Time
}

func newJWKSKeySource(issuer, jwksURL string, client *http.Client, logger *slog.Logger) (*jwksKeySource, error) {
	if client == nil {
		return nil, errors.New("JWKS key source requires an HTTP client")
	}
	parsed, err := url.Parse(jwksURL)
	if err != nil {
		return nil, fmt.Errorf("parse JWKS URL %q: %w", jwksURL, err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("JWKS URL %q has no host", jwksURL)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &jwksKeySource{
		issuer:  issuer,
		jwksURL: parsed.String(),
		client:  client,
		logger:  logger,
		now:     time.Now,
	}, nil
}

// KeyFor returns the key for kid, refreshing at most once per throttle window
// when the cache cannot answer.
func (s *jwksKeySource) KeyFor(ctx context.Context, kid string) (crypto.PublicKey, error) {
	if key, found, fresh := s.lookup(kid); found && fresh {
		return key, nil
	}

	// An unknown kid, or a cache that is empty or past its window. Both are worth
	// one out-of-band fetch.
	refreshErr := s.maybeRefresh(ctx)

	key, found, fresh := s.lookup(kid)
	switch {
	case !fresh:
		return nil, s.staleError(refreshErr)
	case !found:
		// Elided because "kid" is the caller's.
		return nil, fmt.Errorf("issuer %q: %w: kid %q is not in the key set at %s",
			s.issuer, ErrKeyNotFound, elideForLog(kid), s.jwksURL)
	default:
		return key, nil
	}
}

// lookup returns the key, whether kid was present, and whether the set is still
// authoritative.
func (s *jwksKeySource) lookup(kid string) (crypto.PublicKey, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.haveKeys || s.now().Sub(s.lastSuccess) > jwksMaxStaleness {
		return nil, false, false
	}
	key, found := s.keys[kid]
	return key, found, true
}

// refresh ignores the throttle. Used at construction.
func (s *jwksKeySource) refresh(ctx context.Context) error {
	s.mu.Lock()
	s.lastAttempt = s.now()
	s.mu.Unlock()
	return s.fetchAndStore(ctx)
}

// maybeRefresh fetches at most once per jwksRefreshThrottle. A throttled call is
// not an error; the cache still has an answer.
//
//nolint:nilnil // singleflight requires (any, error); the value is unused
func (s *jwksKeySource) maybeRefresh(ctx context.Context) error {
	// singleflight rather than a bare throttle: a caller arriving while a fetch is
	// running must wait for it, or during a rotation every goroutine but one
	// re-reads the cache it just missed in and reports bad_signature for the
	// length of one fetch. The throttle is checked inside the shared call, so a
	// caller arriving after a fetch completes returns immediately.
	_, err, _ := s.refreshing.Do("refresh", func() (any, error) {
		s.mu.Lock()
		now := s.now()
		if !s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < jwksRefreshThrottle {
			s.mu.Unlock()
			return nil, nil
		}
		s.lastAttempt = now
		s.mu.Unlock()

		// Detached from the caller's cancellation, not from its deadline: the
		// shared fetch outlives whichever request happened to start it, so one
		// client disconnecting mid-refresh does not fail every other caller
		// waiting on the same fetch. fetch still bounds itself with
		// jwksFetchTimeout.
		return nil, s.fetchAndStore(context.WithoutCancel(ctx))
	})
	return err
}

func (s *jwksKeySource) fetchAndStore(ctx context.Context) error {
	keys, err := s.fetch(ctx)
	if err != nil {
		// The first fetch comes through here too, when nothing is cached yet.
		// Only that case stops startup when enforcing.
		s.mu.Lock()
		cached := s.haveKeys
		s.mu.Unlock()
		if cached {
			s.logger.Warn("JWKS fetch failed; serving the cached key set until it goes stale",
				"issuer_url", s.issuer, "jwks_url", s.jwksURL, "error", err)
		} else {
			s.logger.Warn("JWKS fetch failed and no key set is cached yet",
				"issuer_url", s.issuer, "jwks_url", s.jwksURL, "error", err)
		}
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = keys
	s.haveKeys = true
	s.lastSuccess = s.now()
	return nil
}

// fetch retrieves and indexes the key set. s.jwksURL is fixed at construction and
// never derived from a request.
//
//nolint:gosec // G704: the URL is the apiserver's, fixed at construction.
func (s *jwksKeySource) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.jwksURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request for %s: %w", s.jwksURL, err)
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS from %s: %w", s.jwksURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS from %s: %w", s.jwksURL, err)
	}
	if len(body) > jwksMaxBytes {
		return nil, fmt.Errorf("JWKS from %s is larger than %d bytes", s.jwksURL, jwksMaxBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(s.jwksURL, resp.StatusCode)
	}
	return parseJWKS(body, s.issuer, s.jwksURL, s.logger)
}

// statusError names the likeliest cause of the two codes an operator will hit.
func statusError(jwksURL string, status int) error {
	switch status {
	case http.StatusForbidden, http.StatusUnauthorized:
		return fmt.Errorf(
			"JWKS fetch from %s returned %d: the endpoint refused this service's credential. "+
				"If this is the cluster's own JWKS, its ServiceAccount needs the "+
				"system:service-account-issuer-discovery ClusterRole", jwksURL, status,
		)
	default:
		return fmt.Errorf("JWKS fetch from %s returned unexpected status %d", jwksURL, status)
	}
}

// parseJWKS indexes a JWKS document by "kid", decoding one entry at a time so an
// unsupported key type is skipped rather than failing the document. Real key sets
// carry duplicate and empty "kid" values; first entry wins.
func parseJWKS(raw []byte, issuer, jwksURL string, logger *slog.Logger) (map[string]crypto.PublicKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse JWKS from %s: %w", jwksURL, err)
	}
	if len(doc.Keys) == 0 {
		return nil, fmt.Errorf("JWKS from %s contains no keys", jwksURL)
	}

	keys := make(map[string]crypto.PublicKey, len(doc.Keys))
	for i, rawKey := range doc.Keys {
		var jwk jose.JSONWebKey
		if err := jwk.UnmarshalJSON(rawKey); err != nil {
			logger.Warn("skipping unparseable JWKS entry",
				"issuer_url", issuer, "jwks_url", jwksURL, "index", i, "error", err)
			continue
		}
		pub, ok := publicKeyOf(jwk.Key)
		if !ok {
			// Every configured algorithm is asymmetric; see validateAlgorithms.
			logger.Warn("skipping JWKS entry that is not an asymmetric public key",
				"issuer_url", issuer, "jwks_url", jwksURL, "index", i, "kid", jwk.KeyID)
			continue
		}
		if _, dup := keys[jwk.KeyID]; dup {
			logger.Debug("ignoring duplicate JWKS key ID",
				"issuer_url", issuer, "jwks_url", jwksURL, "index", i, "kid", jwk.KeyID)
			continue
		}
		keys[jwk.KeyID] = pub
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS from %s contains no usable public keys (%d entries)", jwksURL, len(doc.Keys))
	}
	return keys, nil
}

func publicKeyOf(key any) (crypto.PublicKey, bool) {
	switch typed := key.(type) {
	case *rsa.PublicKey:
		return typed, true
	case *ecdsa.PublicKey:
		return typed, true
	case ed25519.PublicKey:
		return typed, true
	default:
		return nil, false
	}
}

// staleError says which flavor of "no authoritative keys" this is; all wrap
// ErrKeysStale.
func (s *jwksKeySource) staleError(refreshErr error) error {
	s.mu.Lock()
	have, last := s.haveKeys, s.lastSuccess
	s.mu.Unlock()

	detail := fmt.Sprintf("no JWKS has ever been fetched from %s", s.jwksURL)
	if have {
		detail = fmt.Sprintf("last successful JWKS fetch from %s was %s ago, past the %s window",
			s.jwksURL, s.now().Sub(last).Truncate(time.Second), jwksMaxStaleness)
	}
	if refreshErr != nil {
		return fmt.Errorf("issuer %q: %w: %s: %w", s.issuer, ErrKeysStale, detail, refreshErr)
	}
	return fmt.Errorf("issuer %q: %w: %s", s.issuer, ErrKeysStale, detail)
}

func (s *jwksKeySource) keyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// unavailableKeySource stands in for a source that could not be constructed. It
// reports ErrKeysStale, so the request counts as jwks_stale rather than blaming
// the token.
type unavailableKeySource struct {
	issuer string
	cause  error
}

func (s unavailableKeySource) KeyFor(_ context.Context, _ string) (crypto.PublicKey, error) {
	return nil, fmt.Errorf("issuer %q: %w: no key source could be built: %w", s.issuer, ErrKeysStale, s.cause)
}
