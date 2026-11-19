// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testJWT(t *testing.T, payload string) string {
	t.Helper()
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"ES256"}`)) + "." + encode([]byte(payload)) + "." + encode([]byte("signature"))
}

func TestNewMetadataClient_ValidatesURL(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		url     string
		timeout time.Duration
		wantErr string
	}{
		"link-local http":   {url: "http://169.254.169.254", timeout: time.Second},
		"trailing slash":    {url: "http://169.254.169.254/", timeout: time.Second},
		"https":             {url: "https://metadata.example.test", timeout: time.Second},
		"relative":          {url: "169.254.169.254", timeout: time.Second, wantErr: "absolute http(s) url"},
		"unsupported":       {url: "unix:///run/metadata.sock", timeout: time.Second, wantErr: "absolute http(s) url"},
		"path":              {url: "http://169.254.169.254/latest", timeout: time.Second, wantErr: "no path"},
		"query":             {url: "http://169.254.169.254?aud=x", timeout: time.Second, wantErr: "no path"},
		"non-positive wait": {url: "http://169.254.169.254", wantErr: "timeout must be positive"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, err := NewMetadataClient(tc.url, tc.timeout)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, client)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, client)
		})
	}
}

func TestNewMetadataClient_BypassesEnvironmentProxies(t *testing.T) {
	t.Parallel()

	client, err := NewMetadataClient("http://169.254.169.254", time.Second)
	require.NoError(t, err)

	transport, ok := client.client.Transport.(*http.Transport)
	require.True(t, ok, "metadata client transport is %T", client.client.Transport)
	assert.Nil(t, transport.Proxy)
}

func TestMetadataClient_FetchSendsTheMetadataContract(t *testing.T) {
	t.Parallel()

	expiry := time.Date(2026, time.November, 17, 20, 0, 0, 0, time.UTC)
	token := testJWT(t, fmt.Sprintf(`{"sub":"spiffe://nke.example.test/vm/node","exp":%d}`, expiry.Unix()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, metadataIdentityPath, r.URL.Path)
		assert.Equal(t, ControllerAudience, r.URL.Query().Get("aud"))
		assert.Equal(t, "true", r.Header.Get("Metadata"))
		assert.Equal(t, "text/plain", r.Header.Get("Accept"))
		_, _ = fmt.Fprintln(w, token)
	}))
	t.Cleanup(server.Close)

	client, err := NewMetadataClient(server.URL, time.Second)
	require.NoError(t, err)

	got, err := client.Fetch(context.Background(), ControllerAudience)
	require.NoError(t, err)
	assert.Equal(t, token, got.Value)
	assert.True(t, expiry.Equal(got.Expiry), "expiry %s, want %s", got.Expiry, expiry)
}

func TestMetadataClient_FetchRejectsUnusableResponses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status  int
		body    string
		wantErr string
	}{
		"http error":      {status: http.StatusForbidden, body: "denied", wantErr: "returned http 403"},
		"empty body":      {status: http.StatusOK, body: "\n", wantErr: "empty dsx identity token"},
		"not a JWT":       {status: http.StatusOK, body: "opaque", wantErr: "parsing token"},
		"bad payload":     {status: http.StatusOK, body: "a.%%%.c", wantErr: "parsing token"},
		"missing exp":     {status: http.StatusOK, body: testJWT(t, `{"sub":"x"}`), wantErr: "no exp claim"},
		"non-numeric exp": {status: http.StatusOK, body: testJWT(t, `{"exp":"soon"}`), wantErr: "reading token exp claim"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)

			client, err := NewMetadataClient(server.URL, time.Second)
			require.NoError(t, err)

			_, err = client.Fetch(context.Background(), ControllerAudience)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestMetadataTokenSource_CachesUntilNearExpiry(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		exp := start.Add(time.Duration(n) * 5 * time.Minute).Unix()
		_, _ = fmt.Fprint(w, testJWT(t, fmt.Sprintf(`{"exp":%d,"n":%d}`, exp, n)))
	}))
	t.Cleanup(server.Close)

	client, err := NewMetadataClient(server.URL, time.Second)
	require.NoError(t, err)
	source, err := NewMetadataTokenSource(client, ControllerAudience)
	require.NoError(t, err)
	now := start
	source.now = func() time.Time { return now }

	first, err := source.Token(context.Background())
	require.NoError(t, err)

	now = start.Add(4 * time.Minute)
	cached, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first, cached)
	assert.Equal(t, int32(1), calls.Load())

	now = start.Add(5*time.Minute - DefaultRefreshBefore)
	refreshed, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, first, refreshed)
	assert.Equal(t, int32(2), calls.Load())
}

func TestNewMetadataTokenSource_RequiresItsCollaborators(t *testing.T) {
	t.Parallel()

	source, err := NewMetadataTokenSource(nil, ControllerAudience)
	assert.Nil(t, source)
	require.ErrorContains(t, err, "metadata service client")

	client, err := NewMetadataClient("http://169.254.169.254", time.Second)
	require.NoError(t, err)
	source, err = NewMetadataTokenSource(client, "other")
	assert.Nil(t, source)
	require.ErrorContains(t, err, "unsupported controller audience")
}
