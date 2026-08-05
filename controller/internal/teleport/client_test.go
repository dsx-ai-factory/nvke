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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/gravitational/teleport/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
)

// MockClient is a mock implementation of the Client interface for testing.
type MockClient struct {
	mock.Mock
}

func (m *MockClient) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockClient) UpsertToken(ctx context.Context, token ProvisionToken) error {
	args := m.Called(ctx, token)
	return args.Error(0)
}

func (m *MockClient) GetToken(ctx context.Context, name string) (*ProvisionToken, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ProvisionToken), args.Error(1)
}

func (m *MockClient) DeleteToken(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *MockClient) ListTokens(ctx context.Context) ([]ProvisionToken, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]ProvisionToken), args.Error(1)
}

func (m *MockClient) GetNode(ctx context.Context, namespace, name string) (*Node, error) {
	args := m.Called(ctx, namespace, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*Node), args.Error(1)
}

func (m *MockClient) DeleteNode(ctx context.Context, namespace, name string) error {
	args := m.Called(ctx, namespace, name)
	return args.Error(0)
}

func (m *MockClient) ListNodes(ctx context.Context, namespace string, opts ...ListNodesOption) ([]Node, error) {
	args := m.Called(ctx, namespace, opts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]Node), args.Error(1)
}

// Ensure MockClient implements Client
var _ Client = (*MockClient)(nil)

