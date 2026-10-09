// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

// These tests drive a bare handler, not a server: NewServer needs a database and
// a Forge provider, and the rules under test depend on neither.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
)

// modeDisabled is deliberately not an auth.Mode: disabled is the absence of an
// Authenticator, not a setting on one.
const modeDisabled auth.Mode = "disabled"

// probePath is an authenticated route, the one the acceptance script calls.
const probePath = "/api/clusters"

// authCall is one recorded increment.
type authCall struct {
	Outcome auth.Outcome
	Reason  auth.FailureReason
	Issuer  string
}

// callRecorder stands in for auth.Recorder; the real counter is a process-global.
type callRecorder struct {
	mu    sync.Mutex
	calls []authCall
}

func (c *callRecorder) Record(outcome auth.Outcome, reason auth.FailureReason, issuer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, authCall{Outcome: outcome, Reason: reason, Issuer: issuer})
}

func (c *callRecorder) Calls() []authCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]authCall(nil), c.calls...)
}

type spyResult struct {
	called       bool
	principal    *auth.Principal
	hadPrincipal bool
}

// spyHandler reports whether it ran and what principal it was handed.
type spyHandler struct {
	mu     sync.Mutex
	result spyResult
}

func (s *spyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.result.called = true
	s.result.principal, s.result.hadPrincipal = auth.PrincipalFromContext(r.Context())
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("success")); err != nil {
		panic(err)
	}
}

func (s *spyHandler) saw() spyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// harness is a chain in one of the three modes, plus what it recorded.
type harness struct {
	handler  http.Handler
	next     *spyHandler
	recorder *callRecorder
	fixture  fixture
}

// newHarness builds the chain the way internal/server/server.go does: disabled
// means the middleware is absent, not a permissive Authenticator.
func newHarness(t *testing.T, mode auth.Mode) harness {
	t.Helper()

	f := newFixture(t)
	next := &spyHandler{}
	recorder := &callRecorder{}
	logger := slog.New(slog.DiscardHandler)

	var authn *auth.Authenticator
	switch mode {
	case modeDisabled:
		authn = nil
	case auth.ModePermissive, auth.ModeEnforcing:
		var err error
		authn, err = auth.NewAuthenticator(f.verifier, mode, logger, recorder.Record)
		require.NoError(t, err)
	default:
		t.Fatalf("unknown mode %q", mode)
	}

	var handler http.Handler = next
	if authn != nil {
		handler = authn.Wrap(next)
	}

	return harness{handler: handler, next: next, recorder: recorder, fixture: f}
}

func (h harness) do(t *testing.T, target, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

// The acceptance table for the 401/403 split.
func TestMiddlewareEnforcing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      func(*testing.T, fixture) string
		wantStatus int
		wantReason auth.FailureReason
		// wantIssuer is "unknown" only where the controller matched no configured issuer.
		wantIssuer string
	}{
		{
			name:       "no token",
			token:      nil,
			wantStatus: http.StatusUnauthorized,
			wantReason: auth.ReasonNoToken,
			wantIssuer: "unknown",
		},
		{
			name:       "admitted service account with the deployment audience",
			token:      admittedToken,
			wantStatus: http.StatusOK,
			wantReason: "",
			wantIssuer: k8sName,
		},
		{
			name:       "right service account, wrong audience",
			token:      wrongAudienceToken,
			wantStatus: http.StatusUnauthorized,
			wantReason: auth.ReasonAudienceMismatch,
			wantIssuer: k8sName,
		},
		{
			// The one failure that is not 401. The signature verified, so the
			// issuer label is known here and must not be lost.
			name:       "service account outside the admitted namespace",
			token:      unadmittedToken,
			wantStatus: http.StatusForbidden,
			wantReason: auth.ReasonNotAdmitted,
			wantIssuer: k8sName,
		},
		{
			// A genuine "unknown": the registry lookup never succeeded. Expiry and
			// a forged signature take the same 401 path with a known issuer, and
			// the reasons themselves are TestVerifyClaims'.
			name:       "an issuer nobody configured",
			token:      unknownIssuerToken,
			wantStatus: http.StatusUnauthorized,
			wantReason: auth.ReasonUnknownIssuer,
			wantIssuer: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, auth.ModeEnforcing)
			token := ""
			if tt.token != nil {
				token = tt.token(t, h.fixture)
			}

			rr := h.do(t, probePath, token)
			require.Equal(t, tt.wantStatus, rr.Code)

			called := h.next.saw().called
			calls := h.recorder.Calls()
			require.Len(t, calls, 1, "exactly one decision is counted per request")

			if tt.wantStatus == http.StatusOK {
				assert.True(t, called, "the wrapped handler runs on success")
				assert.True(t, h.next.saw().hadPrincipal,
					"a verified request carries its principal to the handler")
				assert.Equal(t, authCall{
					Outcome: auth.OutcomeAuthenticated,
					Reason:  "",
					Issuer:  tt.wantIssuer,
				}, calls[0])
				return
			}

			assert.False(t, called, "an enforcing rejection never reaches the wrapped handler")
			assert.Equal(t, authCall{
				Outcome: auth.OutcomeRejected,
				Reason:  tt.wantReason,
				Issuer:  tt.wantIssuer,
			}, calls[0])
		})
	}
}

