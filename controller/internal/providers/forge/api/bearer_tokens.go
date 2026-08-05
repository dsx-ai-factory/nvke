// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/NVIDIA/nke/controller/internal/config"
	forgesdksupport "github.com/NVIDIA/nke/controller/internal/providers/forge/sdk"
)

// BearerToken represents a cached bearer token for a specific Forge Cloud organization
type BearerToken struct {
	OrgID      string
	Token      string
	Expiration time.Time
}

// NewBearerToken obtains a bearer token for orgID using cfg and wraps it in a BearerToken
// that tracks expiration for cache refresh decisions.
func NewBearerToken(cfg *config.Config, orgID string) (*BearerToken, error) {
	tokenStr, err := forgesdksupport.GetBearerToken(cfg, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get bearer token: %w", err)
	}

	bt := &BearerToken{OrgID: orgID, Token: tokenStr}

	if bt.isServiceKeyToken() {
		// Service keys (nvapi-* prefix) do not expire; no expiration parsing needed.
		return bt, nil
	}

	parsedExpiration, err := parseBearerTokenExpiration(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse bearer token expiration: %w", err)
	}

	// subtract a minute off of the expiration (to make sure we refresh before the token
	// expires); if we don't do this, it's possible that the check for expiration would
	// succeed but the in-flight request takes longer than expected to establish a
	// connection and thus, we're denied access
	parsedExpiration = parsedExpiration.Add(-time.Minute)
	if time.Now().After(parsedExpiration) {
		return nil, fmt.Errorf("parsed bearer token expiration is in the past: %v", parsedExpiration)
	}

	bt.Expiration = parsedExpiration
	return bt, nil
}

// HasExpired checks if the bearer token has expired
func (bt *BearerToken) HasExpired() bool {
	if bt.isServiceKeyToken() {
		return false
	}
	return bt.Expiration.Before(time.Now())
}

func (bt *BearerToken) isServiceKeyToken() bool {
	// the token is a NGC service key /personal api token
	// this is not a legacy API token
	return strings.HasPrefix(bt.Token, "nvapi")
}

// parseBearerTokenExpiration parses the expiration time from a bearer token
func parseBearerTokenExpiration(bearerToken string) (time.Time, error) {
	token, _ := jwt.Parse(bearerToken, nil)

	if token == nil {
		return time.Time{}, fmt.Errorf("failed to parse token")
	} else if token.Claims == nil {
		return time.Time{}, fmt.Errorf("token claims are nil")
	}

	expVal, ok := token.Claims.(jwt.MapClaims)["exp"]
	if !ok {
		return time.Time{}, fmt.Errorf("token expiration is missing")
	}

	expiration, ok := expVal.(float64)
	if !ok {
		return time.Time{}, fmt.Errorf("token expiration is not a float64")
	}

	return time.Unix(int64(expiration), 0), nil
}
