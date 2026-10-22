// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pagination resolves list inputs and protects datastore cursors under caller-owned limits.
package pagination

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/limits"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/validation"
)

const maxTokenBytes = 16 << 10

// PageLimits keeps each resource's page-size policy explicit when resolving a list request.
type PageLimits struct {
	Default int32 // Used for the first page when the request omits a size.
	Max     int32 // Caps requested and resumed pages for the resource.
}

// Cursor retains the datastore position and page boundary needed to resume a list.
type Cursor struct {
	Continue string `json:"continue"` // Resumes the stored snapshot without exposing its internal cursor.
	Size     int32  `json:"size"`     // Lets each service enforce its continuation page-size policy.
}

type pageCursor struct {
	Cursor
	Expires int64 `json:"expires"` // Bounds replay independently of the datastore cursor's lifetime.
}

// Codec authenticates cursors within a caller-supplied collection scope shared by replicas.
// Tokens carry no authorization. Callers must authorize every list request.
type Codec struct {
	tokens cipher.AEAD
	scope  []byte
}

// New binds tokens to an explicit domain and storage scope without retaining the supplied key.
func New(key []byte, scope []string) (*Codec, error) {
	if len(key) < limits.PageTokenKeyMinBytes || len(key) > limits.PageTokenKeyMaxBytes {
		return nil, fmt.Errorf("page-token key must contain between %d and %d bytes", limits.PageTokenKeyMinBytes, limits.PageTokenKeyMaxBytes)
	}
	if len(scope) < 2 || slices.Contains(scope, "") {
		return nil, fmt.Errorf("page-token scope must contain nonempty domain and scope values")
	}
	digest := sha256.Sum256(key)
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return nil, fmt.Errorf("create page-token cipher: %w", err)
	}
	tokens, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("create page-token cipher: %w", err)
	}
	data, err := json.Marshal(scope)
	if err != nil {
		return nil, fmt.Errorf("encode pagination scope: %w", err)
	}
	return &Codec{tokens: tokens, scope: data}, nil
}

// Resolve validates list inputs and restores the page size encoded in a continuation.
func (c *Codec) Resolve(size int32, token string, bounds PageLimits) (Cursor, error) {
	if bounds.Default < 1 || bounds.Max < bounds.Default {
		return Cursor{}, status.Error(codes.Internal, "invalid pagination limits")
	}
	if size < 0 || size > bounds.Max {
		return Cursor{}, validation.FieldViolation("page_size", fmt.Sprintf("must be between 1 and %d, or omitted", bounds.Max))
	}
	if token == "" {
		if size == 0 {
			size = bounds.Default
		}
		return Cursor{Size: size}, nil
	}
	cursor, err := c.Decode(token)
	if err != nil || cursor.Size > bounds.Max || (size != 0 && size != cursor.Size) {
		return Cursor{}, status.Error(codes.InvalidArgument, "invalid or expired page token, or changed page size; restart listing")
	}
	return cursor, nil
}

// Encode seals an expiring cursor so callers can resume without storing pagination sessions.
func (c *Codec) Encode(continuation string, size int32) (string, error) {
	data, err := json.Marshal(pageCursor{Continue: continuation, Size: size, Expires: time.Now().Add(15 * time.Minute).Unix()})
	if err != nil {
		return "", err
	}
	sealed := c.tokens.Seal(nil, nil, data, c.scope) // #nosec G407 -- NewGCMWithRandomNonce generates and prepends the nonce. Nil is required.
	token := base64.RawURLEncoding.EncodeToString(sealed)
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("storage continuation exceeds page token limit")
	}
	return token, nil
}

// Decode rejects altered, expired or out-of-scope cursors before they reach storage.
func (c *Codec) Decode(token string) (Cursor, error) {
	invalid := fmt.Errorf("invalid page token")
	if len(token) > maxTokenBytes {
		return Cursor{}, invalid
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(sealed) < c.tokens.Overhead() {
		return Cursor{}, invalid
	}
	data, err := c.tokens.Open(nil, nil, sealed, c.scope)
	if err != nil {
		return Cursor{}, invalid
	}
	var cursor pageCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Continue == "" || cursor.Size <= 0 || cursor.Expires <= time.Now().Unix() {
		return Cursor{}, invalid
	}
	return cursor.Cursor, nil
}
