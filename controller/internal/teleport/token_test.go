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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateEKPubHash(t *testing.T) {
	// Valid SHA256 hex hash (64 characters)
	validHash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

	tests := []struct {
		name        string
		hash        string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid hash",
			hash:        validHash,
			expectError: false,
		},
		{
			name:        "valid hash with uppercase",
			hash:        "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2",
			expectError: false,
		},
		{
			name:        "valid hash mixed case",
			hash:        "a1B2c3D4e5F6a1B2c3D4e5F6a1B2c3D4e5F6a1B2c3D4e5F6a1B2c3D4e5F6a1B2",
			expectError: false,
		},
		{
			name:        "empty string",
			hash:        "",
			expectError: true,
			errorMsg:    "expected 64 hex characters, got 0",
		},
		{
			name:        "too short",
			hash:        "a1b2c3d4",
			expectError: true,
			errorMsg:    "expected 64 hex characters, got 8",
		},
		{
			name:        "too long",
			hash:        validHash + "extra",
			expectError: true,
			errorMsg:    "expected 64 hex characters, got 69",
		},
		{
			name:        "invalid hex characters",
			hash:        "g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			expectError: true,
			errorMsg:    "not valid hexadecimal",
		},
		{
			name:        "contains spaces",
			hash:        "a1b2c3d4 e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			expectError: true,
			errorMsg:    "expected 64 hex characters, got 65",
		},
		{
			name:        "contains special characters",
			hash:        "a1b2c3d4-e5f6-a1b2-c3d4-e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6",
			expectError: true,
			errorMsg:    "not valid hexadecimal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEKPubHash(tt.hash)

			if tt.expectError {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidEKPubHash, "error should wrap ErrInvalidEKPubHash")
				assert.Contains(t, err.Error(), tt.errorMsg,
					"error message should contain %q", tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
