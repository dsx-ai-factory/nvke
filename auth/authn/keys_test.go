// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwksPath is where these tests serve the document. Production reads the whole
// URL from configuration; nothing in the package hardcodes a path any more.
const jwksPath = "/openid/v1/jwks"

var (
	_ KeySource = (*jwksKeySource)(nil)
	_ KeySource = unavailableKeySource{}
)

const (
	testIssuer = "https://kubernetes.default.svc.cluster.local"
	// testIssuerName is the operator-chosen name the sources map is keyed by.
	testIssuerName = "local-kubernetes"
	algES256       = "ES256"
	algRS256       = "RS256"
)

// fakeClock makes the refresh throttle and the staleness window testable without
// sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// jwksServer stands in for the apiserver's /openid/v1/jwks. It counts hits so the
// throttle can be asserted on, and its body and status can be swapped mid-test.
type jwksServer struct {
	mu     sync.Mutex
	body   []byte
	status int
	hits   int
	srv    *httptest.Server
}

func newJWKSServer(t *testing.T, body []byte) *jwksServer {
	t.Helper()
	j := &jwksServer{body: body, status: http.StatusOK}
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		body, status := j.body, j.status
		j.hits++
		j.mu.Unlock()

		assert.Equal(t, jwksPath, r.URL.Path)
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func (j *jwksServer) Hits() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.hits
}

func (j *jwksServer) Serve(body []byte, status int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.body, j.status = body, status
}

func jwksBody(t *testing.T, keys ...jose.JSONWebKey) []byte {
	t.Helper()
	body, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	require.NoError(t, err)
	return body
}

// ecKey generates a fresh EC key every time. Not cached: the duplicate-kid test
// needs two distinct keys under the same kid.
func ecKey(t *testing.T) (jose.JSONWebKey, crypto.PublicKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return jose.JSONWebKey{Key: priv.Public(), KeyID: "kid-a", Algorithm: algES256, Use: "sig"}, priv.Public()
}

// rsaKeys caches generated RSA keys by name; generation is the slowest thing in
// this package. Distinct names are distinct keys.
var (
	rsaKeysMu sync.Mutex
	rsaKeys   = map[string]*rsa.PrivateKey{}
)

func testRSAKey(t *testing.T, name string) *rsa.PrivateKey {
	t.Helper()
	rsaKeysMu.Lock()
	defer rsaKeysMu.Unlock()
	if key, ok := rsaKeys[name]; ok {
		return key
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate RSA key %q", name)
	rsaKeys[name] = key
	return key
}

// rsaKey wraps testRSAKey as a JWKS entry.
func rsaKey(t *testing.T, kid string) (jose.JSONWebKey, crypto.PublicKey) {
	t.Helper()
	pub := testRSAKey(t, "jwks/"+kid).Public()
	return jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: algRS256, Use: "sig"}, pub
}

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func kubernetesIssuerConfig() *Config {
	return &Config{
		Audiences: []string{"https://cwe.example.com"},
		Issuers: []Issuer{{
			Name:       testIssuerName,
			Issuer:     testIssuer,
			JWKS:       &JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
			Algorithms: []string{algRS256, algES256},
			Admit:      &Admit{ServiceAccounts: []ServiceAccountRule{{Namespace: "nke-clients"}}},
		}},
	}
}

// sourcesAt runs the real newKeySources path, substituting only the transport and
// the clock.
func sourcesAt(t *testing.T, srv *jwksServer, clk *fakeClock, mode Mode) (map[string]KeySource, error) {
	t.Helper()
	factory := func(iss Issuer) (*jwksKeySource, error) {
		src, err := newJWKSKeySource(iss.Issuer, srv.srv.URL+jwksPath, srv.srv.Client(), quietLogger())
		if err != nil {
			return nil, err
		}
		// Marks this as the cluster JWKS, which is what makes statusError print
		// the ClusterRole remedy.
		src.now = clk.Now
		return src, nil
	}
	return newKeySources(context.Background(), kubernetesIssuerConfig(), mode, quietLogger(),
		factory)
}

func TestKeySourcesFetchAtConstructionAndResolveKeys(t *testing.T) {
	t.Parallel()

	jwkA, pubA := rsaKey(t, "kid-a")
	srv := newJWKSServer(t, jwksBody(t, jwkA))
	clk := newFakeClock()

	sources, err := sourcesAt(t, srv, clk, ModeEnforcing)
	require.NoError(t, err)
	assert.Equal(t, 1, srv.Hits(), "the constructor fetches once, before the listener opens")

	// Keyed by the operator-chosen name, one entry per configured issuer.
	require.Len(t, sources, 1)
	require.Contains(t, sources, testIssuerName)

	got, err := sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.NoError(t, err)
	assert.Equal(t, pubA, got)
}

