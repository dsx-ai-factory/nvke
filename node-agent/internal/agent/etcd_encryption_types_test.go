package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptionConfiguration_IdentityPrimaryPromote(t *testing.T) {
	key := "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY="
	cfg := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []ResourceConfig{{
			Resources: []string{"secrets"},
			Providers: []ProviderConfig{
				{Identity: &IdentityConfig{}},
				{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "current-primary-encryption-key", Secret: key}}}},
			},
		}},
	}
	require.NoError(t, cfg.ValidateSecretbox())
	assert.True(t, cfg.IdentityPrimarySecretboxSecondary())
	assert.False(t, cfg.SecretboxPrimaryIdentitySecondary())

	require.NoError(t, cfg.PromoteSecretboxBeforeIdentity())
	assert.True(t, cfg.SecretboxPrimaryIdentitySecondary())
	assert.False(t, cfg.IdentityPrimarySecretboxSecondary())
	require.NoError(t, cfg.ValidateSecretbox())
}

func TestEncryptionConfiguration_PromoteSecretboxBeforeIdentity_idempotent(t *testing.T) {
	key := "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY="
	cfg := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []ResourceConfig{{
			Resources: []string{"secrets"},
			Providers: []ProviderConfig{
				{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "current-primary-encryption-key", Secret: key}}}},
				{Identity: &IdentityConfig{}},
			},
		}},
	}
	require.NoError(t, cfg.ValidateSecretbox())
	assert.True(t, cfg.SecretboxPrimaryIdentitySecondary())
	require.NoError(t, cfg.PromoteSecretboxBeforeIdentity())
	assert.True(t, cfg.SecretboxPrimaryIdentitySecondary())
}

func TestEncryptionConfiguration_PromoteSecretboxBeforeIdentity_multiResource(t *testing.T) {
	key := "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY="
	cfg := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []ResourceConfig{
			{
				Resources: []string{"secrets"},
				Providers: []ProviderConfig{
					{Identity: &IdentityConfig{}},
					{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "current-primary-encryption-key", Secret: key}}}},
				},
			},
			{
				Resources: []string{"configmaps"},
				Providers: []ProviderConfig{
					{Identity: &IdentityConfig{}},
					{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "current-primary-encryption-key", Secret: key}}}},
				},
			},
		},
	}
	require.NoError(t, cfg.ValidateSecretbox())
	require.NoError(t, cfg.PromoteSecretboxBeforeIdentity())
	assert.True(t, cfg.SecretboxPrimaryIdentitySecondary())
}
