// SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