// Permissive suppresses both the 401 and the 403. Suppressing only the 401 would
// 403 every caller outside the allow-list, so the unadmitted row is the one that
// matters.
func TestMiddlewarePermissive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		token       func(*testing.T, fixture) string
		wantOutcome auth.Outcome
		wantReason  auth.FailureReason
		wantIssuer  string
	}{
		{
			name:        "admitted service account",
			token:       admittedToken,
			wantOutcome: auth.OutcomeAuthenticated,
			wantReason:  "",
			wantIssuer:  k8sName,
		},
		{
			// The permissive branch has no reason- or status-dependent conditional,
			// so every failure runs the same block; one stands for all. Which reason
			// each token produces is TestMiddlewareEnforcing's.
			name:        "unadmitted namespace",
			token:       unadmittedToken,
			wantOutcome: auth.OutcomeWouldReject,
			wantReason:  auth.ReasonNotAdmitted,
			wantIssuer:  k8sName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, auth.ModePermissive)
			token := ""
			if tt.token != nil {
				token = tt.token(t, h.fixture)
			}

			rr := h.do(t, probePath, token)

			assert.Equal(t, http.StatusOK, rr.Code, "permissive never rejects")
			assert.Equal(t, "success", rr.Body.String())
			assert.Empty(t, rr.Header().Get("WWW-Authenticate"))

			saw := h.next.saw()
			assert.True(t, saw.called, "permissive always calls the next handler")

			calls := h.recorder.Calls()
			require.Len(t, calls, 1)
			assert.Equal(t, tt.wantOutcome, calls[0].Outcome)
			assert.Equal(t, tt.wantReason, calls[0].Reason)
			assert.Equal(t, tt.wantIssuer, calls[0].Issuer)

			if tt.wantOutcome == auth.OutcomeAuthenticated {
				require.True(t, saw.hadPrincipal, "a token that verifies still produces a principal")
				assert.Equal(t, admittedSubject, saw.principal.Subject)
				return
			}
			assert.False(t, saw.hadPrincipal, "a request that would have been rejected carries no principal")
		})
	}
}

// Prometheus keeps one series per label combination forever, so a caller-supplied
// issuer label would let anyone reaching the API mint series at will.
func TestMiddlewareIssuerLabelIsNotCallerSupplied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, auth.ModeEnforcing)

	// Every unregistered issuer misses the same lookup and collapses to the same
	// label, so two distinct ones prove the cardinality bound a hundred would.
	const attempts = 2
	for i := range attempts {
		spec := validKubernetesToken(h.fixture, time.Now())
		spec.Issuer = fmt.Sprintf("https://attacker.test/%d", i)
		rr := h.do(t, probePath, MintToken(t, spec))
		require.Equal(t, http.StatusUnauthorized, rr.Code)
	}

	values := map[string]int{}
	for _, call := range h.recorder.Calls() {
		values[call.Issuer]++
		assert.Equal(t, auth.ReasonUnknownIssuer, call.Reason)
	}
	assert.Equal(t, map[string]int{"unknown": attempts}, values,
		"attacker-chosen issuers are one series, not one apiece")
}

// Across every outcome the label takes the two configured names and "unknown",
// and nothing else.
func TestMiddlewareIssuerLabelSpansOnlyConfiguredNames(t *testing.T) {
	t.Parallel()

	h := newHarness(t, auth.ModePermissive)
	now := time.Now()

	spireWrongAudience := validSPIREToken(h.fixture, now)
	spireWrongAudience.Audience = []any{deploymentAudience}

	for _, token := range []string{
		"",
		"not-a-jwt",
		admittedToken(t, h.fixture),
		wrongAudienceToken(t, h.fixture),
		unadmittedToken(t, h.fixture),
		expiredToken(t, h.fixture),
		forgedToken(t, h.fixture),
		unknownIssuerToken(t, h.fixture),
		MintToken(t, validSPIREToken(h.fixture, now)),
		MintToken(t, spireWrongAudience),
	} {
		rr := h.do(t, probePath, token)
		require.Equal(t, http.StatusOK, rr.Code)
	}

	seen := map[string]bool{}
	for _, call := range h.recorder.Calls() {
		seen[call.Issuer] = true
	}
	assert.Equal(t, map[string]bool{k8sName: true, spireName: true, "unknown": true}, seen)
}

