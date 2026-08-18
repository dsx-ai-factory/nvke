// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const (
	TPMTokenNamePrefix = "nke-cwe-" //nolint:gosec // This is a naming prefix, not credentials
)

var ErrInvalidEKPubHash = errors.New("invalid EK public key hash format")

// TPMTokenName returns the deterministic Teleport token name for a node.
func TPMTokenName(nodeID uuid.UUID) string {
	return TPMTokenNamePrefix + nodeID.String()
}

// ValidateEKPubHash validates that the hash is a valid hex-encoded SHA-256 digest.
func ValidateEKPubHash(hash string) error {
	const expectedLength = sha256.Size * 2 // 64 hex chars = 32-byte SHA-256 digest

	if len(hash) != expectedLength {
		return fmt.Errorf("%w: expected %d hex characters, got %d", ErrInvalidEKPubHash, expectedLength, len(hash))
	}

	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("%w: not valid hexadecimal: %w", ErrInvalidEKPubHash, err)
	}

	return nil
}