func TestRefreshThrottleIsOnePerIssuerPerWindow(t *testing.T) {
	t.Parallel()

	jwkA, _ := ecKey(t)
	srv := newJWKSServer(t, jwksBody(t, jwkA))
	clk := newFakeClock()

	sources, err := sourcesAt(t, srv, clk, ModeEnforcing)
	require.NoError(t, err)
	require.Equal(t, 1, srv.Hits())

	// Without the throttle each unknown kid would be a request to the apiserver.
	// Every iteration takes the same early return, so a handful spans the window
	// as well as fifty would.
	const misses = 4
	for range misses {
		clk.Advance(5 * time.Second)
		_, err := sources[testIssuerName].KeyFor(context.Background(), "unknown")
		require.ErrorIs(t, err, ErrKeyNotFound)
	}
	assert.Equal(t, 1, srv.Hits(), "unknown-kid requests inside one window must produce no extra fetch")

	clk.Advance(jwksRefreshThrottle)
	_, err = sources[testIssuerName].KeyFor(context.Background(), "unknown")
	require.ErrorIs(t, err, ErrKeyNotFound)
	assert.Equal(t, 2, srv.Hits())

	_, err = sources[testIssuerName].KeyFor(context.Background(), "unknown")
	require.ErrorIs(t, err, ErrKeyNotFound)
	assert.Equal(t, 2, srv.Hits(), "the second miss in the new window is throttled again")
}

func TestDuplicateAndEmptyKidAreTolerated(t *testing.T) {
	t.Parallel()

	// Shaped after a live JWKS that publishes its signing key twice, once with a
	// kid and once with an empty one.
	jwkNamed, pubNamed := ecKey(t)
	jwkEmpty := jose.JSONWebKey{Key: pubNamed, KeyID: "", Algorithm: algES256, Use: "sig"}
	// One duplicate is enough: the named and empty spellings take the same
	// already-indexed branch.
	jwkDupNamed, _ := ecKey(t)

	srv := newJWKSServer(t, jwksBody(t, jwkNamed, jwkEmpty, jwkDupNamed))
	clk := newFakeClock()

	sources, err := sourcesAt(t, srv, clk, ModeEnforcing)
	require.NoError(t, err, "a duplicate or empty kid is not a configuration error")

	got, err := sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.NoError(t, err)
	assert.Equal(t, pubNamed, got, "first entry wins, so the result does not depend on iteration order")

	gotEmpty, err := sources[testIssuerName].KeyFor(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, pubNamed, gotEmpty)
}

func TestUnusableJWKSEntriesAreSkippedNotFatal(t *testing.T) {
	t.Parallel()

	jwkA, pubA := ecKey(t)
	body := jwksBody(t, jwkA)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	keys, _ := doc["keys"].([]any)
	doc["keys"] = append(keys,
		map[string]any{"kty": "MYSTERY", "kid": "kid-x"},
		map[string]any{"kty": "oct", "kid": "kid-symmetric", "k": "AAAAAAAAAAAAAAAAAAAAAA"},
	)
	mixed, err := json.Marshal(doc)
	require.NoError(t, err)

	srv := newJWKSServer(t, mixed)
	sources, err := sourcesAt(t, srv, newFakeClock(), ModeEnforcing)
	require.NoError(t, err, "one unsupported entry must not cost us the whole key set")

	got, err := sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.NoError(t, err)
	assert.Equal(t, pubA, got)

	_, err = sources[testIssuerName].KeyFor(context.Background(), "kid-symmetric")
	require.ErrorIs(t, err, ErrKeyNotFound, "a symmetric key must never become a verification key")
}

func TestInitialFetchFailureIsFatalWhenEnforcingAndNotWhenPermissive(t *testing.T) {
	t.Parallel()

	t.Run("enforcing", func(t *testing.T) {
		t.Parallel()

		srv := newJWKSServer(t, nil)
		srv.Serve([]byte("nope"), http.StatusServiceUnavailable)

		sources, err := sourcesAt(t, srv, newFakeClock(), ModeEnforcing)
		require.Error(t, err, "enforcing controller must not open its listener without keys")
		assert.Nil(t, sources)
		assert.Contains(t, err.Error(), "local-kubernetes")
		assert.Contains(t, err.Error(), "503")
	})

	t.Run("permissive", func(t *testing.T) {
		t.Parallel()

		srv := newJWKSServer(t, nil)
		srv.Serve([]byte("nope"), http.StatusServiceUnavailable)

		sources, err := sourcesAt(t, srv, newFakeClock(), ModePermissive)
		require.NoError(t, err, "permissive controller rejects nobody, so a missing key set costs only a counter")
		require.NotNil(t, sources)

		_, err = sources[testIssuerName].KeyFor(context.Background(), "kid-a")
		require.ErrorIs(t, err, ErrKeysStale,
			"never having had keys is jwks_stale, so the metric blames the controller and not the caller's token")
		require.NotErrorIs(t, err, ErrKeyNotFound)
	})
}

