// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file ports the NGC bearer-token exchange previously provided by the
// internal SDK's helper package. It is intentionally stdlib-only so the
// controller can be built against the public SDK, which does not ship an
// NGC auth helper.

package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// ngcEnvProduction and ngcEnvStaging are the NGC environment names.
	ngcEnvProduction = "production"
	ngcEnvStaging    = "staging"

	// ngcProductionServerIndex and ngcStagingServerIndex are the server-list
	// indices for the NGC production and staging environments.
	ngcProductionServerIndex = 0
	ngcStagingServerIndex    = 1

	// ngcProductionAuthURL and ngcStagingAuthURL are the NGC auth-service token
	// endpoints used to exchange a legacy API key for a session token.
	ngcProductionAuthURL = "https://authn.nvidia.com/token?service=ngc&scope=group/ngc"
	ngcStagingAuthURL    = "https://stg.auth.ngc.nvidia.com/token?service=ngc&scope=group/ngc-stg"
)

var (
	// ngcEnvServerIndexMap maps an NGC environment name to its server-list index.
	ngcEnvServerIndexMap = map[string]int{ //nolint:gochecknoglobals // static lookup table
		ngcEnvProduction: ngcProductionServerIndex,
		ngcEnvStaging:    ngcStagingServerIndex,
	}

	// ngcEnvAuthURLMap maps an NGC environment name to its auth-service URL.
	ngcEnvAuthURLMap = map[string]string{ //nolint:gochecknoglobals // static lookup table
		ngcEnvProduction: ngcProductionAuthURL,
		ngcEnvStaging:    ngcStagingAuthURL,
	}
)

// ngcAuthServiceResponse is the JSON body returned by the NGC auth service.
type ngcAuthServiceResponse struct {
	Token string `json:"token"`
}

// generateNGCSessionToken exchanges a legacy NGC API key for a session token via
// the NGC auth service.
func generateNGCSessionToken(env, apiKey, org string) (*string, error) {
	if strings.HasPrefix(apiKey, "nvapi") {
		return nil, fmt.Errorf("API key is not a legacy NGC API key")
	}

	authURLPrefix, ok := ngcEnvAuthURLMap[env]
	if !ok {
		return nil, fmt.Errorf("invalid environment: %s", env)
	}

	// Create auth token (base64 encoded)
	authStr := "$oauthtoken:" + apiKey
	authToken := base64.StdEncoding.EncodeToString([]byte(authStr))

	// Create request
	url := authURLPrefix + ":" + org
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("error creating request for NGC auth service: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+authToken)

	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending request to NGC auth service: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code from NGC auth service: %d", resp.StatusCode)
	}

	// Parse response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response from NGC auth service: %w", err)
	}

	// Unmarshal JSON response
	var respData ngcAuthServiceResponse
	if err := json.Unmarshal(body, &respData); err != nil {
		return nil, fmt.Errorf("error parsing JSON response from NGC auth service: %w", err)
	}

	return &respData.Token, nil
}

// getNGCBearerToken returns a bearer token for the given NGC environment.
//
// nvapi-prefixed personal API keys are returned as-is; legacy API keys are
// exchanged for a session token via the NGC auth service.
func getNGCBearerToken(env, apiKey, org string) (*string, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	// Populate the bearer token
	var bearerToken *string

	if strings.HasPrefix(apiKey, "nvapi") {
		// It's an NGC Personal API key
		bearerToken = &apiKey
	} else {
		// Assume legacy API Key
		sessionToken, err := generateNGCSessionToken(env, apiKey, org)
		if err != nil {
			return nil, fmt.Errorf("error generating NGC session token: %w", err)
		}

		bearerToken = sessionToken
	}

	// Return the bearer token
	return bearerToken, nil
}
