// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mgmtcluster

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeSecretGetter is a minimal secretGetter double, keyed by Secret name.
type fakeSecretGetter map[string]*corev1.Secret

func (f fakeSecretGetter) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Secret, error) {
	secret, ok := f[name]
	if !ok {
		return nil, fmt.Errorf("secret %q not found", name)
	}
	return secret, nil
}

func TestGetClusterKubeconfigReturnsAdminSecretData(t *testing.T) {
	c := newWithSecretGetter(fakeSecretGetter{
		"my-cluster-admin-kubeconfig": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-admin-kubeconfig", Namespace: secretsNamespace},
			Data:       map[string][]byte{kubeconfigSecretKey: []byte("fake-kubeconfig-bytes")},
		},
	})

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
	c := newWithSecretGetter(fakeSecretGetter{
		"my-cluster-kubeconfig": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-kubeconfig", Namespace: secretsNamespace},
			Data:       map[string][]byte{kubeconfigSecretKey: []byte("spire-exec-auth-kubeconfig")},
		},
	})

	if _, err := c.GetClusterKubeconfig(context.Background(), "my-cluster"); err == nil {
		t.Fatal("expected error when only the non-admin Secret exists, got nil")
	}
}

func TestGetClusterKubeconfigErrorsWhenSecretMissing(t *testing.T) {
	c := newWithSecretGetter(fakeSecretGetter{})

	_, err := c.GetClusterKubeconfig(context.Background(), "missing-cluster")
	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
}

func TestGetClusterKubeconfigErrorsWhenDataKeyMissing(t *testing.T) {
	c := newWithSecretGetter(fakeSecretGetter{
		"my-cluster-admin-kubeconfig": {
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-admin-kubeconfig", Namespace: secretsNamespace},
			Data:       map[string][]byte{"unexpected-key": []byte("x")},
		},
	})

	if _, err := c.GetClusterKubeconfig(context.Background(), "my-cluster"); err == nil {
		t.Fatal("expected error when the secret has no \"value\" key, got nil")
	}
}
