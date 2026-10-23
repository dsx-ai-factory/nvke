// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

type nodeAgentEncryptionClient interface {
	GetEtcdEncryptionKeyIdentity(ctx context.Context, req *nvkeproto.GetEtcdEncryptionKeyIdentityRequest, opts ...grpc.CallOption) (*nvkeproto.GetEtcdEncryptionKeyIdentityResponse, error)
	InitializeKubernetesAPIEncryption(ctx context.Context, req *nvkeproto.InitializeKubernetesAPIEncryptionRequest, opts ...grpc.CallOption) (*nvkeproto.InitializeKubernetesAPIEncryptionResponse, error)
	PromoteKubernetesAPIEncryptionProviders(ctx context.Context, req *nvkeproto.PromoteKubernetesAPIEncryptionProvidersRequest, opts ...grpc.CallOption) (*nvkeproto.PromoteKubernetesAPIEncryptionProvidersResponse, error)
	RemovePlainTextSecondaryEncryptionEntry(ctx context.Context, req *nvkeproto.RemovePlainTextSecondaryEncryptionEntryRequest, opts ...grpc.CallOption) (*nvkeproto.RemovePlainTextSecondaryEncryptionEntryResponse, error)
	UpdateEncryptionConfig(ctx context.Context, req *nvkeproto.UpdateEncryptionConfigRequest, opts ...grpc.CallOption) (*nvkeproto.UpdateEncryptionConfigResponse, error)
}

type nodeAgentClientDialer func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error)

// EncryptionKey holds key material and its Kubernetes storage identity.
// It is read only inside activities and must never be placed in a rollout payload.
type EncryptionKey struct {
	Key  string
	Name string
}

// EncryptionKeyReader reads key material at activity execution time so no key
// enters Temporal workflow history. Enablement may fall back to active for
// recovery; rotation must use the distinct pending key.
type EncryptionKeyReader interface {
	ReadEnablementKey(ctx context.Context, clusterID uuid.UUID) (EncryptionKey, error)
	ReadRotationKey(ctx context.Context, clusterID uuid.UUID) (EncryptionKey, error)
}

type kubernetesEncryptionKeyReader struct {
	client    client.Client
	namespace string
}

func NewKubernetesEncryptionKeyReader(k8sClient client.Client, namespace string) EncryptionKeyReader {
	return &kubernetesEncryptionKeyReader{client: k8sClient, namespace: namespace}
}

func (r *kubernetesEncryptionKeyReader) ReadEnablementKey(ctx context.Context, clusterID uuid.UUID) (EncryptionKey, error) {
	secret, name, err := r.getSecret(ctx, clusterID)
	if err != nil {
		return EncryptionKey{}, err
	}
	if key := string(secret.Data[constants.EtcdEncryptionKeyFieldPending]); key != "" {
		return EncryptionKey{Key: key, Name: string(secret.Data[constants.EtcdEncryptionKeyFieldPendingName])}, nil
	}
	if key := string(secret.Data[constants.EtcdEncryptionKeyFieldActive]); key != "" {
		return EncryptionKey{Key: key, Name: string(secret.Data[constants.EtcdEncryptionKeyFieldActiveName])}, nil
	}
	return EncryptionKey{}, fmt.Errorf("etcd encryption key Secret %s/%s has neither pending nor active key material", r.namespace, name)
}

func (r *kubernetesEncryptionKeyReader) ReadRotationKey(ctx context.Context, clusterID uuid.UUID) (EncryptionKey, error) {
	secret, name, err := r.getSecret(ctx, clusterID)
	if err != nil {
		return EncryptionKey{}, err
	}
	if key := string(secret.Data[constants.EtcdEncryptionKeyFieldPending]); key != "" {
		return EncryptionKey{Key: key, Name: string(secret.Data[constants.EtcdEncryptionKeyFieldPendingName])}, nil
	}
	return EncryptionKey{}, fmt.Errorf("etcd encryption key Secret %s/%s has no pending rotation key", r.namespace, name)
}

func (r *kubernetesEncryptionKeyReader) getSecret(ctx context.Context, clusterID uuid.UUID) (*corev1.Secret, string, error) {
	if r.client == nil {
		return nil, "", errors.New("kubernetes client is not configured")
	}
	secret := &corev1.Secret{}
	name := constants.EtcdEncryptionKeySecretName(clusterID)
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: name}, secret); err != nil {
		return nil, name, fmt.Errorf("get etcd encryption key Secret %s/%s: %w", r.namespace, name, err)
	}
	return secret, name, nil
}
