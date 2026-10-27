// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
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
// enablement workflow. Keep this explicit so an agent default change cannot
// silently change controller behavior.
const (
	configmaps = "configmaps"
	secrets    = "secrets"
)

func DefaultObjectsToEncrypt() []string {
	return []string{
		secrets,
		configmaps,
		"persistentvolumeclaims",
		"persistentvolumes",
		"serviceaccounts",
		"events",
		"services",
	}
}

type phase string

const (
	// updateEncryptionConfigAttempts bounds the rotation's UpdateEncryptionConfig
	// calls per node when the agent reports a retry-safe code. Each attempt can
	// wait about two minutes for the reload, so all of them fit in the
	// eight-minute ApplyToNode activity timeout.
	updateEncryptionConfigAttempts       = 3
	defaultUpdateEncryptionConfigBackoff = 5 * time.Second
)

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
	updateRetryBackoff    time.Duration
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
	op := &EncryptionOperation{name: name, phase: operationPhase, updateRetryBackoff: defaultUpdateEncryptionConfigBackoff}
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

	key := EncryptionKey{}
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
	// Only a rotation staged by an earlier controller has an unnamed pending
	// key. Its agents may predate the identity RPC, so it keeps the original
	// unnamed requests: the agent auto-names the new key and later phases match
	// the key bytes.
	if op.isRotation() && key.Name != "" {
		if err := verifyRotationKeyIdentity(ctx, client, &key); err != nil {
			return rollout.OutcomeFailedNotRestored, err
		}
	}
	if err := op.runEncryptionPhase(ctx, client, artifact, key); err != nil {
		if op.isRotation() {
			// An upstream validation error may echo the secret-bearing request.
			// Do not serialize that error into Temporal activity failures.
			return rollout.OutcomeFailedNotRestored, fmt.Errorf("%s on node %s failed; inspect node encryption health", op.name, node.Name())
		}
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("%s on node %s: %w", op.name, node.Name(), err)
	}
	return rollout.OutcomeApplied, nil
}

func (op *EncryptionOperation) runEncryptionPhase(ctx context.Context, client nodeAgentEncryptionClient, artifact encryptionArtifact, key EncryptionKey) error {
	switch op.phase {
	case phaseInitialize:
		resp, err := client.InitializeKubernetesAPIEncryption(ctx, &nvkeproto.InitializeKubernetesAPIEncryptionRequest{
			EncryptionKey: key.Key, EncryptionKeyName: key.Name, ObjectsToEncrypt: artifact.ObjectsToEncrypt,
		})
		if err != nil {
			return fmt.Errorf("initialize kubernetes API encryption: %w", err)
		}
		if msg := resp.GetErrorMessage(); msg != "" {
			return fmt.Errorf("initialize kubernetes API encryption: %s", msg)
		}
	case phasePromote:
		if _, err := client.PromoteKubernetesAPIEncryptionProviders(ctx, &nvkeproto.PromoteKubernetesAPIEncryptionProvidersRequest{}); err != nil {
			return fmt.Errorf("promote kubernetes API encryption providers: %w", err)
		}
	case phaseRemove:
		resp, err := client.RemovePlainTextSecondaryEncryptionEntry(ctx, &nvkeproto.RemovePlainTextSecondaryEncryptionEntryRequest{})
		if err != nil {
			return fmt.Errorf("remove plain-text identity provider: %w", err)
		}
		if msg := resp.GetErrorMessage(); msg != "" {
			return fmt.Errorf("remove plain-text identity provider: %s", msg)
		}
	case phaseAddSecondaryKey:
		if err := op.updateEncryptionConfig(ctx, client, &nvkeproto.UpdateEncryptionConfigRequest{
			Operation: &nvkeproto.UpdateEncryptionConfigRequest_AddSecondaryKey{
				AddSecondaryKey: &nvkeproto.AddSecondaryKeyOperation{NewKey: key.Key, KeyName: key.Name},
			},
		}); err != nil {
			return fmt.Errorf("add secondary encryption key: %w", err)
		}
	case phasePromotePrimaryKey:
		if err := op.updateEncryptionConfig(ctx, client, &nvkeproto.UpdateEncryptionConfigRequest{
			Operation: &nvkeproto.UpdateEncryptionConfigRequest_PromotePrimaryKey{
				PromotePrimaryKey: &nvkeproto.PromotePrimaryKeyOperation{NewKey: key.Key, KeyName: key.Name},
			},
		}); err != nil {
			return fmt.Errorf("promote primary encryption key: %w", err)
		}
	case phaseRemoveOldEncryptionKey:
		if err := op.updateEncryptionConfig(ctx, client, &nvkeproto.UpdateEncryptionConfigRequest{
			Operation: &nvkeproto.UpdateEncryptionConfigRequest_RemoveOldKeys{
				RemoveOldKeys: &nvkeproto.RemoveOldKeysOperation{PrimaryKey: key.Key, KeyName: key.Name},
			},
		}); err != nil {
			return fmt.Errorf("remove old encryption keys: %w", err)
		}
	default:
		return fmt.Errorf("unknown etcd encryption phase %q", op.phase)
	}
	return nil
}

