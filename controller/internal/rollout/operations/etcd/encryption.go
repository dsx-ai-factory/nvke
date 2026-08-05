// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
)

const (
	OperationNameInitializeKubernetesAPIEncryption       = "etcd-encryption.initialize-kubernetes-api-encryption"
	OperationNamePromoteKubernetesAPIEncryptionProviders = "etcd-encryption.promote-kubernetes-api-encryption-providers"
	OperationNameRemovePlainTextSecondaryEncryptionEntry = "etcd-encryption.remove-plain-text-secondary-encryption-entry"
	OperationNameAddSecondaryEncryptionKey               = "etcd-encryption.add-secondary-encryption-key"
	OperationNamePromotePrimaryEncryptionKey             = "etcd-encryption.promote-primary-encryption-key"
	OperationNameRemoveOldEncryptionKeys                 = "etcd-encryption.remove-old-encryption-keys"
)

// DefaultObjectsToEncrypt returns the resource list used by the brownfield
// enablement workflow. Keep this explicit so a node-agent default change cannot
// silently change CWE behavior.
func DefaultObjectsToEncrypt() []string {
	return []string{
		"secrets",
		"configmaps",
		"persistentvolumeclaims",
		"persistentvolumes",
		"serviceaccounts",
		"events",
		"services",
	}
}

type phase string

const (
	phaseInitialize             phase = "initialize"
	phasePromote                phase = "promote"
	phaseRemove                 phase = "remove"
	phaseAddSecondaryKey        phase = "add-secondary-key"
	phasePromotePrimaryKey      phase = "promote-primary-key"
	phaseRemoveOldEncryptionKey phase = "remove-old-encryption-keys"
)

// EncryptionOperation implements the per-node etcd enablement and key-rotation
// mutations. The payload contains only non-secret rollout context; operations
// read key material inside their activities so it never enters Temporal history.
type EncryptionOperation struct {
	name                  string
	phase                 phase
	objectsToEncrypt      []string
	nodeAgentClientDialer nodeAgentClientDialer
	encryptionKeyReader   EncryptionKeyReader
}

var (
	_ rollout.Operation                 = (*EncryptionOperation)(nil)
	_ rollout.Validator                 = (*EncryptionOperation)(nil)
	_ rollout.ApplyToNodePayloadBuilder = (*EncryptionOperation)(nil)
)

func NewInitializeOperation(opts ...Option) *EncryptionOperation {
	return newEncryptionOperation(OperationNameInitializeKubernetesAPIEncryption, phaseInitialize, opts...)
}

func NewPromoteProvidersOperation(opts ...Option) *EncryptionOperation {
	return newEncryptionOperation(OperationNamePromoteKubernetesAPIEncryptionProviders, phasePromote, opts...)
}

func NewRemovePlainTextSecondaryOperation(opts ...Option) *EncryptionOperation {
	return newEncryptionOperation(OperationNameRemovePlainTextSecondaryEncryptionEntry, phaseRemove, opts...)
}

func NewAddSecondaryKeyOperation(opts ...Option) *EncryptionOperation {
	return newEncryptionOperation(OperationNameAddSecondaryEncryptionKey, phaseAddSecondaryKey, opts...)
}

func NewPromotePrimaryKeyOperation(opts ...Option) *EncryptionOperation {
	return newEncryptionOperation(OperationNamePromotePrimaryEncryptionKey, phasePromotePrimaryKey, opts...)
}

func NewRemoveOldKeysOperation(opts ...Option) *EncryptionOperation {
	return newEncryptionOperation(OperationNameRemoveOldEncryptionKeys, phaseRemoveOldEncryptionKey, opts...)
}

func newEncryptionOperation(name string, operationPhase phase, opts ...Option) *EncryptionOperation {
	op := &EncryptionOperation{name: name, phase: operationPhase}
	for _, opt := range opts {
		opt(op)
	}
	return op
}

func (op *EncryptionOperation) Name() string { return op.name }