// Every path here is served with no credential, in enforcing mode, and counts
// nothing.
func TestMiddlewareAnonymousSurface(t *testing.T) {
	t.Parallel()

	paths := []string{
		"/openapi.yaml",
		"/api/version",

		// The mux 301s the unslashed form downstream of this middleware. Both
		// spellings normalize to this one, so it stands for the pair.
		"/api/docs",

		// The Swagger UI's own embedded assets, served from below its base path.
		// The whole subtree is exempt, whatever the extension.
		"/api/docs/swagger-ui.css",

		"/docs",
		"/healthz",
		"/readyz",

		// Cleaned onto the entries above, so a doubled slash is not a 401.
		"//healthz",
		"/api/./version",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, auth.ModeEnforcing)
			rr := h.do(t, path, "")

			assert.Equal(t, http.StatusOK, rr.Code, "%s must be anonymous", path)
			saw := h.next.saw()
			assert.True(t, saw.called)
			assert.False(t, saw.hadPrincipal, "an anonymous route is not authenticated, so it has no principal")
			assert.Empty(t, h.recorder.Calls(), "an anonymous route is not a decision and is not counted")
		})
	}
}

// Deny by default. This binary serves nothing outside the API and the documented
// surface, so every path below authenticates first and 404s second. That order
// keeps an unrouted path from reporting whether it exists.
func TestMiddlewareCatchAllDoesNotLeak(t *testing.T) {
	t.Parallel()

	paths := []string{
		// A real operation. Registered or not, every "/api/" path exits the same
		// way, so one stands for the unregistered ones too.
		"/api/clusters",
		"/API/CLUSTERS",

		// No UI is served here, so its index and its assets are not exempt. They
		// authenticate and then 404.
		"/",
		"/index.html",
		"/api.js",
		"/vendor/js-yaml.min.js",

		// Near misses on the exempt literals, which prefix matching would admit.
		"/api/versions",
		"/openapi.yaml.bak",
		"/healthzz",
		"/api/docsclusters",

		// A dot-segment, cleaned before matching. Cleans to "/api/clusters", but
		// would be exempt via the swagger prefix if it were not cleaned.
		"/api/docs/../clusters",

		// path.Clean does not decode, so this stays one opaque segment. Matching on
		// the decoded path alone would fold it onto "/api/version".
		"/api/docs%2f..%2fversion",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, auth.ModeEnforcing)
			rr := h.do(t, path, "")

			assert.Equal(t, http.StatusUnauthorized, rr.Code, "%s must require authentication", path)
			assert.False(t, h.next.saw().called)
		})
	}
}

// Anything that is not a bearer token is no_token: a 401, never a 500.
func TestMiddlewareBearerHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		header     func(token string) string
		wantStatus int
		wantReason auth.FailureReason
	}{
		{
			name:       "Bearer",
			header:     func(token string) string { return "Bearer " + token },
			wantStatus: http.StatusOK,
		},
		{
			// RFC 7235 makes the scheme case-insensitive, and clients disagree.
			name:       "lowercase scheme",
			header:     func(token string) string { return "bearer " + token },
			wantStatus: http.StatusOK,
		},
		{
			name:       "another scheme",
			header:     func(token string) string { return "Basic " + token },
			wantStatus: http.StatusUnauthorized,
			wantReason: auth.ReasonNoToken,
		},
		{
			// One field rather than two: the same length check catches a bare
			// "Bearer" and an empty header.
			name:       "no scheme",
			header:     func(token string) string { return token },
			wantStatus: http.StatusUnauthorized,
			wantReason: auth.ReasonNoToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, auth.ModeEnforcing)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, probePath, http.NoBody)
			if header := tt.header(admittedToken(t, h.fixture)); header != "" {
				req.Header.Set("Authorization", header)
			}
			rr := httptest.NewRecorder()
			h.handler.ServeHTTP(rr, req)

			assert.Equal(t, tt.wantStatus, rr.Code)
			if tt.wantStatus != http.StatusOK {
				calls := h.recorder.Calls()
				require.Len(t, calls, 1)
				assert.Equal(t, tt.wantReason, calls[0].Reason)
			}
		})
	}
}