// updateEncryptionConfig retries the codes the RPC contract calls safe to retry,
// Unavailable and DeadlineExceeded, within a bounded budget. ApplyToNode returns
// outcomes rather than activity errors, so nothing else retries a node mutation
// before the phase fails. Every other code fails at once.
func (op *EncryptionOperation) updateEncryptionConfig(ctx context.Context, client nodeAgentEncryptionClient, req *nvkeproto.UpdateEncryptionConfigRequest) error {
	backoff := op.updateRetryBackoff
	for attempt := 1; ; attempt++ {
		_, err := client.UpdateEncryptionConfig(ctx, req)
		code := status.Code(err)
		if err == nil || attempt == updateEncryptionConfigAttempts || code != codes.Unavailable && code != codes.DeadlineExceeded {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		backoff *= 2
	}
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

func (op *EncryptionOperation) readEncryptionKey(ctx context.Context, clusterID uuid.UUID) (EncryptionKey, error) {
	if op.encryptionKeyReader == nil {
		return EncryptionKey{}, errors.New("etcd encryption operation key reader is not configured")
	}
	var key EncryptionKey
	var err error
	switch op.phase {
	case phaseInitialize:
		key, err = op.encryptionKeyReader.ReadEnablementKey(ctx, clusterID)
	case phaseAddSecondaryKey, phasePromotePrimaryKey, phaseRemoveOldEncryptionKey:
		key, err = op.encryptionKeyReader.ReadRotationKey(ctx, clusterID)
	case phasePromote, phaseRemove:
		return EncryptionKey{}, fmt.Errorf("etcd encryption phase %q does not use key material", op.phase)
	default:
		return EncryptionKey{}, fmt.Errorf("etcd encryption phase %q does not use key material", op.phase)
	}
	if err != nil {
		if op.isRotation() {
			return EncryptionKey{}, fmt.Errorf("read etcd rotation key failed; inspect Kubernetes access and pending key state")
		}
		return EncryptionKey{}, fmt.Errorf("read etcd encryption key for cluster %s: %w", clusterID, err)
	}
	if key.Key == "" {
		return EncryptionKey{}, fmt.Errorf("etcd encryption key is empty for cluster %s", clusterID)
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
		return nil, nil, fmt.Errorf("dial agent on node %s: %w", node.Name(), err)
	}
	if client == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("dial agent on node %s returned nil client", node.Name())
	}
	return client, cleanup, nil
}

func (op *EncryptionOperation) resolveDialer() (nodeAgentClientDialer, error) {
	if op.nodeAgentClientDialer == nil {
		return nil, errors.New("etcd encryption operation agent dialer is not configured")
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

func (op *EncryptionOperation) isRotation() bool {
	return op.phase == phaseAddSecondaryKey || op.phase == phasePromotePrimaryKey || op.phase == phaseRemoveOldEncryptionKey
}

// verifyRotationKeyIdentity confirms the upgraded agent supports named keys and
// rejects conflicting live names before any mutation.
func verifyRotationKeyIdentity(ctx context.Context, client nodeAgentEncryptionClient, key *EncryptionKey) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sum := sha256.Sum256([]byte(key.Key))
	resp, err := client.GetEtcdEncryptionKeyIdentity(ctx, &nvkeproto.GetEtcdEncryptionKeyIdentityRequest{KeySha256: hex.EncodeToString(sum[:])})
	if err != nil {
		return errors.New("cannot prove rotation key identity; check connectivity and upgrade older node agents")
	}
	if name := resp.GetKeyName(); name != "" && name != key.Name {
		return errors.New("pending encryption storage identity conflicts with the node configuration")
	}
	return nil
}
