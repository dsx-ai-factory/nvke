// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeServerCABundle writes the test server's certificate as a PEM bundle and
// returns its path, so the client authenticates the endpoint the same way a
// node does.
func writeServerCABundle(t *testing.T, server *httptest.Server) string {
	t.Helper()

	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	require.NotNil(t, encoded)
	path := filepath.Join(t.TempDir(), "dsx-identity-ca.pem")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}

// writeUnrelatedCABundle writes a self-signed certificate that signs nothing in
// these tests. httptest reuses one certificate across all its TLS servers, so a
// second httptest server would not be a distinct trust root.
func writeUnrelatedCABundle(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "unrelated-ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func TestNewExchangeClient_RejectsUnusableConfiguration(t *testing.T) {
	t.Parallel()

	type parameters struct {
		endpoint string
		timeout  time.Duration
		wantErr  string
	}
	tests := map[string]parameters{
		"plaintext endpoint": {
			endpoint: "http://identity.example.test/v1/exchange",
			timeout:  time.Second,
			wantErr:  "absolute https url",
		},
		"relative endpoint": {
			endpoint: "/v1/exchange",
			timeout:  time.Second,
			wantErr:  "absolute https url",
		},
		"empty endpoint": {
			endpoint: "",
			timeout:  time.Second,
			wantErr:  "absolute https url",
		},
		"zero timeout": {
			endpoint: "https://identity.example.test/v1/exchange",
			timeout:  0,
			wantErr:  "timeout must be positive",
		},
		"negative timeout": {
			endpoint: "https://identity.example.test/v1/exchange",
			timeout:  -time.Second,
			wantErr:  "timeout must be positive",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, err := NewExchangeClient(test.endpoint, "", test.timeout)
			assert.Nil(t, client)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestNewExchangeClient_RejectsUnusableCABundle(t *testing.T) {
	t.Parallel()

	endpoint := "https://identity.example.test/v1/exchange"

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		client, err := NewExchangeClient(endpoint, filepath.Join(t.TempDir(), "absent.pem"), time.Second)
		assert.Nil(t, client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading dsx identity ca bundle")
	})

	t.Run("file with no certificates", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "empty.pem")
		require.NoError(t, os.WriteFile(path, []byte("not a certificate\n"), 0o600))

		client, err := NewExchangeClient(endpoint, path, time.Second)
		assert.Nil(t, client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "contains no certificates")
	})
}

func TestExchange_SendsRFC8693RequestAndReturnsToken(t *testing.T) {
	t.Parallel()

	var gotForm url.Values
	var gotContentType string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		gotContentType = r.Header.Get("Content-Type")
		require.NoError(t, r.ParseForm())
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"dsx-token","issued_token_type":"urn:ietf:params:oauth:token-type:jwt","token_type":"Bearer","expires_in":300}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewExchangeClient(server.URL+"/v1/exchange", writeServerCABundle(t, server), 5*time.Second)
	require.NoError(t, err)

	issuedAt := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return issuedAt }

	token, err := client.Exchange(context.Background(), "spire-jwt-svid")
	require.NoError(t, err)

	assert.Equal(t, "dsx-token", token.Value)
	assert.Equal(t, issuedAt.Add(300*time.Second), token.Expiry)
	assert.Equal(t, "application/x-www-form-urlencoded", gotContentType)
	assert.Equal(t, grantTypeTokenExchange, gotForm.Get("grant_type"))
	assert.Equal(t, "spire-jwt-svid", gotForm.Get("subject_token"))
	assert.Equal(t, tokenTypeJWT, gotForm.Get("subject_token_type"))
	assert.Equal(t, tokenTypeJWT, gotForm.Get("requested_token_type"))
	// The endpoint takes no audience parameter; it preserves the audience of
	// the subject token.
	assert.Empty(t, gotForm.Get("audience"))
}

func TestExchange_AcceptsAResponseThatOmitsTheIssuedTokenType(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"dsx-token","token_type":"Bearer","expires_in":300}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewExchangeClient(server.URL+"/v1/exchange", writeServerCABundle(t, server), 5*time.Second)
	require.NoError(t, err)

	token, err := client.Exchange(context.Background(), "spire-jwt-svid")
	require.NoError(t, err)
	assert.Equal(t, "dsx-token", token.Value)
}

func TestExchange_FailsClosedOnUnusableResponses(t *testing.T) {
	t.Parallel()

	type parameters struct {
		status  int
		body    string
		wantErr string
	}
	tests := map[string]parameters{
		"unauthorized": {
			status:  http.StatusUnauthorized,
			body:    `{"error":"invalid_grant"}`,
			wantErr: "returned http 401",
		},
		"server error": {
			status:  http.StatusInternalServerError,
			body:    "",
			wantErr: "returned http 500",
		},
		"malformed json": {
			status:  http.StatusOK,
			body:    "{",
			wantErr: "decoding dsx identity exchange response",
		},
		"no access token": {
			status:  http.StatusOK,
			body:    `{"expires_in":300}`,
			wantErr: "returned no access token",
		},
		"zero expiry": {
			status:  http.StatusOK,
			body:    `{"access_token":"dsx-token","expires_in":0}`,
			wantErr: "returned expires_in 0",
		},
		"negative expiry": {
			status:  http.StatusOK,
			body:    `{"access_token":"dsx-token","expires_in":-1}`,
			wantErr: "returned expires_in -1",
		},
		"issued token type is not a jwt": {
			status:  http.StatusOK,
			body:    `{"access_token":"dsx-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":300}`,
			wantErr: `issued token type "urn:ietf:params:oauth:token-type:access_token"`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			client, err := NewExchangeClient(server.URL+"/v1/exchange", writeServerCABundle(t, server), 5*time.Second)
			require.NoError(t, err)

			token, err := client.Exchange(context.Background(), "spire-jwt-svid")
			assert.Empty(t, token.Value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestExchange_DoesNotLeakTheSubjectTokenInErrors(t *testing.T) {
	t.Parallel()

	const subjectToken = "spire-jwt-svid-secret"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		w.WriteHeader(http.StatusBadRequest)
		// An endpoint that echoes the request must not turn into an agent log leak.
		_, _ = w.Write([]byte(`{"error":"invalid_grant","subject_token":"` + subjectToken + `"}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewExchangeClient(server.URL+"/v1/exchange", writeServerCABundle(t, server), 5*time.Second)
	require.NoError(t, err)

	_, err = client.Exchange(context.Background(), subjectToken)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), subjectToken)
}

func TestExchange_RejectsAnEndpointOutsideTheCABundle(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"dsx-token","expires_in":300}`))
	}))
	t.Cleanup(server.Close)

	// A bundle that does not include the server's certificate must fail the
	// call rather than silently fall back to the system trust store.
	client, err := NewExchangeClient(server.URL+"/v1/exchange", writeUnrelatedCABundle(t), 5*time.Second)
	require.NoError(t, err)

	_, err = client.Exchange(context.Background(), "spire-jwt-svid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "calling dsx identity exchange")
}

func TestExchange_RejectsAnEmptySubjectToken(t *testing.T) {
	t.Parallel()

	client, err := NewExchangeClient("https://identity.example.test/v1/exchange", "", time.Second)
	require.NoError(t, err)

	_, err = client.Exchange(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subject token must not be empty")
}
