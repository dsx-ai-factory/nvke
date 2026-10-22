// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Codec tests protect cursor isolation, bounded lists and resumption across replicas.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/limits"
)

// TestConfiguration rejects unusable keys and ambiguous scope configuration before issuing tokens.
func TestConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keyLen int
		scope  []string
		valid  bool
	}{
		{name: "minimum key", keyLen: limits.PageTokenKeyMinBytes, scope: []string{"list-v1", "storage"}, valid: true},
		{name: "maximum key", keyLen: limits.PageTokenKeyMaxBytes, scope: []string{"list-v1", "storage"}, valid: true},
		{name: "missing key", scope: []string{"list-v1", "storage"}},
		{name: "short key", keyLen: limits.PageTokenKeyMinBytes - 1, scope: []string{"list-v1", "storage"}},
		{name: "long key", keyLen: limits.PageTokenKeyMaxBytes + 1, scope: []string{"list-v1", "storage"}},
		{name: "missing scope", keyLen: limits.PageTokenKeyMinBytes},
		{name: "domain only", keyLen: limits.PageTokenKeyMinBytes, scope: []string{"list-v1"}},
		{name: "empty domain", keyLen: limits.PageTokenKeyMinBytes, scope: []string{"", "storage"}},
		{name: "empty scope component", keyLen: limits.PageTokenKeyMinBytes, scope: []string{"list-v1", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(make([]byte, tc.keyLen), tc.scope)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestResolve checks list policy independently of resource wiring and cursor encryption.
func TestResolve(t *testing.T) {
	codec, err := New(make([]byte, limits.PageTokenKeyMinBytes), []string{"list-v1", "storage"})
	require.NoError(t, err)
	const continuation = "private-storage-position"
	token, err := codec.Encode(continuation, 2)
	require.NoError(t, err)
	largerPage, err := codec.Encode(continuation, 200)
	require.NoError(t, err)
	bounds := PageLimits{Default: 50, Max: 100}
	for _, tc := range []struct {
		name     string
		size     int32
		token    string
		bounds   PageLimits
		wantSize int32
		code     codes.Code
	}{
		{name: "default first page", bounds: bounds, wantSize: 50},
		{name: "explicit first page", size: 100, bounds: bounds, wantSize: 100},
		{name: "previous page size", token: token, bounds: bounds, wantSize: 2},
		{name: "matching page size", size: 2, token: token, bounds: bounds, wantSize: 2},
		{name: "changed page size", size: 3, token: token, bounds: bounds, code: codes.InvalidArgument},
		{name: "negative page size", size: -1, bounds: bounds, code: codes.InvalidArgument},
		{name: "oversized page size", size: 101, bounds: bounds, code: codes.InvalidArgument},
		{name: "oversized cursor page size", token: largerPage, bounds: bounds, code: codes.InvalidArgument},
		{name: "invalid token", token: "invalid-token", bounds: bounds, code: codes.InvalidArgument},
		{name: "different default", bounds: PageLimits{Default: 75, Max: 200}, wantSize: 75},
		{name: "different maximum", token: largerPage, bounds: PageLimits{Default: 75, Max: 200}, wantSize: 200},
		{name: "missing default", bounds: PageLimits{Max: 100}, code: codes.Internal},
		{name: "default above maximum", bounds: PageLimits{Default: 101, Max: 100}, code: codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := codec.Resolve(tc.size, tc.token, tc.bounds)
			require.Equal(t, tc.code, status.Code(err))
			if tc.code != codes.OK {
				if tc.token != "" {
					assert.Contains(t, err.Error(), "restart listing")
				}
				return
			}
			assert.Equal(t, tc.wantSize, page.Size)
			if tc.token != "" {
				assert.Equal(t, continuation, page.Continue)
			} else {
				assert.Empty(t, page.Continue)
			}
		})
	}
}

// TestPageTokens verifies secrecy, scope isolation, expiration and bounds independently of list policy.
func TestPageTokens(t *testing.T) {
	key := make([]byte, limits.PageTokenKeyMinBytes)
	scope := []string{"list-v1", "storage", "collection"}
	codec, err := New(key, scope)
	require.NoError(t, err)
	want := Cursor{Continue: "private-storage-namespace-and-key", Size: 200}
	token, err := codec.Encode(want.Continue, want.Size)
	require.NoError(t, err)
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(token)
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), want.Continue)
	sealed[len(sealed)-1] ^= 1
	_, err = codec.Decode(base64.RawURLEncoding.EncodeToString(sealed))
	require.Error(t, err, "authenticated payload changes must be rejected")
	resumed, err := New(key, scope)
	require.NoError(t, err)
	got, err := resumed.Decode(token)
	require.NoError(t, err)
	assert.Equal(t, want, got, "replicas resume cursors without imposing a resource's page-size maximum")
	for _, tc := range []struct {
		name  string
		key   []byte
		scope []string
	}{
		{name: "domain", key: key, scope: []string{"another-list-v1", "storage", "collection"}},
		{name: "scope", key: key, scope: []string{"list-v1", "another-storage", "collection"}},
		{name: "key", key: []byte(strings.Repeat("k", limits.PageTokenKeyMinBytes)), scope: scope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other, err := New(tc.key, tc.scope)
			require.NoError(t, err)
			_, err = other.Decode(token)
			require.Error(t, err)
		})
	}
	for _, tc := range []struct {
		name   string
		cursor pageCursor
	}{
		{name: "expired", cursor: pageCursor{Cursor: want, Expires: time.Now().Add(-time.Minute).Unix()}},
		{name: "empty position", cursor: pageCursor{Size: 2, Expires: time.Now().Add(time.Minute).Unix()}},
		{name: "nonpositive size", cursor: pageCursor{Continue: want.Continue, Expires: time.Now().Add(time.Minute).Unix()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.cursor)
			require.NoError(t, err)
			sealed := codec.tokens.Seal(nil, nil, data, codec.scope) // #nosec G407 -- NewGCMWithRandomNonce requires nil.
			_, err = codec.Decode(base64.RawURLEncoding.EncodeToString(sealed))
			require.Error(t, err)
		})
	}
	for name, invalid := range map[string]string{
		"malformed": "A" + token,
		"short":     "AA",
		"oversized": strings.Repeat("A", maxTokenBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := codec.Decode(invalid)
			require.Error(t, err)
		})
	}
	_, err = codec.Encode(strings.Repeat("x", maxTokenBytes), 2)
	require.Error(t, err, "oversized cursors cannot produce tokens that decoding would reject")
}