func (op *EncryptionOperation) ApplyToNode(ctx context.Context, node rollout.Node, payload rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	if node == nil {
		return rollout.OutcomeFailedRestored, errors.New("node is nil")
	}
	if node.Role() != rollout.NodeRoleControlPlane {
		return rollout.OutcomeFailedRestored, fmt.Errorf("node %s has role %q; etcd encryption only supports control-plane nodes", node.Name(), node.Role())
	}
	artifact, err := parsePayload(payload)
	if err != nil {
		return rollout.OutcomeFailedRestored, err
	}
	if artifact.ClusterID == uuid.Nil {
		return rollout.OutcomeFailedRestored, errors.New("etcd encryption artifact is missing clusterId")
	}

	key := ""
	if op.requiresEncryptionKey() {
		key, err = op.readEncryptionKey(ctx, artifact.ClusterID)
		if err != nil {
			return rollout.OutcomeFailedRestored, err
		}
	}

	client, cleanup, err := op.dial(ctx, node)
	if err != nil {
		return rollout.OutcomeFailedRestored, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if err := op.runEncryptionPhase(ctx, client, artifact, key); err != nil {
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("%s on node %s: %w", op.name, node.Name(), err)
	}
	return rollout.OutcomeApplied, nil
}

func (op *EncryptionOperation) runEncryptionPhase(ctx context.Context, client nodeAgentEncryptionClient, artifact encryptionArtifact, key string) error {
	switch op.phase {
	case phaseInitialize:
		resp, err := client.InitializeKubernetesAPIEncryption(ctx, &nkeproto.InitializeKubernetesAPIEncryptionRequest{
			EncryptionKey: key, ObjectsToEncrypt: artifact.ObjectsToEncrypt,
		})
		if err != nil {
			return fmt.Errorf("initialize kubernetes API encryption: %w", err)
		}
		if msg := resp.GetErrorMessage(); msg != "" {
			return fmt.Errorf("initialize kubernetes API encryption: %s", msg)
		}
	case phasePromote:
		if _, err := client.PromoteKubernetesAPIEncryptionProviders(ctx, &nkeproto.PromoteKubernetesAPIEncryptionProvidersRequest{}); err != nil {
			return fmt.Errorf("promote kubernetes API encryption providers: %w", err)
		}
	case phaseRemove:
		resp, err := client.RemovePlainTextSecondaryEncryptionEntry(ctx, &nkeproto.RemovePlainTextSecondaryEncryptionEntryRequest{})
		if err != nil {
			return fmt.Errorf("remove plain-text identity provider: %w", err)
		}
		if msg := resp.GetErrorMessage(); msg != "" {
			return fmt.Errorf("remove plain-text identity provider: %s", msg)
		}
	case phaseAddSecondaryKey:
		if _, err := client.UpdateEncryptionConfig(ctx, &nkeproto.UpdateEncryptionConfigRequest{
			Operation: &nkeproto.UpdateEncryptionConfigRequest_AddSecondaryKey{
				AddSecondaryKey: &nkeproto.AddSecondaryKeyOperation{NewKey: key},
			},
		}); err != nil {
			return fmt.Errorf("add secondary encryption key: %w", err)
		}
	case phasePromotePrimaryKey:
		if _, err := client.UpdateEncryptionConfig(ctx, &nkeproto.UpdateEncryptionConfigRequest{
			Operation: &nkeproto.UpdateEncryptionConfigRequest_PromotePrimaryKey{
				PromotePrimaryKey: &nkeproto.PromotePrimaryKeyOperation{NewKey: key},
			},
		}); err != nil {
			return fmt.Errorf("promote primary encryption key: %w", err)
		}
	case phaseRemoveOldEncryptionKey:
		if _, err := client.UpdateEncryptionConfig(ctx, &nkeproto.UpdateEncryptionConfigRequest{
			Operation: &nkeproto.UpdateEncryptionConfigRequest_RemoveOldKeys{
				RemoveOldKeys: &nkeproto.RemoveOldKeysOperation{PrimaryKey: key},
			},
		}); err != nil {
			return fmt.Errorf("remove old encryption keys: %w", err)
		}
	default:
		return fmt.Errorf("unknown etcd encryption phase %q", op.phase)
	}
	return nil
}

func (op *EncryptionOperation) Validate(_ context.Context, cluster rollout.Cluster) error {
	if cluster == nil {
		return errors.New("cluster is nil")
	}
	if len(cluster.ControlPlaneNodes()) == 0 {
		return fmt.Errorf("cluster %s has no control-plane nodes", cluster.Name())
	}
	if _, err := op.resolveDialer(); err != nil {
		return err
	}
	if op.requiresEncryptionKey() && op.encryptionKeyReader == nil {
		return errors.New("etcd encryption operation key reader is not configured")
	}
	if op.phase == phaseInitialize && len(op.objectsToEncrypt) == 0 {
		return errors.New("etcd encryption operation objects to encrypt are not configured")
	}
	return nil
}

func (op *EncryptionOperation) BuildApplyToNodePayload(_ context.Context, cluster rollout.Cluster) (rollout.ApplyToNodePayload, error) {
	if cluster == nil {
		return nil, errors.New("cluster is nil")
	}
	out, err := json.Marshal(encryptionArtifact{ClusterID: cluster.ID(), ObjectsToEncrypt: append([]string(nil), op.objectsToEncrypt...)})
	if err != nil {
		return nil, fmt.Errorf("marshal etcd encryption artifact: %w", err)
	}
	return rollout.ApplyToNodePayload(out), nil
}

func (op *EncryptionOperation) readEncryptionKey(ctx context.Context, clusterID uuid.UUID) (string, error) {
	if op.encryptionKeyReader == nil {
		return "", errors.New("etcd encryption operation key reader is not configured")
	}
	var key string
	var err error
	switch op.phase {
	case phaseInitialize:
		key, err = op.encryptionKeyReader.ReadEnablementKey(ctx, clusterID)
	case phaseAddSecondaryKey, phasePromotePrimaryKey, phaseRemoveOldEncryptionKey:
		key, err = op.encryptionKeyReader.ReadRotationKey(ctx, clusterID)
	case phasePromote, phaseRemove:
		return "", fmt.Errorf("etcd encryption phase %q does not use key material", op.phase)
	default:
		return "", fmt.Errorf("etcd encryption phase %q does not use key material", op.phase)
	}
	if err != nil {
		return "", fmt.Errorf("read etcd encryption key for cluster %s: %w", clusterID, err)
	}
	if key == "" {
		return "", fmt.Errorf("etcd encryption key is empty for cluster %s", clusterID)
	}
	return key, nil
}

func (op *EncryptionOperation) requiresEncryptionKey() bool {
	switch op.phase {
	case phaseInitialize, phaseAddSecondaryKey, phasePromotePrimaryKey, phaseRemoveOldEncryptionKey:
		return true
	case phasePromote, phaseRemove:
		return false
	default:
		return false
	}
}

func (op *EncryptionOperation) dial(ctx context.Context, node rollout.Node) (nodeAgentEncryptionClient, func(), error) {
	dialer, err := op.resolveDialer()
	if err != nil {
		return nil, nil, err
	}
	client, cleanup, err := dialer(ctx, node)
	if err != nil {
		return nil, nil, fmt.Errorf("dial node-agent on node %s: %w", node.Name(), err)
	}
	if client == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("dial node-agent on node %s returned nil client", node.Name())
	}
	return client, cleanup, nil
}

func (op *EncryptionOperation) resolveDialer() (nodeAgentClientDialer, error) {
	if op.nodeAgentClientDialer == nil {
		return nil, errors.New("etcd encryption operation node-agent dialer is not configured")
	}
	return op.nodeAgentClientDialer, nil
}

type encryptionArtifact struct {
	ClusterID        uuid.UUID `json:"clusterId"`
	ObjectsToEncrypt []string  `json:"objectsToEncrypt,omitempty"`
}

func parsePayload(payload rollout.ApplyToNodePayload) (encryptionArtifact, error) {
	if len(payload) == 0 {
		return encryptionArtifact{}, errors.New("etcd encryption artifact is empty")
	}
	var out encryptionArtifact
	if err := json.Unmarshal(payload, &out); err != nil {
		return encryptionArtifact{}, fmt.Errorf("unmarshal etcd encryption artifact: %w", err)
	}
	return out, nil
}
