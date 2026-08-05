// SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubeadmconfig

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateToken creates a new kubeadm token.
func GenerateToken() (string, error) {
	// 11 random bytes → 22 hex chars: 3 bytes (6 hex) before the dot, 8 bytes (16 hex) after.
	tokenBytes := make([]byte, 11)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Convert to lowercase hexadecimal
	tokenHex := hex.EncodeToString(tokenBytes)

	// Format as kubeadm token
	token := fmt.Sprintf("%s.%s", tokenHex[:6], tokenHex[6:])

	return token, nil
}