// generateTestCertPEM creates a PEM-encoded certificate with the given expiry.
func generateTestCertPEM(t *testing.T, expiry time.Time) []byte {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     expiry,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func TestNewClient_ConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.TeleportConfig
		wantErr error
	}{
		{
			name: "disabled returns error",
			cfg: config.TeleportConfig{
				Enabled:      false,
				IdentityFile: "/path/to/identity",
				ProxyAddress: "proxy.example.com:443",
			},
			wantErr: ErrTeleportDisabled,
		},
		{
			name: "missing identity file returns error",
			cfg: config.TeleportConfig{
				Enabled:      true,
				IdentityFile: "",
				ProxyAddress: "proxy.example.com:443",
			},
			wantErr: ErrIdentityFileNotSet,
		},
		{
			name: "missing proxy address returns error",
			cfg: config.TeleportConfig{
				Enabled:      true,
				IdentityFile: "/path/to/identity",
				ProxyAddress: "",
			},
			wantErr: ErrProxyAddressNotSet,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			client, err := NewClient(ctx, tt.cfg)

			assert.Nil(t, client)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestMockClient_TPMTokenOperations(t *testing.T) {
	ctx := context.Background()

	t.Run("UpsertToken with TPM config", func(t *testing.T) {
		mockClient := new(MockClient)
		token := ProvisionToken{
			Name:       "tpm-join-token",
			Roles:      []string{"Node"},
			Expiry:     time.Now().Add(time.Hour),
			JoinMethod: JoinMethodTPM,
			Labels: map[string]string{
				"resource-group": "nke",
			},
			Spec: &TPMConfig{
				Allow: []TPMRule{
					{
						Description:  "worker-node-1",
						EKPublicHash: "d4b45864d9d6fabfc568d74f26c35ababde2105337d7af9a6605e1c56c891aa6",
					},
				},
			},
		}

		mockClient.On("UpsertToken", ctx, token).Return(nil)

		err := mockClient.UpsertToken(ctx, token)

		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("UpsertToken error", func(t *testing.T) {
		mockClient := new(MockClient)
		token := ProvisionToken{
			Name:       "test-token",
			Roles:      []string{"Node"},
			JoinMethod: JoinMethodTPM,
		}
		expectedErr := errors.New("permission denied")

		mockClient.On("UpsertToken", ctx, token).Return(expectedErr)

		err := mockClient.UpsertToken(ctx, token)

		require.Error(t, err)
		assert.Equal(t, expectedErr, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("GetToken returns TPM config", func(t *testing.T) {
		mockClient := new(MockClient)
		expectedToken := &ProvisionToken{
			Name:       "tpm-join-token",
			Roles:      []string{"Node"},
			Expiry:     time.Now().Add(time.Hour),
			JoinMethod: JoinMethodTPM,
			Spec: &TPMConfig{
				Allow: []TPMRule{
					{
						Description:  "worker-1",
						EKPublicHash: "sha256:abc123",
					},
				},
			},
		}

		mockClient.On("GetToken", ctx, "tpm-join-token").Return(expectedToken, nil)

		token, err := mockClient.GetToken(ctx, "tpm-join-token")

		require.NoError(t, err)
		assert.Equal(t, JoinMethodTPM, token.JoinMethod)

		tpmCfg, ok := token.Spec.(*TPMConfig)
		require.True(t, ok, "Spec should be *TPMConfig")
		assert.Len(t, tpmCfg.Allow, 1)
		assert.Equal(t, "sha256:abc123", tpmCfg.Allow[0].EKPublicHash)
		mockClient.AssertExpectations(t)
	})

	t.Run("GetToken not found", func(t *testing.T) {
		mockClient := new(MockClient)
		expectedErr := errors.New("token not found")

		mockClient.On("GetToken", ctx, "nonexistent").Return(nil, expectedErr)

		token, err := mockClient.GetToken(ctx, "nonexistent")

		assert.Nil(t, token)
		require.Error(t, err)
		assert.Equal(t, expectedErr, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("DeleteToken success", func(t *testing.T) {
		mockClient := new(MockClient)

		mockClient.On("DeleteToken", ctx, "test-token").Return(nil)

		err := mockClient.DeleteToken(ctx, "test-token")

		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("ListTokens success", func(t *testing.T) {
		mockClient := new(MockClient)
		expectedTokens := []ProvisionToken{
			{Name: "token-1", Roles: []string{"Node"}, JoinMethod: JoinMethodTPM},
			{Name: "token-2", Roles: []string{"Node"}, JoinMethod: JoinMethodTPM},
		}

		mockClient.On("ListTokens", ctx).Return(expectedTokens, nil)

		tokens, err := mockClient.ListTokens(ctx)

		require.NoError(t, err)
		assert.Len(t, tokens, 2)
		mockClient.AssertExpectations(t)
	})
}

func TestMockClient_NodeOperations(t *testing.T) {
	ctx := context.Background()
	namespace := "default"

	t.Run("GetNode success", func(t *testing.T) {
		mockClient := new(MockClient)
		expectedNode := &Node{
			Name:      "test-node",
			Hostname:  "test-node.example.com",
			Addr:      "10.0.0.1:22",
			Namespace: namespace,
			Labels: map[string]string{
				"resource-group": "nke",
			},
		}

		mockClient.On("GetNode", ctx, namespace, "test-node").Return(expectedNode, nil)

		node, err := mockClient.GetNode(ctx, namespace, "test-node")

		require.NoError(t, err)
		assert.Equal(t, expectedNode, node)
		mockClient.AssertExpectations(t)
	})

	t.Run("GetNode not found", func(t *testing.T) {
		mockClient := new(MockClient)
		expectedErr := errors.New("node not found")

		mockClient.On("GetNode", ctx, namespace, "nonexistent").Return(nil, expectedErr)

		node, err := mockClient.GetNode(ctx, namespace, "nonexistent")

		assert.Nil(t, node)
		require.Error(t, err)
		assert.Equal(t, expectedErr, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("DeleteNode success", func(t *testing.T) {
		mockClient := new(MockClient)

		mockClient.On("DeleteNode", ctx, namespace, "test-node").Return(nil)

		err := mockClient.DeleteNode(ctx, namespace, "test-node")

		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("ListNodes success", func(t *testing.T) {
		mockClient := new(MockClient)
		expectedNodes := []Node{
			{Name: "node-1", Hostname: "node-1.example.com"},
			{Name: "node-2", Hostname: "node-2.example.com"},
		}

		mockClient.On("ListNodes", ctx, namespace, []ListNodesOption(nil)).Return(expectedNodes, nil)

		nodes, err := mockClient.ListNodes(ctx, namespace)

		require.NoError(t, err)
		assert.Len(t, nodes, 2)
		mockClient.AssertExpectations(t)
	})
}

func TestMockClient_Close(t *testing.T) {
	t.Run("Close success", func(t *testing.T) {
		mockClient := new(MockClient)

		mockClient.On("Close").Return(nil)

		err := mockClient.Close()

		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})
}

func TestJoinMethod_Constants(t *testing.T) {
	assert.Equal(t, JoinMethodTPM, JoinMethod("tpm"))
}

func TestProvisionToken_Structure(t *testing.T) {
	expiry := time.Now().Add(24 * time.Hour)
	token := ProvisionToken{
		Name:       "tpm-join-token",
		Roles:      []string{"Node"},
		Expiry:     expiry,
		JoinMethod: JoinMethodTPM,
		Labels: map[string]string{
			"resource-group": "nke",
			"cluster":        "test-cluster",
		},
		Spec: &TPMConfig{
			Allow: []TPMRule{
				{
					Description:  "worker-1",
					EKPublicHash: "d4b45864d9d6fabfc568d74f26c35ababde2105337d7af9a6605e1c56c891aa6",
				},
			},
			EKCertAllowedCAs: []string{"-----BEGIN CERTIFICATE-----\n..."},
		},
	}

	assert.Equal(t, "tpm-join-token", token.Name)
	assert.Contains(t, token.Roles, "Node")
	assert.Equal(t, JoinMethodTPM, token.JoinMethod)
	assert.Equal(t, "nke", token.Labels["resource-group"])
	assert.Equal(t, expiry, token.Expiry)

	tpmCfg, ok := token.Spec.(*TPMConfig)
	require.True(t, ok)
	assert.Len(t, tpmCfg.Allow, 1)
	assert.Equal(t, "worker-1", tpmCfg.Allow[0].Description)
	assert.Len(t, tpmCfg.EKCertAllowedCAs, 1)
}

func TestTPMConfig_Structure(t *testing.T) {
	cfg := TPMConfig{
		Allow: []TPMRule{
			{
				Description:         "server-asset-12345",
				EKPublicHash:        "d4b45864d9d6fabfc568d74f26c35ababde2105337d7af9a6605e1c56c891aa6",
				EKCertificateSerial: "73:df:dc:bd:af:ef:8a:d8:15:2e:96:71:7a:3e:7f:a4",
			},
		},
		EKCertAllowedCAs: []string{"-----BEGIN CERTIFICATE-----\nMII..."},
	}

	assert.Len(t, cfg.Allow, 1)
	assert.Equal(t, "server-asset-12345", cfg.Allow[0].Description)
	assert.NotEmpty(t, cfg.Allow[0].EKPublicHash)
	assert.NotEmpty(t, cfg.Allow[0].EKCertificateSerial)
	assert.Len(t, cfg.EKCertAllowedCAs, 1)
}

func TestNode_Structure(t *testing.T) {
	node := Node{
		Name:      "worker-node-1",
		Hostname:  "worker-1.cluster.local",
		Addr:      "10.0.0.10:3022",
		Namespace: "default",
		Labels: map[string]string{
			"resource-group": "nke",
			"role":           "worker",
		},
	}

	assert.Equal(t, "worker-node-1", node.Name)
	assert.Equal(t, "worker-1.cluster.local", node.Hostname)
	assert.Equal(t, "10.0.0.10:3022", node.Addr)
	assert.Equal(t, "default", node.Namespace)
	assert.Equal(t, "worker", node.Labels["role"])
}

func TestMockClient_TPMWorkflow(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockClient)

	token := ProvisionToken{
		Name:       "node-tpm-token",
		Roles:      []string{"Node"},
		JoinMethod: JoinMethodTPM,
		Labels:     map[string]string{"resource-group": "nke"},
		Spec: &TPMConfig{
			Allow: []TPMRule{
				{
					Description:  "worker-node-1",
					EKPublicHash: "d4b45864d9d6fabfc568d74f26c35ababde2105337d7af9a6605e1c56c891aa6",
				},
			},
		},
	}

	mockClient.On("UpsertToken", ctx, mock.AnythingOfType("ProvisionToken")).Return(nil)
	mockClient.On("GetToken", ctx, "node-tpm-token").Return(&token, nil)
	mockClient.On("DeleteToken", ctx, "node-tpm-token").Return(nil)

	err := mockClient.UpsertToken(ctx, token)
	require.NoError(t, err)

	retrievedToken, err := mockClient.GetToken(ctx, "node-tpm-token")
	require.NoError(t, err)
	assert.Equal(t, JoinMethodTPM, retrievedToken.JoinMethod)

	tpmCfg, ok := retrievedToken.Spec.(*TPMConfig)
	require.True(t, ok)
	assert.Len(t, tpmCfg.Allow, 1)

	err = mockClient.DeleteToken(ctx, "node-tpm-token")
	require.NoError(t, err)

	mockClient.AssertExpectations(t)
}

// Internal conversion function tests

func TestToTeleportToken(t *testing.T) {
	c := &clientImpl{}
	expiry := time.Now().Add(time.Hour)

	t.Run("unsupported join method returns error", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "static-token",
			Roles:      []string{"Node"},
			Expiry:     expiry,
			JoinMethod: JoinMethod("token"), // Static tokens not supported
		}

		_, err := c.toTeleportToken(token)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsupportedJoinMethod)
	})

	t.Run("TPM join method with Allow rules", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "tpm-token",
			Roles:      []string{"Node", "App"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
			Labels: map[string]string{
				"resource-group": "nke",
			},
			Spec: &TPMConfig{
				Allow: []TPMRule{
					{
						Description:  "worker-1",
						EKPublicHash: "sha256:abc123",
					},
					{
						Description:         "worker-2",
						EKCertificateSerial: "12:34:56:78",
					},
				},
			},
		}

		result, err := c.toTeleportToken(token)
		require.NoError(t, err)
		assert.Equal(t, "tpm-token", result.GetName())
		assert.Equal(t, types.JoinMethodTPM, result.GetJoinMethod())
		assert.Contains(t, result.GetRoles(), types.SystemRole("Node"))
		assert.Contains(t, result.GetRoles(), types.SystemRole("App"))

		v2, ok := result.(*types.ProvisionTokenV2)
		require.True(t, ok)
		require.NotNil(t, v2.Spec.TPM)
		assert.Len(t, v2.Spec.TPM.Allow, 2)
		assert.Equal(t, "worker-1", v2.Spec.TPM.Allow[0].Description)
		assert.Equal(t, "sha256:abc123", v2.Spec.TPM.Allow[0].EKPublicHash)
	})

	t.Run("TPM join method with EKCertAllowedCAs and Allow rules", func(t *testing.T) {
		testCert := string(generateTestCertPEM(t, expiry.Add(365*24*time.Hour)))
		token := ProvisionToken{
			Name:       "tpm-ca-token",
			Roles:      []string{"Node"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
			Spec: &TPMConfig{
				Allow: []TPMRule{
					{
						Description:         "ca-validated-host",
						EKCertificateSerial: "00:11:22:33",
					},
				},
				EKCertAllowedCAs: []string{testCert},
			},
		}

		result, err := c.toTeleportToken(token)
		require.NoError(t, err)
		v2, ok := result.(*types.ProvisionTokenV2)
		require.True(t, ok)
		require.NotNil(t, v2.Spec.TPM)
		assert.Len(t, v2.Spec.TPM.EKCertAllowedCAs, 1)
		assert.Len(t, v2.Spec.TPM.Allow, 1)
	})

	t.Run("TPM join method with bot name", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "bot-token",
			Roles:      []string{"Bot"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
			BotName:    "my-bot",
			Spec: &TPMConfig{
				Allow: []TPMRule{
					{Description: "bot-host", EKPublicHash: "sha256:xyz"},
				},
			},
		}

		result, err := c.toTeleportToken(token)
		require.NoError(t, err)
		assert.Equal(t, "my-bot", result.GetBotName())
	})

	t.Run("TPM join method missing config", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "tpm-no-config",
			Roles:      []string{"Node"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
		}

		_, err := c.toTeleportToken(token)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TPM join method requires *TPMConfig")
	})

	t.Run("TPM join method nil spec", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "tpm-nil-spec",
			Roles:      []string{"Node"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
			Spec:       nil, // TokenSpec interface prevents wrong types at compile time
		}

		_, err := c.toTeleportToken(token)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TPM join method requires *TPMConfig")
	})

	t.Run("TPM join method empty config", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "tpm-empty-config",
			Roles:      []string{"Node"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
			Spec:       &TPMConfig{},
		}

		_, err := c.toTeleportToken(token)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TPM join method requires at least one Allow rule")
	})

	t.Run("token without labels", func(t *testing.T) {
		token := ProvisionToken{
			Name:       "no-labels-token",
			Roles:      []string{"Node"},
			Expiry:     expiry,
			JoinMethod: JoinMethodTPM,
			Spec: &TPMConfig{
				Allow: []TPMRule{{Description: "rule", EKPublicHash: "sha256:hash"}},
			},
		}

		result, err := c.toTeleportToken(token)
		require.NoError(t, err)
		assert.Equal(t, "no-labels-token", result.GetName())
	})
}

func TestFromTeleportToken(t *testing.T) {
	expiry := time.Now().Add(time.Hour)

	t.Run("unknown join method (e.g. static token) leaves Spec nil", func(t *testing.T) {
		// Test that we can read tokens with unsupported join methods from Teleport
		// (e.g., static tokens created by other means) - Spec will be nil
		tt, err := types.NewProvisionTokenFromSpec("static-token", expiry, types.ProvisionTokenSpecV2{
			Roles:      types.SystemRoles{types.RoleNode},
			JoinMethod: types.JoinMethodToken,
		})
		require.NoError(t, err)

		result := fromTeleportToken(tt)
		assert.Equal(t, "static-token", result.Name)
		assert.Equal(t, JoinMethod("token"), result.JoinMethod) // raw string, not a CWE constant
		assert.Contains(t, result.Roles, "Node")
		assert.Nil(t, result.Spec) // unsupported join methods have nil Spec
	})

	t.Run("TPM token with rules", func(t *testing.T) {
		testCert := string(generateTestCertPEM(t, expiry.Add(365*24*time.Hour)))
		tt, err := types.NewProvisionTokenFromSpec("tpm-token", expiry, types.ProvisionTokenSpecV2{
			Roles:      types.SystemRoles{types.RoleNode},
			JoinMethod: types.JoinMethodTPM,
			TPM: &types.ProvisionTokenSpecV2TPM{
				Allow: []*types.ProvisionTokenSpecV2TPM_Rule{
					{
						Description:  "worker-1",
						EKPublicHash: "sha256:abc",
					},
				},
				EKCertAllowedCAs: []string{testCert},
			},
		})
		require.NoError(t, err)
		metadata := tt.GetMetadata()
		metadata.Labels = map[string]string{"env": "prod"}
		tt.SetMetadata(metadata)

		result := fromTeleportToken(tt)
		assert.Equal(t, "tpm-token", result.Name)
		assert.Equal(t, JoinMethodTPM, result.JoinMethod)
		assert.Equal(t, "prod", result.Labels["env"])

		tpmCfg, ok := result.Spec.(*TPMConfig)
		require.True(t, ok)
		assert.Len(t, tpmCfg.Allow, 1)
		assert.Equal(t, "worker-1", tpmCfg.Allow[0].Description)
		assert.Equal(t, "sha256:abc", tpmCfg.Allow[0].EKPublicHash)
		assert.Len(t, tpmCfg.EKCertAllowedCAs, 1)
	})

	t.Run("token with bot name", func(t *testing.T) {
		tt, err := types.NewProvisionTokenFromSpec("bot-token", expiry, types.ProvisionTokenSpecV2{
			Roles:      types.SystemRoles{types.RoleBot},
			JoinMethod: types.JoinMethodTPM,
			BotName:    "my-bot",
			TPM: &types.ProvisionTokenSpecV2TPM{
				Allow: []*types.ProvisionTokenSpecV2TPM_Rule{
					{Description: "bot-rule", EKPublicHash: "sha256:bot"},
				},
			},
		})
		require.NoError(t, err)

		result := fromTeleportToken(tt)
		assert.Equal(t, "my-bot", result.BotName)
		assert.Contains(t, result.Roles, "Bot")
	})

	t.Run("token with nil labels gets empty map", func(t *testing.T) {
		tt, err := types.NewProvisionTokenFromSpec("no-labels", expiry, types.ProvisionTokenSpecV2{
			Roles:      types.SystemRoles{types.RoleNode},
			JoinMethod: types.JoinMethodTPM,
			TPM: &types.ProvisionTokenSpecV2TPM{
				Allow: []*types.ProvisionTokenSpecV2TPM_Rule{
					{Description: "rule", EKPublicHash: "sha256:hash"},
				},
			},
		})
		require.NoError(t, err)

		result := fromTeleportToken(tt)
		assert.NotNil(t, result.Labels)
		assert.Empty(t, result.Labels)
	})
}

func TestFromTeleportNode(t *testing.T) {
	t.Run("node with all fields", func(t *testing.T) {
		server, err := types.NewServer("test-node", types.KindNode, types.ServerSpecV2{
			Addr:     "10.0.0.1:3022",
			Hostname: "test-node.example.com",
		})
		require.NoError(t, err)
		server.SetStaticLabels(map[string]string{
			"role":           "worker",
			"resource-group": "nke",
		})

		result := fromTeleportNode(server)
		assert.Equal(t, "test-node", result.Name)
		assert.Equal(t, "test-node.example.com", result.Hostname)
		assert.Equal(t, "10.0.0.1:3022", result.Addr)
		assert.Equal(t, "worker", result.Labels["role"])
		assert.Equal(t, "nke", result.Labels["resource-group"])
	})

	t.Run("node with no labels gets empty map", func(t *testing.T) {
		server, err := types.NewServer("minimal-node", types.KindNode, types.ServerSpecV2{
			Addr:     "10.0.0.2:3022",
			Hostname: "minimal.example.com",
		})
		require.NoError(t, err)

		result := fromTeleportNode(server)
		assert.NotNil(t, result.Labels)
	})
}

func TestTPMConfigConversion(t *testing.T) {
	t.Run("toTeleportTPMConfig", func(t *testing.T) {
		cfg := &TPMConfig{
			Allow: []TPMRule{
				{
					Description:         "rule-1",
					EKPublicHash:        "sha256:hash1",
					EKCertificateSerial: "11:22:33",
				},
				{
					Description:  "rule-2",
					EKPublicHash: "sha256:hash2",
				},
			},
			EKCertAllowedCAs: []string{"CA1", "CA2"},
		}

		result := toTeleportTPMConfig(cfg)

		require.NotNil(t, result)
		assert.Len(t, result.Allow, 2)
		assert.Equal(t, "rule-1", result.Allow[0].Description)
		assert.Equal(t, "sha256:hash1", result.Allow[0].EKPublicHash)
		assert.Equal(t, "11:22:33", result.Allow[0].EKCertificateSerial)
		assert.Len(t, result.EKCertAllowedCAs, 2)
	})

	t.Run("fromTeleportTPMConfig", func(t *testing.T) {
		tpmSpec := &types.ProvisionTokenSpecV2TPM{
			Allow: []*types.ProvisionTokenSpecV2TPM_Rule{
				{
					Description:         "teleport-rule",
					EKPublicHash:        "sha256:teleport",
					EKCertificateSerial: "aa:bb:cc",
				},
			},
			EKCertAllowedCAs: []string{"TeleportCA"},
		}

		result := fromTeleportTPMConfig(tpmSpec)

		require.NotNil(t, result)
		assert.Len(t, result.Allow, 1)
		assert.Equal(t, "teleport-rule", result.Allow[0].Description)
		assert.Equal(t, "sha256:teleport", result.Allow[0].EKPublicHash)
		assert.Equal(t, "aa:bb:cc", result.Allow[0].EKCertificateSerial)
		assert.Len(t, result.EKCertAllowedCAs, 1)
	})

	t.Run("roundtrip conversion preserves data", func(t *testing.T) {
		original := &TPMConfig{
			Allow: []TPMRule{
				{
					Description:         "roundtrip-test",
					EKPublicHash:        "sha256:roundtrip",
					EKCertificateSerial: "ff:ee:dd",
				},
			},
			EKCertAllowedCAs: []string{"RoundtripCA"},
		}

		teleportCfg := toTeleportTPMConfig(original)
		result := fromTeleportTPMConfig(teleportCfg)

		assert.Equal(t, original.Allow[0].Description, result.Allow[0].Description)
		assert.Equal(t, original.Allow[0].EKPublicHash, result.Allow[0].EKPublicHash)
		assert.Equal(t, original.Allow[0].EKCertificateSerial, result.Allow[0].EKCertificateSerial)
		assert.Equal(t, original.EKCertAllowedCAs, result.EKCertAllowedCAs)
	})
}
