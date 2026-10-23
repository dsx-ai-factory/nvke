// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"errors"
	"fmt"
	"log/slog"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// ---------------------------------------------------------------------------
// EncryptionConfiguration types — Kubernetes apiserver encryption at rest
// ---------------------------------------------------------------------------

type (
	EncryptionConfiguration struct {
		ApiVersion string           `json:"apiVersion" yaml:"apiVersion"`
		Kind       string           `json:"kind"       yaml:"kind"`
		Resources  []ResourceConfig `json:"resources"  yaml:"resources"`
	}

	ResourceConfig struct {
		Resources []string         `json:"resources" yaml:"resources"`
		Providers []ProviderConfig `json:"providers" yaml:"providers"`
	}

	ProviderConfig struct {
		Secretbox *SecretboxConfig `json:"secretbox,omitempty" yaml:"secretbox,omitempty"`
		KMS       *KMSConfig       `json:"kms,omitempty"       yaml:"kms,omitempty"`
		Identity  *IdentityConfig  `json:"identity,omitempty"  yaml:"identity,omitempty"`
	}

	SecretboxConfig struct {
		Keys []KeyConfig `json:"keys" yaml:"keys"`
	}

	KMSConfig struct {
		Name      string `json:"name"                yaml:"name"`
		Endpoint  string `json:"endpoint"            yaml:"endpoint"`
		Timeout   string `json:"timeout,omitempty"   yaml:"timeout,omitempty"`
		Cachesize int    `json:"cachesize,omitempty" yaml:"cachesize,omitempty"`
	}

	IdentityConfig struct{}

	KeyConfig struct {
		Name   string `json:"name"   yaml:"name"`
		Secret string `json:"secret" yaml:"secret"`
	}
)

// ---------------------------------------------------------------------------
// EncryptionConfiguration methods — key access and mutation
// ---------------------------------------------------------------------------

// ValidateSecretbox checks that at least one secretbox provider exists and all
// providers have consistent keys (same count, same secrets, same order).
func (c *EncryptionConfiguration) ValidateSecretbox() error {
	slices := c.secretboxKeySlices()
	if len(slices) == 0 {
		return errors.New("no secretbox provider found in encryption config")
	}
	first := *slices[0]
	for _, keys := range slices[1:] {
		if len(*keys) != len(first) {
			return errors.New("secretbox providers are out of sync across resource groups; manual reconciliation required before key rotation")
		}
		for i, k := range *keys {
			if k.Secret != first[i].Secret {
				return errors.New("secretbox providers are out of sync across resource groups; manual reconciliation required before key rotation")
			}
		}
	}
	return nil
}

// secretboxKeySlices returns pointers to the key slice of every secretbox
// provider in the config, across all resource groups. Mutations through these
// pointers are reflected in the config that will be marshaled to disk.
func (c *EncryptionConfiguration) secretboxKeySlices() []*[]KeyConfig {
	var result []*[]KeyConfig
	for i := range c.Resources {
		for j := range c.Resources[i].Providers {
			if c.Resources[i].Providers[j].Secretbox != nil {
				result = append(result, &c.Resources[i].Providers[j].Secretbox.Keys)
			}
		}
	}
	return result
}

// PrimaryKey returns the first secretbox key (the one used for new encryptions).
// Returns zero value and false if no secretbox keys exist.
func (c *EncryptionConfiguration) PrimaryKey() (KeyConfig, bool) {
	slices := c.secretboxKeySlices()
	if len(slices) == 0 || len(*slices[0]) == 0 {
		return KeyConfig{}, false
	}
	return (*slices[0])[0], true
}

// KeyCount returns the number of keys in the first secretbox provider.
func (c *EncryptionConfiguration) KeyCount() int {
	slices := c.secretboxKeySlices()
	if len(slices) == 0 {
		return 0
	}
	return len(*slices[0])
}

// HasKey reports whether the given secret exists in the secretbox keys.
func (c *EncryptionConfiguration) HasKey(secret string) bool {
	slices := c.secretboxKeySlices()
	if len(slices) == 0 {
		return false
	}
	return findKeyIdx(*slices[0], secret) >= 0
}

// IsInDesiredState reports whether the config is already in the state that
// the requested operation would produce.
func (c *EncryptionConfiguration) IsInDesiredState(req *pb.UpdateEncryptionConfigRequest) bool {
	primary, hasPrimary := c.PrimaryKey()
	switch op := req.Operation.(type) {
	case *pb.UpdateEncryptionConfigRequest_AddSecondaryKey:
		return c.HasKey(op.AddSecondaryKey.GetNewKey())
	case *pb.UpdateEncryptionConfigRequest_PromotePrimaryKey:
		return hasPrimary && primary.Secret == op.PromotePrimaryKey.GetNewKey()
	case *pb.UpdateEncryptionConfigRequest_RemoveOldKeys:
		return c.KeyCount() == 1 && hasPrimary && primary.Secret == op.RemoveOldKeys.GetPrimaryKey()
	default:
		return false
	}
}

