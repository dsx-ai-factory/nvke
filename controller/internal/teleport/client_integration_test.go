// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package teleport_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/teleport"
)

// Default token expiry for integration tests.
// Can be overridden with TELEPORT_TEST_TOKEN_EXPIRY env var (e.g., "30m", "2h").
const defaultTestTokenExpiry = 5 * time.Minute

func getTestTokenExpiry() time.Duration {
	if val := os.Getenv("TELEPORT_TEST_TOKEN_EXPIRY"); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return defaultTestTokenExpiry
}

// getTestClient creates a Teleport client for integration testing.
// It skips the test if Teleport is not enabled.
func getTestClient(t *testing.T) teleport.Client {
	t.Helper()

	cfg, err := config.ParseConfigFromEnv()
	if err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}
	if !cfg.Teleport.Enabled {
		t.Skip("TELEPORT_ENABLED not set, skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := teleport.NewClient(ctx, cfg.Teleport)
	if err != nil {
		t.Fatalf("failed to create teleport client: %v", err)
	}

	t.Cleanup(func() { client.Close() })
	return client
}

// testTokenName generates a unique token name for each test.
func testTokenName(t *testing.T) string {
	t.Helper()
	// Use test name + timestamp for uniqueness
	return fmt.Sprintf("cwe-integration-test-%d", time.Now().UnixNano())
}

// createTestToken creates a standard test token with the given name.
// Tokens auto-expire since we don't have delete permissions.
// Default expiry is 5 minutes, configurable via TELEPORT_TEST_TOKEN_EXPIRY env var.
func createTestToken(name string) teleport.ProvisionToken {
	return teleport.ProvisionToken{
		Name:       name,
		Roles:      []string{"Node"},
		JoinMethod: teleport.JoinMethodTPM,
		Expiry:     time.Now().Add(getTestTokenExpiry()),
		Labels: map[string]string{
			"cwe-integration-test": "true",
			"created-by":           "cwe-integration-test",
		},
		Spec: &teleport.TPMConfig{
			Allow: []teleport.TPMRule{{
				Description:  "CWE integration test token",
				EKPublicHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
		},
	}
}

// TestIntegration_TokenCreateAndRead tests creating and reading a token.
func TestIntegration_TokenCreateAndRead(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()
	tokenName := testTokenName(t)

	// Create token
	token := createTestToken(tokenName)
	err := client.UpsertToken(ctx, token)
	require.NoError(t, err, "UpsertToken should succeed")

	// Get token and verify
	got, err := client.GetToken(ctx, tokenName)
	require.NoError(t, err, "GetToken should succeed")
	assert.Equal(t, tokenName, got.Name)
	assert.Equal(t, teleport.JoinMethodTPM, got.JoinMethod)
	assert.Equal(t, []string{"Node"}, got.Roles)
	assert.Equal(t, "true", got.Labels["cwe-integration-test"])

	// Verify ownership labels were automatically added
	assert.Equal(t, "nke-cloud-workflow-engine", got.Labels[teleport.LabelManagedBy],
		"managed-by label should be set automatically")

	// Verify TPM config
	tpmConfig, ok := got.Spec.(*teleport.TPMConfig)
	require.True(t, ok, "Spec should be *TPMConfig")
	require.Len(t, tpmConfig.Allow, 1, "Should have one Allow rule")
	assert.Equal(t, "CWE integration test token", tpmConfig.Allow[0].Description)
}

// TestIntegration_OwnershipLabels verifies that ownership labels are automatically injected.
func TestIntegration_OwnershipLabels(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()
	tokenName := testTokenName(t)

	// Create token with custom labels (no ownership labels)
	token := teleport.ProvisionToken{
		Name:       tokenName,
		Roles:      []string{"Node"},
		JoinMethod: teleport.JoinMethodTPM,
		Expiry:     time.Now().Add(getTestTokenExpiry()),
		Labels: map[string]string{
			"custom-label": "custom-value",
		},
		Spec: &teleport.TPMConfig{
			Allow: []teleport.TPMRule{{
				Description:  "Test ownership labels",
				EKPublicHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
		},
	}

	err := client.UpsertToken(ctx, token)
	require.NoError(t, err)

	// Retrieve and verify ownership labels were added
	got, err := client.GetToken(ctx, tokenName)
	require.NoError(t, err)

	// Custom label should be preserved
	assert.Equal(t, "custom-value", got.Labels["custom-label"])

	// Ownership label should be automatically added
	assert.Equal(t, "nke-cloud-workflow-engine", got.Labels[teleport.LabelManagedBy])
}

// TestIntegration_ListTokensFiltersOwned verifies ListTokens only returns CWE-owned tokens.
func TestIntegration_ListTokensFiltersOwned(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()
	tokenName := testTokenName(t)

	// Create a token
	token := createTestToken(tokenName)
	err := client.UpsertToken(ctx, token)
	require.NoError(t, err)

	// ListTokens should include our token (it's owned by CWE)
	tokens, err := client.ListTokens(ctx)
	require.NoError(t, err)

	found := false
	for _, tok := range tokens {
		if tok.Name == tokenName {
			found = true
			break
		}
	}
	assert.True(t, found, "our token should appear in tokens list")

	// All returned tokens should have the ownership label (filtered by default)
	for _, tok := range tokens {
		assert.Equal(t, "nke-cloud-workflow-engine", tok.Labels[teleport.LabelManagedBy],
			"all listed tokens should have managed-by label")
	}
}

// TestIntegration_TokenList tests listing tokens and finding our created token.
func TestIntegration_TokenList(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()
	tokenName := testTokenName(t)

	// Create token
	token := createTestToken(tokenName)
	err := client.UpsertToken(ctx, token)
	require.NoError(t, err, "UpsertToken should succeed")

	// List tokens and verify ours is present
	tokens, err := client.ListTokens(ctx)
	require.NoError(t, err, "ListTokens should succeed")

	found := false
	for _, tok := range tokens {
		if tok.Name == tokenName {
			found = true
			break
		}
	}
	assert.True(t, found, "token should appear in list")
}

// TestIntegration_TokenNotFound verifies error handling for non-existent tokens.
func TestIntegration_TokenNotFound(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()

	_, err := client.GetToken(ctx, "nonexistent-token-that-should-not-exist-12345")
	assert.True(t, trace.IsNotFound(err), "GetToken should return trace.NotFound error for non-existent token")
}

// TestIntegration_UpsertUpdatesExisting verifies that Upsert updates an existing token.
func TestIntegration_UpsertUpdatesExisting(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()
	tokenName := testTokenName(t)

	// Create initial token with version label
	token := createTestToken(tokenName)
	token.Labels["version"] = "1"
	require.NoError(t, client.UpsertToken(ctx, token), "initial UpsertToken should succeed")

	// Verify initial version
	got, err := client.GetToken(ctx, tokenName)
	require.NoError(t, err)
	assert.Equal(t, "1", got.Labels["version"])

	// Update with new version label
	token.Labels["version"] = "2"
	require.NoError(t, client.UpsertToken(ctx, token), "update UpsertToken should succeed")

	// Verify update
	got, err = client.GetToken(ctx, tokenName)
	require.NoError(t, err)
	assert.Equal(t, "2", got.Labels["version"])
}

// TestIntegration_MultipleTPMRules verifies tokens with multiple TPM allow rules.
func TestIntegration_MultipleTPMRules(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()
	tokenName := testTokenName(t)

	// Create token with multiple TPM rules
	token := teleport.ProvisionToken{
		Name:       tokenName,
		Roles:      []string{"Node"},
		JoinMethod: teleport.JoinMethodTPM,
		Expiry:     time.Now().Add(getTestTokenExpiry()),
		Labels: map[string]string{
			"cwe-integration-test": "true",
		},
		Spec: &teleport.TPMConfig{
			Allow: []teleport.TPMRule{
				{
					Description:  "First node",
					EKPublicHash: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				},
				{
					Description:  "Second node",
					EKPublicHash: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				},
				{
					Description:  "Third node",
					EKPublicHash: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
				},
			},
		},
	}

	err := client.UpsertToken(ctx, token)
	require.NoError(t, err, "UpsertToken with multiple rules should succeed")

	// Verify all rules are present
	got, err := client.GetToken(ctx, tokenName)
	require.NoError(t, err)

	tpmConfig, ok := got.Spec.(*teleport.TPMConfig)
	require.True(t, ok)
	require.Len(t, tpmConfig.Allow, 3, "Should have 3 Allow rules")

	// Verify each rule
	descriptions := make([]string, len(tpmConfig.Allow))
	for i, rule := range tpmConfig.Allow {
		descriptions[i] = rule.Description
	}
	assert.Contains(t, descriptions, "First node")
	assert.Contains(t, descriptions, "Second node")
	assert.Contains(t, descriptions, "Third node")
}
