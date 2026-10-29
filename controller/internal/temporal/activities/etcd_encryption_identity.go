// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// encryptionIdentityError is a fixed diagnostic about encryption storage
// identity. It never carries key material or upstream error text, so activities
// may return it to the workflow unredacted.
type encryptionIdentityError string

func (e encryptionIdentityError) Error() string { return string(e) }

const (
	errIdentityAgentUnsupported encryptionIdentityError = "a joined control-plane node agent predates encryption identity discovery; upgrade older node agents"
	errIdentityNoJoinedNodes    encryptionIdentityError = "no joined control-plane nodes available to prove encryption storage identity"
)

// discoverEncryptionKeyName reads each joined control-plane node. Absence is
// allowed only when adopting a legacy pending key that may not be distributed
// yet. A live identity is never guessed from the bytes or renamed.
func (a *Activities) discoverEncryptionKeyName(ctx context.Context, clusterID uuid.UUID, key string, allowAbsent bool) (string, error) {
	if a.db == nil || a.nodeAgentClient == nil {
		return "", errors.New("encryption identity discovery requires database and node agent access")
	}
	cp := true
	nodes, err := a.db.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &cp})
	if err != nil {
		return "", encryptionIdentityError("cannot list control-plane nodes for encryption identity discovery")
	}
	sum := sha256.Sum256([]byte(key))
	digest := hex.EncodeToString(sum[:])
	name, count, unsupported := "", 0, false
	for _, node := range nodes {
		if node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}
		count++
		if node.IP == "" {
			return "", encryptionIdentityError("joined control-plane node has no address for encryption identity discovery")
		}
		observed, err := a.nodeAgentClient.GetEtcdEncryptionKeyIdentity(ctx, node, digest)
		if errors.Is(err, agent.ErrEncryptionIdentityUnsupported) {
			unsupported = true // keep reading: a peer may already have proven a name
			continue
		}
		if err != nil {
			return "", encryptionIdentityError("cannot prove encryption storage identity on every control-plane node; check connectivity and upgrade older node agents")
		}
		if observed == "" {
			if allowAbsent {
				continue
			}
			return "", encryptionIdentityError("managed encryption key is absent from a joined control-plane node")
		}
		if name != "" && name != observed {
			return "", encryptionIdentityError("control-plane encryption storage identities disagree; reconcile before bootstrap or rotation")
		}
		name = observed
	}
	if count == 0 {
		return "", errIdentityNoJoinedNodes
	}
	if unsupported {
		if name != "" && name != constants.LegacyAPIEncryptionKeyName {
			return "", encryptionIdentityError("cannot prove encryption storage identity on every control-plane node; upgrade older node agents")
		}
		return "", errIdentityAgentUnsupported
	}
	return name, nil
}

// bootstrapKubeAPIEncryptionKeyName returns the storage name for kubeadm init and
// join. A Secret created before names were recorded has none, and the identity
// RPC cannot answer when no node has joined yet (init) or when every joined agent
// predates it. Pre-rotation clusters always used the legacy bootstrap name, which
// is also the only name an agent without named-key support can honor, so use it
// for this request without recording it. Identity that cannot be read for any
// other reason still fails, and a recorded name is always used as is.
func (a *Activities) bootstrapKubeAPIEncryptionKeyName(ctx context.Context, clusterID uuid.UUID) (string, error) {
	name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	if errors.Is(err, errIdentityAgentUnsupported) || errors.Is(err, errIdentityNoJoinedNodes) {
		return constants.LegacyAPIEncryptionKeyName, nil
	}
	return name, err
}

func (a *Activities) requireKubeAPIEncryptionKeyName(ctx context.Context, clusterID uuid.UUID) (string, error) {
	secret, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	if err != nil {
		return "", err
	}
	return a.adoptEncryptionKeyName(ctx, clusterID, secret, constants.EtcdEncryptionKeyFieldActive, constants.EtcdEncryptionKeyFieldActiveName)
}

func (a *Activities) adoptEncryptionKeyName(ctx context.Context, clusterID uuid.UUID, secret *corev1.Secret, keyField, nameField string) (string, error) {
	if name := string(secret.Data[nameField]); name != "" {
		return name, nil
	}
	key := string(secret.Data[keyField])
	if key == "" {
		return "", errors.New("managed encryption key is missing")
	}
	name, err := a.discoverEncryptionKeyName(ctx, clusterID, key, false)
	if err != nil {
		return "", err
	}
	secret.Data[nameField] = []byte(name)
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return "", encryptionIdentityError("cannot persist discovered encryption storage identity")
	}
	return name, nil
}
