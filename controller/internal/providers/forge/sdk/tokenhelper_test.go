// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package sdk

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func TestGetBearerToken_DockerTest(t *testing.T) {
	cfg := &config.Config{ForgeEnvironment: config.ForgeEnvironmentTest}
	token, err := GetBearerToken(cfg, "any-org")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Must be a valid JWT with a future expiration
	parsed, _ := jwt.Parse(token, func(_ *jwt.Token) (interface{}, error) {
		return []byte(config.ForgeEnvironmentTest), nil
	})
	require.NotNil(t, parsed)

	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)

	exp, ok := claims["exp"].(float64)
	require.True(t, ok)
	require.Greater(t, time.Unix(int64(exp), 0).Unix(), time.Now().Unix(), "token must not be expired")
}