func TestForbiddenFetchNamesTheMissingClusterRole(t *testing.T) {
	t.Parallel()

	srv := newJWKSServer(t, nil)
	srv.Serve([]byte(`{"kind":"Status","code":403}`), http.StatusForbidden)

	_, err := sourcesAt(t, srv, newFakeClock(), ModeEnforcing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system:service-account-issuer-discovery",
		"the likeliest cause of a 403 here should not need a log dive")
}

// The first fetch and a later refresh share fetchAndStore, but only the refresh
// has a cache to fall back on. The log has to say which one failed: the no-cache
// case is the one that stops an enforcing controller from starting.
func TestFetchFailureSaysWhetherAnythingIsCached(t *testing.T) {
	t.Parallel()

	var log bytes.Buffer
	srv := newJWKSServer(t, nil)
	srv.Serve([]byte("nope"), http.StatusServiceUnavailable)

	src, err := newJWKSKeySource(testIssuer, srv.srv.URL+jwksPath, srv.srv.Client(),
		slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	require.NoError(t, err)

	require.Error(t, src.refresh(context.Background()))
	assert.Contains(t, log.String(), "no key set is cached yet")
	assert.NotContains(t, log.String(), "serving the cached key set",
		"nothing has been fetched yet, so there is no cache to serve")

	// Succeed once, so there is something to fall back on.
	jwkA, _ := rsaKey(t, "kid-a")
	srv.Serve(jwksBody(t, jwkA), http.StatusOK)
	require.NoError(t, src.refresh(context.Background()))

	log.Reset()
	srv.Serve([]byte("nope"), http.StatusServiceUnavailable)
	require.Error(t, src.refresh(context.Background()))
	assert.Contains(t, log.String(), "serving the cached key set")
	assert.NotContains(t, log.String(), "no key set is cached yet")
}

func TestCachedKeysStayAuthoritativeUntilTheStalenessWindowPasses(t *testing.T) {
	t.Parallel()

	jwkA, pubA := ecKey(t)
	good := jwksBody(t, jwkA)
	srv := newJWKSServer(t, good)
	clk := newFakeClock()

	sources, err := sourcesAt(t, srv, clk, ModeEnforcing)
	require.NoError(t, err)
	require.Equal(t, 1, srv.Hits())

	// The issuer goes away after a successful start.
	srv.Serve([]byte("gateway timeout"), http.StatusGatewayTimeout)

	// A miss past the throttle window drives a refresh, which fails. The cached set
	// stays authoritative.
	clk.Advance(jwksRefreshThrottle + time.Second)
	_, err = sources[testIssuerName].KeyFor(context.Background(), "kid-b")
	require.ErrorIs(t, err, ErrKeyNotFound)
	require.NotErrorIs(t, err, ErrKeysStale)
	require.Equal(t, 2, srv.Hits())

	got, err := sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.NoError(t, err, "a failed refresh must not invalidate a good cached key")
	assert.Equal(t, pubA, got)

	// Just inside the window.
	clk.Advance(jwksMaxStaleness - time.Minute)
	got, err = sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.NoError(t, err)
	assert.Equal(t, pubA, got)

	// Past it the key set is no longer trusted, and says so rather than presenting
	// as a signature failure.
	clk.Advance(2 * time.Minute)
	_, err = sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.ErrorIs(t, err, ErrKeysStale)
	require.NotErrorIs(t, err, ErrKeyNotFound)
	assert.Contains(t, err.Error(), "past the 30m0s window")

	// It recovers on its own once the issuer comes back.
	srv.Serve(good, http.StatusOK)
	clk.Advance(jwksRefreshThrottle + time.Second)
	got, err = sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.NoError(t, err)
	assert.Equal(t, pubA, got)
}

// tlsJWKSServer is an HTTPS JWKS endpoint with a throwaway CA. Separate from
// jwksServer, which is plain HTTP and asserts the apiserver's path.
func tlsJWKSServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// caPEMFile writes srv's certificate out as the PEM bundle caBundlePath expects.
func caPEMFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	require.NotNil(t, cert, "httptest TLS server has no certificate")
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	return path
}

