// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
)

const (
	kubeadmStatusPending = "pending"
	kubeadmStatusReady   = "ready"
	kubeadmStatusError   = "error"
	kubeadmPhaseInit     = "initialize"
	kubeadmPhaseJoin     = "join"

	cniStatusReady = "ready"
	cniStatusError = "error"

	joinStringCacheTTL = 5 * time.Minute
)

func (a *Activities) kubeadmContainerMode(ctx context.Context, cluster db.Cluster, node db.Node) (bool, error) {
	isKubeadmContainerMode, err := a.isDockerMachineBackedNode(ctx, cluster, node)
	if err != nil {
		return false, fmt.Errorf("failed to check if node is docker machine backed: %w", err)
	}
	return isKubeadmContainerMode, nil
}

func toNodeAgentFiles(files []kubeadmconfig.File) []*nvkeproto.File {
	out := make([]*nvkeproto.File, 0, len(files))
	for _, file := range files {
		out = append(out, &nvkeproto.File{
			Path:    file.Path,
			Content: file.Content,
			Mode:    file.Mode,
			Owner:   file.Owner,
		})
	}
	return out
}

// clusterAuthentication uses the providers pinned on the cluster, never the live file.
func (a *Activities) clusterAuthentication(cluster db.Cluster) kubeadmconfig.ClusterAuthentication {
	return kubeadmconfig.ClusterAuthentication{
		ClusterName:   cluster.Name,
		Shared:        a.cfg.NvkeOIDCAuth,
		SharedEnabled: cluster.OIDCAuthEnabled,
		Providers:     db.ClusterOIDCProvidersToConfig(cluster.OIDCProviders),
		TrustDomain:   a.trustDomain,

		RBACConfigMap: a.cfg.RBACConfigMapSource(),
	}
}

func (a *Activities) ensureKubeAPIEncryptionKey(ctx context.Context, clusterID uuid.UUID) (string, error) {
	key, err := a.requireKubeAPIEncryptionKey(ctx, clusterID)
	if err == nil && key != "" {
		return key, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}

	key, err = generateEncryptionKey()
	if err != nil {
		return "", err
	}
	secretName := constants.EtcdEncryptionKeySecretName(clusterID)
	secret := &corev1.Secret{}
	lookupErr := a.k8sClient.Get(ctx, client.ObjectKey{Namespace: a.cfg.PodNamespace, Name: secretName}, secret)
	if apierrors.IsNotFound(lookupErr) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: a.cfg.PodNamespace,
				Name:      secretName,
			},
			Data: map[string][]byte{
				constants.EtcdEncryptionKeyFieldActive:      []byte(key),
				constants.EtcdEncryptionKeyFieldActiveName:  []byte(constants.LegacyAPIEncryptionKeyName),
				constants.EtcdEncryptionKeyFieldActiveSince: []byte(fmt.Sprintf("%d", time.Now().Unix())),
			},
		}
		if err := a.k8sClient.Create(ctx, secret); err != nil {
			return "", fmt.Errorf("creating kube-api encryption key secret %s/%s: %w", a.cfg.PodNamespace, secretName, err)
		}
		return key, nil
	}
	if lookupErr != nil {
		return "", fmt.Errorf("getting kube-api encryption key secret %s/%s: %w", a.cfg.PodNamespace, secretName, lookupErr)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[constants.EtcdEncryptionKeyFieldActive] = []byte(key)
	secret.Data[constants.EtcdEncryptionKeyFieldActiveName] = []byte(constants.LegacyAPIEncryptionKeyName)
	secret.Data[constants.EtcdEncryptionKeyFieldActiveSince] = []byte(fmt.Sprintf("%d", time.Now().Unix()))
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return "", fmt.Errorf("updating kube-api encryption key secret %s/%s: %w", a.cfg.PodNamespace, secretName, err)
	}
	return key, nil
}

func (a *Activities) requireKubeAPIEncryptionKey(ctx context.Context, clusterID uuid.UUID) (string, error) {
	if a.k8sClient == nil {
		return "", fmt.Errorf("kubernetes client is required for kube-api encryption keys")
	}
	secretName := constants.EtcdEncryptionKeySecretName(clusterID)
	secret := &corev1.Secret{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{Namespace: a.cfg.PodNamespace, Name: secretName}, secret); err != nil {
		return "", err
	}
	key := string(secret.Data[constants.EtcdEncryptionKeyFieldActive])
	if key == "" {
		return "", fmt.Errorf("kube-api encryption key secret %s/%s is missing %q", a.cfg.PodNamespace, secretName, constants.EtcdEncryptionKeyFieldActive)
	}
	return key, nil
}

func generateEncryptionKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
