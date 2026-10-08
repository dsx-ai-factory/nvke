// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v2"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/apiserver/pkg/storage/value/encrypt/secretbox"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

func storageTransformer(t *testing.T, key KeyConfig) value.Transformer {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(key.Secret)
	require.NoError(t, err)
	var secret [32]byte
	copy(secret[:], decoded)
	return value.NewPrefixTransformers(nil, value.PrefixTransformer{
		Prefix: []byte("k8s:enc:secretbox:v1:" + key.Name + ":"), Transformer: secretbox.NewSecretboxTransformer(secret),
	})
}

func TestRotationThenBootstrapPreservesStorageIdentity(t *testing.T) {
	for _, bootstrap := range []string{"join", "init"} {
		t.Run(bootstrap, func(t *testing.T) {
			existing := &NodeAgentServer{RootPath: t.TempDir()}
			require.NoError(t, existing.createAPIEncryptionConfig(testKey1, false, nil))
			cfg, err := existing.readEncryptionConfig()
			require.NoError(t, err)
			// Two rotations exercise durable identities beyond the first auto-name.
			for i, secret := range []string{testKey2, testKey1} {
				name := []string{"rotation-name-one", "rotation-name-two"}[i]
				requests := []*pb.UpdateEncryptionConfigRequest{
					{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: secret, KeyName: name}}},
					{Operation: &pb.UpdateEncryptionConfigRequest_PromotePrimaryKey{PromotePrimaryKey: &pb.PromotePrimaryKeyOperation{NewKey: secret, KeyName: name}}},
					{Operation: &pb.UpdateEncryptionConfigRequest_RemoveOldKeys{RemoveOldKeys: &pb.RemoveOldKeysOperation{PrimaryKey: secret, KeyName: name}}},
				}
				for _, req := range requests {
					require.NoError(t, cfg.ValidateMutation(req))
					cfg.ApplyMutation(req)
				}
				deployed, ok := cfg.PrimaryKey()
				require.True(t, ok)
				fresh := newKubeadmFileTestServer(t)
				fresh.NodeStatus = &NodeStatus{Role: ControlPlaneRole, Status: PendingStatus}
				if bootstrap == "join" {
					fresh.Runner = &MockRunner{Output: []byte("joined")}
					fresh.processKubeadmJoin([]string{"join", "192.0.2.1:6443"}, &pb.KubeadmJoinRequest{ApiEncryptionKey: secret, ApiEncryptionKeyName: name})
				} else {
					fresh.Runner = &MockRunner{Output: []byte(initOutput)}
					fresh.processKubeadmInit([]string{"init", "--upload-certs"}, &pb.KubeadmInitRequest{ApiEncryptionEnabled: true, ApiEncryptionKey: secret, ApiEncryptionKeyName: name, NoCniInstall: true})
				}
				require.Equal(t, ReadyStatus, fresh.NodeStatus.Status)
				joinedCfg, err := fresh.readEncryptionConfig()
				require.NoError(t, err)
				joined, ok := joinedCfg.PrimaryKey()
				require.True(t, ok)
				require.Equal(t, deployed.Name, joined.Name)
				for _, pair := range [][2]KeyConfig{{deployed, joined}, {joined, deployed}} {
					ctx, dataCtx := context.Background(), value.DefaultContext("/registry/secrets/default/fixture")
					stored, err := storageTransformer(t, pair[0]).TransformToStorage(ctx, []byte("public-fixture"), dataCtx)
					require.NoError(t, err)
					plain, _, err := storageTransformer(t, pair[1]).TransformFromStorage(ctx, stored, dataCtx)
					require.NoError(t, err)
					require.Equal(t, "public-fixture", string(plain))
				}
			}
		})
	}
}