// Same server, no caBundlePath, so the client falls back to the system roots and
// the throwaway CA is not among them.
func TestHTTPJWKSRefusesAnUntrustedServer(t *testing.T) {
	t.Parallel()

	jwkA, _ := rsaKey(t, "kid-a")
	srv := tlsJWKSServer(t, jwksBody(t, jwkA))

	cfg := kubernetesIssuerConfig()
	cfg.Issuers[0].Name = "untrusted"
	cfg.Issuers[0].JWKS = &JWKSKeys{URI: srv.URL + "/keys"}

	_, err := newKeySources(context.Background(), cfg, ModeEnforcing, quietLogger(),
		jwksSourceFromConfig(quietLogger()))
	require.Error(t, err, "an issuer whose TLS chain does not verify must not start enforcing")
	assert.Contains(t, err.Error(), "initial JWKS fetch")
}

// The file being wrong rather than the chain. Both spellings produce an empty
// pool, which would otherwise fail every fetch with an error naming nothing.
func TestHTTPJWKSRejectsAnUnusableCABundle(t *testing.T) {
	t.Parallel()

	notPEM := filepath.Join(t.TempDir(), "not-a-bundle.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("this is not a certificate\n"), 0o600))

	for name, path := range map[string]string{
		"absent":  filepath.Join(t.TempDir(), "missing.pem"),
		"not PEM": notPEM,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := kubernetesIssuerConfig()
			cfg.Issuers[0].JWKS = &JWKSKeys{
				URI:          "https://idp.example.com/keys",
				CABundlePath: path,
			}

			_, err := newKeySources(context.Background(), cfg, ModeEnforcing, quietLogger(),
				jwksSourceFromConfig(quietLogger()))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "caBundlePath",
				"the error has to name the file so an operator can find it")
		})
	}
}

// The token file is usually a projected service account volume, which the kubelet
// rotates well inside the controller's lifetime, so it must be re-read per request.
func TestHTTPJWKSBearerTokenIsRereadPerRequest(t *testing.T) {
	t.Parallel()

	jwkA, _ := rsaKey(t, "kid-rotating")
	body := jwksBody(t, jwkA)

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("first-token\n"), 0o600))

	cfg := kubernetesIssuerConfig()
	cfg.Issuers[0].Name = "idp"
	cfg.Issuers[0].JWKS = &JWKSKeys{
		URI:             srv.URL + "/keys",
		CABundlePath:    caPEMFile(t, srv),
		BearerTokenPath: tokenPath,
	}

	sources, err := newKeySources(context.Background(), cfg, ModeEnforcing, quietLogger(),
		jwksSourceFromConfig(quietLogger()))
	require.NoError(t, err)

	// Rotate the file, then force a second fetch.
	require.NoError(t, os.WriteFile(tokenPath, []byte("second-token\n"), 0o600))
	src, ok := sources["idp"].(*jwksKeySource)
	require.True(t, ok)
	require.NoError(t, src.refresh(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2, "expected the priming fetch and the forced refresh")
	assert.Equal(t, "Bearer first-token", seen[0])
	assert.Equal(t, "Bearer second-token", seen[1],
		"the token file must be re-read per request, not captured at startup")
}

// An issuer with no jwks block is a configuration error, so it is fatal in
// permissive mode too: permissive changes what a rejection costs, not what a
// valid configuration is.
func TestMissingJWKSBlockIsRejectedInBothModes(t *testing.T) {
	t.Parallel()

	// The check returns before the mode is read at all, so permissive -- the mode
	// that does tolerate a failed fetch -- is the one worth stating.
	cfg := kubernetesIssuerConfig()
	cfg.Issuers[0].JWKS = nil

	_, err := newKeySources(context.Background(), cfg, ModePermissive, quietLogger(),
		jwksSourceFromConfig(quietLogger()))
	require.Error(t, err, "a configuration error is fatal in permissive mode too")
}

