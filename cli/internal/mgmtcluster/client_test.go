// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mgmtcluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetClusterKubeconfigReturnsAdminSecretData(t *testing.T) {
	kube := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-admin-kubeconfig", Namespace: secretsNamespace},
		Data:       map[string][]byte{kubeconfigSecretKey: []byte("fake-kubeconfig-bytes")},
	})
	c := NewWithClient(kube)

	data, err := c.GetClusterKubeconfig(context.Background(), "my-cluster")
	if err != nil {
		t.Fatalf("GetClusterKubeconfig: %v", err)
	}
	if string(data) != "fake-kubeconfig-bytes" {
		t.Errorf("data = %q, want %q", data, "fake-kubeconfig-bytes")
	}
}

func TestGetClusterKubeconfigIgnoresNonAdminSecret(t *testing.T) {
	// Only the CWE/CAPI-consumed "-kubeconfig" Secret exists (e.g. a SPIRE
	// exec-auth kubeconfig on an OIDC-enabled cluster) — GetClusterKubeconfig
	// must not fall back to it.
	kube := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-kubeconfig", Namespace: secretsNamespace},
		Data:       map[string][]byte{kubeconfigSecretKey: []byte("spire-exec-auth-kubeconfig")},
	})
	c := NewWithClient(kube)

	if _, err := c.GetClusterKubeconfig(context.Background(), "my-cluster"); err == nil {
		t.Fatal("expected error when only the non-admin Secret exists, got nil")
	}
}

func TestGetClusterKubeconfigErrorsWhenSecretMissing(t *testing.T) {
	c := NewWithClient(fake.NewClientset())

	_, err := c.GetClusterKubeconfig(context.Background(), "missing-cluster")
	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
}

func TestGetClusterKubeconfigErrorsWhenDataKeyMissing(t *testing.T) {
	kube := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-admin-kubeconfig", Namespace: secretsNamespace},
		Data:       map[string][]byte{"unexpected-key": []byte("x")},
	})
	c := NewWithClient(kube)

	if _, err := c.GetClusterKubeconfig(context.Background(), "my-cluster"); err == nil {
		t.Fatal("expected error when the secret has no \"value\" key, got nil")
	}
}
