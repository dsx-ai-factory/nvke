// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
