// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	spireExecCommand = "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"
)

var ErrWrongSecretOwner = errors.New("secret owned by a different cluster")

// ValidateSecretOwnership checks the immutable cluster-id label.
// Missing labels are allowed (legacy Secrets). kubeconfig-mode may change
// (EnableOIDC rewrites admin → SPIRE in place).
func ValidateSecretOwnership(secret *corev1.Secret, cluster db.Cluster) error {
	if secret.Labels == nil {
		return nil
	}
	if existing := secret.Labels[constants.LabelKeyClusterID]; existing != "" && existing != cluster.ID.String() {
		return fmt.Errorf("%w: %s/%s has cluster-id %s, want %s", ErrWrongSecretOwner, secret.Namespace, secret.Name, existing, cluster.ID)
	}
	return nil
}

// SetSecretOwnership writes the immutable ownership labels onto secret.
func SetSecretOwnership(secret *corev1.Secret, cluster db.Cluster, mode, managedByLabelValue string) {
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[constants.LabelKeyNkeManagedBy] = managedByLabelValue
	secret.Labels[constants.LabelKeyClusterID] = cluster.ID.String()
	secret.Labels[constants.LabelKeyKubeconfigMode] = mode
}

// IsSPIREExecKubeconfig reports whether kubeconfigBytes is credential-free SPIRE exec-auth.
// Every AuthInfo is inspected: a credential-bearing entry fails closed even if another
// user is SPIRE exec. Map iteration order must not affect the result.
func IsSPIREExecKubeconfig(kubeconfigBytes []byte) (bool, error) {
	cfg, err := clientcmd.Load(kubeconfigBytes)
	if err != nil {
		return false, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	sawSPIRE := false
	for _, auth := range cfg.AuthInfos {
		if auth == nil {
			continue
		}
		if authInfoHasStaticCredentials(auth) {
			return false, nil
		}
		if auth.Exec != nil && auth.Exec.Command == spireExecCommand {
			sawSPIRE = true
		}
	}
	return sawSPIRE, nil
}

func authInfoHasStaticCredentials(auth *clientcmdapi.AuthInfo) bool {
	return len(auth.ClientCertificateData) > 0 || auth.ClientCertificate != "" ||
		len(auth.ClientKeyData) > 0 || auth.ClientKey != "" ||
		auth.Token != "" || auth.TokenFile != "" ||
		auth.Username != "" || auth.Password != "" ||
		auth.AuthProvider != nil
}
