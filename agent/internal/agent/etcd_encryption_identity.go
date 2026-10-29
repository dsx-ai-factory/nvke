// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/hex"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const legacyAPIEncryptionKeyName = "current-primary-encryption-key"

func validateEncryptionKeyName(name string) error {
	if len(name) == 0 || len(name) > 253 {
		return errors.New("encryption key name must contain 1 to 253 characters")
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return errors.New("encryption key name must contain only letters, digits, dots, underscores or hyphens")
	}
	return nil
}

func keyNameMatches(actual, requested string) bool { return requested == "" || actual == requested }

func (c *EncryptionConfiguration) hasNamedKey(secret, name string) bool {
	slices := c.secretboxKeySlices()
	if len(slices) == 0 {
		return false
	}
	for _, key := range *slices[0] {
		if key.Secret == secret && keyNameMatches(key.Name, name) {
			return true
		}
	}
	return false
}

func (c *EncryptionConfiguration) validateMutationIdentity(req *pb.UpdateEncryptionConfigRequest) error {
	var secret, name string
	switch op := req.Operation.(type) {
	case *pb.UpdateEncryptionConfigRequest_AddSecondaryKey:
		secret, name = op.AddSecondaryKey.GetNewKey(), op.AddSecondaryKey.GetKeyName()
	case *pb.UpdateEncryptionConfigRequest_PromotePrimaryKey:
		secret, name = op.PromotePrimaryKey.GetNewKey(), op.PromotePrimaryKey.GetKeyName()
	case *pb.UpdateEncryptionConfigRequest_RemoveOldKeys:
		secret, name = op.RemoveOldKeys.GetPrimaryKey(), op.RemoveOldKeys.GetKeyName()
	}
	if name == "" { // Legacy requests retain the existing storage identity.
		return nil
	}
	if err := validateEncryptionKeyName(name); err != nil {
		return err
	}
	for _, keys := range c.secretboxKeySlices() {
		for _, key := range *keys {
			if key.Secret == secret && key.Name != name || key.Name == name && key.Secret != secret {
				return errors.New("encryption key identity conflicts with the live configuration; renaming a storage key is unsafe")
			}
		}
	}
	return nil
}

// GetEtcdEncryptionKeyIdentity reports a key's persisted storage identity. It
// returns no key material and does not mutate the configuration or its ordering.
func (n *NodeAgentServer) GetEtcdEncryptionKeyIdentity(ctx context.Context, req *pb.GetEtcdEncryptionKeyIdentityRequest) (*pb.GetEtcdEncryptionKeyIdentityResponse, error) {
	defer logRPCSpan(ctx, "GetEtcdEncryptionKeyIdentity")()
	if n.NodeStatus.Role != ControlPlaneRole {
		return nil, status.Error(codes.FailedPrecondition, "encryption key identity requires a control-plane node")
	}
	digest, err := hex.DecodeString(req.GetKeySha256())
	if err != nil || len(digest) != 32 {
		return nil, status.Error(codes.InvalidArgument, "key_sha256 must be a SHA-256 hex digest")
	}
	n.encryptionMu.Lock()
	defer n.encryptionMu.Unlock()
	cfg, err := n.readEncryptionConfig()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "cannot read encryption configuration")
	}
	if err := cfg.ValidateSecretbox(); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "encryption storage identities are inconsistent")
	}
	response := &pb.GetEtcdEncryptionKeyIdentityResponse{}
	for _, keys := range cfg.secretboxKeySlices() {
		for _, key := range *keys {
			if encryptionKeyDigest(key.Secret) != hex.EncodeToString(digest) {
				continue
			}
			if validateEncryptionKeyName(key.Name) != nil || response.KeyName != "" && response.KeyName != key.Name {
				return nil, status.Error(codes.FailedPrecondition, "encryption storage identities are inconsistent")
			}
			response.KeyName = key.Name
		}
	}
	return response, nil
}