func TestSourceConstructionFailureStillYieldsAnEntryThatReportsStale(t *testing.T) {
	t.Parallel()

	broken := func(Issuer) (*jwksKeySource, error) {
		return newJWKSKeySource("iss", "://not-a-url", http.DefaultClient, quietLogger())
	}

	_, err := newKeySources(context.Background(), kubernetesIssuerConfig(), ModeEnforcing, quietLogger(),
		broken)
	require.Error(t, err)

	sources, err := newKeySources(context.Background(), kubernetesIssuerConfig(), ModePermissive, quietLogger(),
		broken)
	require.NoError(t, err)
	_, err = sources[testIssuerName].KeyFor(context.Background(), "kid-a")
	require.ErrorIs(t, err, ErrKeysStale)
	require.NotErrorIs(t, err, ErrKeyNotFound)
}

// Key rotation. Without singleflight a goroutine that misses while another is
// fetching would see the throttle, skip the refresh and return ErrKeyNotFound,
// rejecting valid tokens signed with the new key for the length of the fetch.
func TestConcurrentMissesWaitForTheInFlightRefresh(t *testing.T) {
	t.Parallel()

	jwkA, _ := rsaKey(t, "kid-a")
	jwkB, pubB := rsaKey(t, "kid-b")

	var (
		mu      sync.Mutex
		body    = jwksBody(t, jwkA)
		fetches int
	)
	release := make(chan struct{})
	inFetch := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n := fetches
		fetches++
		current := body
		mu.Unlock()

		// Block only the rotation fetch, so the priming one returns at once.
		if n == 1 {
			select {
			case inFetch <- struct{}{}:
			default:
			}
			<-release
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_, _ = w.Write(current)
	}))
	t.Cleanup(srv.Close)

	clk := newFakeClock()
	src, err := newJWKSKeySource(testIssuer, srv.URL+jwksPath, srv.Client(), quietLogger())
	require.NoError(t, err)
	src.now = clk.Now
	require.NoError(t, src.refresh(context.Background()))

	// Rotation: both keys published, throttle window passed.
	mu.Lock()
	body = jwksBody(t, jwkA, jwkB)
	mu.Unlock()
	clk.Advance(jwksRefreshThrottle + time.Second)

	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() {
			_, err := src.KeyFor(context.Background(), "kid-b")
			results <- err
		}()
	}

	// Let them pile up behind the one fetch before releasing it.
	<-inFetch
	time.Sleep(50 * time.Millisecond)
	close(release)

	for range callers {
		require.NoError(t, <-results,
			"a caller that missed during a rotation must wait for the in-flight fetch, not report the kid missing")
	}

	got, err := src.KeyFor(context.Background(), "kid-b")
	require.NoError(t, err)
	assert.Equal(t, pubB, got)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, fetches, "the throttle must still collapse the burst into one rotation fetch")
}

// TestOneCallerCancellingDoesNotFailTheOthers covers the singleflight caveat:
// the shared fetch runs on whichever caller happened to start it, so without
// detaching that context a client disconnecting mid-refresh would fail every
// other caller waiting on the same fetch.
func TestOneCallerCancellingDoesNotFailTheOthers(t *testing.T) {
	t.Parallel()

	jwkA, _ := rsaKey(t, "kid-a")
	jwkB, pubB := rsaKey(t, "kid-b")

	var mu sync.Mutex
	body := jwksBody(t, jwkA)
	release := make(chan struct{})
	inFetch := make(chan struct{}, 1)
	var fetches int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n := fetches
		fetches++
		current := body
		mu.Unlock()
		if n == 1 {
			select {
			case inFetch <- struct{}{}:
			default:
			}
			<-release
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_, _ = w.Write(current)
	}))
	t.Cleanup(srv.Close)

	clk := newFakeClock()
	src, err := newJWKSKeySource(testIssuer, srv.URL+jwksPath, srv.Client(), quietLogger())
	require.NoError(t, err)
	src.now = clk.Now
	require.NoError(t, src.refresh(context.Background()))

	mu.Lock()
	body = jwksBody(t, jwkA, jwkB)
	mu.Unlock()
	clk.Advance(jwksRefreshThrottle + time.Second)

	// The caller that starts the fetch, and cancels while it is in flight.
	cancelCtx, cancel := context.WithCancel(context.Background())
	starter := make(chan error, 1)
	go func() {
		_, err := src.KeyFor(cancelCtx, "kid-b")
		starter <- err
	}()

	<-inFetch

	// A second caller joins the same fetch, then the first one goes away.
	joiner := make(chan error, 1)
	go func() {
		_, err := src.KeyFor(context.Background(), "kid-b")
		joiner <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)

	require.NoError(t, <-joiner,
		"a caller that did not cancel must still get its key when another caller disconnects")
	<-starter

	got, err := src.KeyFor(context.Background(), "kid-b")
	require.NoError(t, err)
	assert.Equal(t, pubB, got)
}