// ValidateMutation checks preconditions for the mutation that cannot be
// detected from IsInDesiredState alone. Must be called before ApplyMutation
// (and before any writes) so that invalid requests are rejected without
// leaving partial state on disk.
func (c *EncryptionConfiguration) ValidateMutation(req *pb.UpdateEncryptionConfigRequest) error {
	primary, _ := c.PrimaryKey()
	switch op := req.Operation.(type) {
	case *pb.UpdateEncryptionConfigRequest_PromotePrimaryKey:
		if !c.HasKey(op.PromotePrimaryKey.GetNewKey()) {
			return errors.New("key not found in encryption config; add it as secondary first")
		}
	case *pb.UpdateEncryptionConfigRequest_RemoveOldKeys:
		primaryKey := op.RemoveOldKeys.GetPrimaryKey()
		if !c.HasKey(primaryKey) {
			return errors.New("primary_key not found in encryption config")
		}
		if primary.Secret != primaryKey {
			return errors.New("primary_key must be the current primary (position 0); call PromotePrimaryKey first")
		}
	}
	return nil
}

// SecretboxPrimaryIdentitySecondary reports whether every resource group uses
// secretbox as the first provider and identity as the second (greenfield or
// post-promotion layout).
func (c *EncryptionConfiguration) SecretboxPrimaryIdentitySecondary() bool {
	for i := range c.Resources {
		p := c.Resources[i].Providers
		if len(p) < 2 {
			return false
		}
		if p[0].Secretbox == nil || p[1].Identity == nil {
			return false
		}
	}
	return len(c.Resources) > 0
}

// IdentityPrimarySecretboxSecondary reports whether every resource group uses
// identity first and secretbox second (existing-cluster bootstrap phase).
func (c *EncryptionConfiguration) IdentityPrimarySecretboxSecondary() bool {
	for i := range c.Resources {
		p := c.Resources[i].Providers
		if len(p) < 2 {
			return false
		}
		if p[0].Identity == nil || p[1].Secretbox == nil {
			return false
		}
	}
	return len(c.Resources) > 0
}

// PromoteSecretboxBeforeIdentity reorders providers from identity-then-secretbox
// to secretbox-then-identity for each resource group. No-op for groups that
// already have secretbox first. Returns an error if a group cannot be promoted.
func (c *EncryptionConfiguration) PromoteSecretboxBeforeIdentity() error {
	for i := range c.Resources {
		p := &c.Resources[i].Providers
		if len(*p) < 2 {
			return errors.New("encryption config needs at least two providers per resource group")
		}
		first := (*p)[0]
		second := (*p)[1]
		if first.Secretbox != nil {
			continue
		}
		if first.Identity == nil || second.Secretbox == nil {
			return errors.New("expected identity then secretbox as first two providers for migration promote")
		}
		newP := make([]ProviderConfig, 0, len(*p))
		newP = append(newP, second, first)
		if len(*p) > 2 {
			newP = append(newP, (*p)[2:]...)
		}
		*p = newP
	}
	return nil
}

// ApplyMutation mutates all secretbox providers according to req.
// Callers must have already called ValidateMutation and verified
// the config is NOT in the desired state.
func (c *EncryptionConfiguration) ApplyMutation(req *pb.UpdateEncryptionConfigRequest) {
	slices := c.secretboxKeySlices()
	switch op := req.Operation.(type) {
	case *pb.UpdateEncryptionConfigRequest_AddSecondaryKey:
		newKey := op.AddSecondaryKey.GetNewKey()
		keyName := nextKeyName(*slices[0])
		for _, keys := range slices {
			*keys = append(*keys, KeyConfig{Name: keyName, Secret: newKey})
		}
		slog.Info("added secondary encryption key", "key_name", keyName, "total_keys", len(*slices[0]))

	case *pb.UpdateEncryptionConfigRequest_PromotePrimaryKey:
		newKey := op.PromotePrimaryKey.GetNewKey()
		for _, keys := range slices {
			idx := findKeyIdx(*keys, newKey)
			if idx < 0 {
				continue
			}
			promoted := (*keys)[idx]
			newKeys := make([]KeyConfig, 0, len(*keys))
			newKeys = append(newKeys, promoted)
			newKeys = append(newKeys, (*keys)[:idx]...)
			newKeys = append(newKeys, (*keys)[idx+1:]...)
			*keys = newKeys
		}
		slog.Info("promoted key to primary", "total_keys", len(*slices[0]))

	case *pb.UpdateEncryptionConfigRequest_RemoveOldKeys:
		primaryKey := op.RemoveOldKeys.GetPrimaryKey()
		for _, keys := range slices {
			var kept []KeyConfig
			for _, k := range *keys {
				if k.Secret == primaryKey {
					kept = append(kept, k)
				}
			}
			*keys = kept
		}
		slog.Info("removed old encryption keys")
	}
}

// ---------------------------------------------------------------------------
// Key helpers
// ---------------------------------------------------------------------------

// findKeyIdx returns the index of the first KeyConfig whose Secret matches key,
// or -1 if not found.
func findKeyIdx(keys []KeyConfig, key string) int {
	for i, k := range keys {
		if k.Secret == key {
			return i
		}
	}
	return -1
}

// nextKeyName returns a key name with a suffix higher than any existing
// kube-api-encryption-key-N name, preventing collisions after key removals.
func nextKeyName(keys []KeyConfig) string {
	maxIdx := 0
	for _, k := range keys {
		var n int
		if _, err := fmt.Sscanf(k.Name, "kube-api-encryption-key-%d", &n); err == nil && n > maxIdx {
			maxIdx = n
		}
	}
	return fmt.Sprintf("kube-api-encryption-key-%d", maxIdx+1)
}