func TestEncryptionIdentityDiscoveryAndRenameRejection(t *testing.T) {
	n := &NodeAgentServer{RootPath: t.TempDir(), NodeStatus: &NodeStatus{Role: ControlPlaneRole}}
	const deployedName = "kube-api-encryption-key-7"
	require.NoError(t, n.createNamedAPIEncryptionConfig(testKey1, deployedName, false, nil))
	resp, err := n.GetEtcdEncryptionKeyIdentity(context.Background(), &pb.GetEtcdEncryptionKeyIdentityRequest{KeySha256: encryptionKeyDigest(testKey1)})
	require.NoError(t, err)
	require.Equal(t, deployedName, resp.KeyName)
	resp, err = n.GetEtcdEncryptionKeyIdentity(context.Background(), &pb.GetEtcdEncryptionKeyIdentityRequest{KeySha256: encryptionKeyDigest(testKey2)})
	require.NoError(t, err)
	require.Empty(t, resp.KeyName)
	require.ErrorContains(t, n.createNamedAPIEncryptionConfig(testKey1, legacyAPIEncryptionKeyName, false, nil), "renaming a storage key is unsafe")
	cfg, err := n.readEncryptionConfig()
	require.NoError(t, err)
	primary, _ := cfg.PrimaryKey()
	require.Equal(t, deployedName, primary.Name)
	for _, req := range []*pb.UpdateEncryptionConfigRequest{
		{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey1, KeyName: "different-name"}}},
		{Operation: &pb.UpdateEncryptionConfigRequest_PromotePrimaryKey{PromotePrimaryKey: &pb.PromotePrimaryKeyOperation{NewKey: testKey1, KeyName: "different-name"}}},
		{Operation: &pb.UpdateEncryptionConfigRequest_RemoveOldKeys{RemoveOldKeys: &pb.RemoveOldKeysOperation{PrimaryKey: testKey1, KeyName: "different-name"}}},
		{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2, KeyName: deployedName}}},
	} {
		require.False(t, cfg.IsInDesiredState(req))
		require.ErrorContains(t, cfg.ValidateMutation(req), "identity conflicts")
		_, err := n.tryCommitEncryptionConfigUpdate(context.Background(), req, n.encRotationWALPath())
		require.ErrorContains(t, err, "identity conflicts")
	}
	// Resource groups with the same bytes but different names are incompatible.
	cfg.Resources = append(cfg.Resources, ResourceConfig{Resources: []string{"configmaps"}, Providers: []ProviderConfig{{Secretbox: &SecretboxConfig{Keys: []KeyConfig{{Name: "different-name", Secret: testKey1}}}}}})
	require.Error(t, cfg.ValidateSecretbox())
	encoded, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(n.RootPath, "etc/kubernetes/kube-api-encryption/encryption-config.yaml"), encoded, 0o600))
	_, err = n.GetEtcdEncryptionKeyIdentity(context.Background(), &pb.GetEtcdEncryptionKeyIdentityRequest{KeySha256: encryptionKeyDigest(testKey1)})
	require.ErrorContains(t, err, "identities are inconsistent")
}

func TestLegacyRotationRequestsPreserveTheirStorageNamesAndWALIdentity(t *testing.T) {
	n := &NodeAgentServer{RootPath: t.TempDir()}
	require.NoError(t, n.createAPIEncryptionConfig(testKey1, false, nil))
	cfg, err := n.readEncryptionConfig()
	require.NoError(t, err)
	legacy := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2}}}
	require.NoError(t, cfg.ValidateMutation(legacy))
	cfg.ApplyMutation(legacy)
	keys := *cfg.secretboxKeySlices()[0]
	require.Equal(t, "kube-api-encryption-key-1", keys[1].Name)
	named := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2, KeyName: keys[1].Name}}}
	require.True(t, cfg.IsInDesiredState(named))
	legacyID, err := encRotationOpID(legacy)
	require.NoError(t, err)
	namedID, err := encRotationOpID(named)
	require.NoError(t, err)
	require.Equal(t, legacyID, namedID)
}
