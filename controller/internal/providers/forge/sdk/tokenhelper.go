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
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/NVIDIA/nke/controller/internal/config"
	forgesdkhelper "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go/helper"
)

// GetBearerToken returns a bearer token for orgID appropriate for cfg.ForgeEnvironment.
//
// For ForgeEnvironmentProduction it exchanges cfg.FWEAPIKey for an NGC bearer token.
// For ForgeEnvironmentTest it returns a self-signed JWT so the test
// server can be reached without real NGC credentials.
func GetBearerToken(cfg *config.Config, orgID string) (string, error) {
	if cfg.ForgeEnvironment == config.ForgeEnvironmentTest {
		claims := jwt.MapClaims{
			"exp": time.Now().Add(time.Hour).Unix(),
			"sub": config.ForgeEnvironmentTest,
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, err := token.SignedString([]byte(config.ForgeEnvironmentTest))
		if err != nil {
			return "", fmt.Errorf("signing test token: %w", err)
		}
		return signed, nil
	}

	bearerToken, err := forgesdkhelper.GetNGCBearerToken(cfg.ForgeEnvironment, cfg.FWEAPIKey, orgID)
	if err != nil {
		return "", fmt.Errorf("getting NGC bearer token: %w", err)
	}
	if bearerToken == nil {
		return "", fmt.Errorf("bearer token is nil")
	}
	return *bearerToken, nil
}