// Rejections look like every other error this API returns, with a body free of
// anything the caller did not already know.
func TestMiddlewareErrorResponseShape(t *testing.T) {
	t.Parallel()

	t.Run("401", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, auth.ModeEnforcing)
		rr := h.do(t, probePath, "")

		require.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
		// RFC 7235 requires a challenge on a 401.
		assert.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		require.Contains(t, body, "message")
		message, ok := body["message"].(string)
		require.True(t, ok)
		assert.Contains(t, message, "Authentication required")
	})

	t.Run("403", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, auth.ModeEnforcing)
		rr := h.do(t, probePath, unadmittedToken(t, h.fixture))

		require.Equal(t, http.StatusForbidden, rr.Code)
		assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
		// No challenge: a better token will not help.
		assert.Empty(t, rr.Header().Get("WWW-Authenticate"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		message, ok := body["message"].(string)
		require.True(t, ok)
		assert.Contains(t, message, "Not admitted")
	})

	t.Run("no token or claim ever reaches the body", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, auth.ModeEnforcing)
		token := wrongAudienceToken(t, h.fixture)
		rr := h.do(t, probePath, token)

		require.Equal(t, http.StatusUnauthorized, rr.Code)
		body := rr.Body.String()
		assert.NotContains(t, body, token)
		assert.NotContains(t, body, "nke.wrong.invalid")
		assert.NotContains(t, body, k8sIssuer)
		// The reason describes the caller's own credential, which is what makes a
		// 401 debuggable.
		assert.Contains(t, body, string(auth.ReasonAudienceMismatch))
	})
}

// The two nil arguments a caller may pass must not panic on the first request.
func TestNewAuthenticatorTolerates(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	authn, err := auth.NewAuthenticator(f.verifier, auth.ModeEnforcing, nil, nil)
	require.NoError(t, err)

	next := &spyHandler{}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, probePath, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+MintToken(t, validKubernetesToken(f, time.Now())))

	authn.Wrap(next).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

// The one argument that is not tolerated. Disabling authentication means building
// no Authenticator, never one with nothing behind it.
func TestNewAuthenticatorRejectsNilVerifier(t *testing.T) {
	t.Parallel()

	authn, err := auth.NewAuthenticator(nil, auth.ModeEnforcing, nil, nil)
	require.Error(t, err)
	assert.Nil(t, authn)
}

// The same bound on the far side of a successful check. Wrap logs the path from
// its own Debug statement, which TestMiddlewareRejectionLogIsBounded never
// reaches because it sends no token: admission is not a license to write an
// unbounded path into the log on every request.
func TestMiddlewareAuthenticatedLogIsBounded(t *testing.T) {
	t.Parallel()

	const maxRecordBytes = 2048
	huge := strings.Repeat("A", hugeBytes)

	var log bytes.Buffer
	f := newFixture(t)
	authn, err := auth.NewAuthenticator(f.verifier, auth.ModeEnforcing,
		slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})), nil)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, probePath+"/"+huge, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+MintToken(t, validKubernetesToken(f, time.Now())))

	next := &spyHandler{}
	authn.Wrap(next).ServeHTTP(httptest.NewRecorder(), req)

	require.True(t, next.saw().called, "the request must still reach the handler")
	require.NotZero(t, log.Len(), "the authentication must still be logged")
	assert.Less(t, log.Len(), maxRecordBytes,
		"one authenticated request wrote %d bytes of log", log.Len())
	assert.NotContains(t, log.String(), huge)
}

// Bounds the bytes an unauthenticated request can write to the node's disk:
// handleFailure logs the underlying error, and "iss", "kid" and the path all come
// from the caller.
func TestMiddlewareRejectionLogIsBounded(t *testing.T) {
	t.Parallel()

	// One whole JSON-encoded rejection record, two orders of magnitude below the
	// input.
	const maxRecordBytes = 2048
	huge := strings.Repeat("A", hugeBytes)

	tests := []struct {
		name   string
		target string
		token  func(t *testing.T, f fixture) string
	}{
		{
			// The claim-side amplifiers ("iss", "kid") are elided inside verify.go
			// and bounded by TestErrorMessagesAreBoundedForUnauthenticatedCallers.
			// The path is this middleware's own, and it is logged from two separate
			// statements, so it runs in both modes.
			name:   "an oversized request path with no token",
			target: probePath + "/" + huge,
			token:  func(*testing.T, fixture) string { return "" },
		},
	}

	for _, tt := range tests {
		for _, mode := range []auth.Mode{auth.ModeEnforcing, auth.ModePermissive} {
			t.Run(tt.name+", "+string(mode), func(t *testing.T) {
				t.Parallel()

				var log bytes.Buffer
				f := newFixture(t)
				authn, err := auth.NewAuthenticator(f.verifier, mode,
					slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})), nil)
				require.NoError(t, err)

				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.target, http.NoBody)
				if token := tt.token(t, f); token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				authn.Wrap(&spyHandler{}).ServeHTTP(httptest.NewRecorder(), req)

				require.NotZero(t, log.Len(), "the rejection must still be logged")
				assert.Less(t, log.Len(), maxRecordBytes,
					"one unauthenticated request wrote %d bytes of log", log.Len())
				assert.NotContains(t, log.String(), huge)
			})
		}
	}
}
