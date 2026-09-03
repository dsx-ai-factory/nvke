// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mgmtcluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestClient(t *testing.T, objs ...runtime.Object) *Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return newWithClient(kube)
}

func TestGetClusterKubeconfigReturnsAdminSecretData(t *testing.T) {
	c := newTestClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-admin-kubeconfig", Namespace: secretsNamespace},
		Data:       map[string][]byte{kubeconfigSecretKey: []byte("fake-kubeconfig-bytes")},
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
	c := newTestClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-kubeconfig", Namespace: secretsNamespace},
		Data:       map[string][]byte{kubeconfigSecretKey: []byte("spire-exec-auth-kubeconfig")},
	})

	if _, err := c.GetClusterKubeconfig(context.Background(), "my-cluster"); err == nil {
		t.Fatal("expected error when only the non-admin Secret exists, got nil")
	}
}

func TestGetClusterKubeconfigErrorsWhenSecretMissing(t *testing.T) {
	c := newTestClient(t)

	_, err := c.GetClusterKubeconfig(context.Background(), "missing-cluster")
	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
}

func TestGetClusterKubeconfigErrorsWhenDataKeyMissing(t *testing.T) {
	c := newTestClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-admin-kubeconfig", Namespace: secretsNamespace},
		Data:       map[string][]byte{"unexpected-key": []byte("x")},
	})

	if _, err := c.GetClusterKubeconfig(context.Background(), "my-cluster"); err == nil {
		t.Fatal("expected error when the secret has no \"value\" key, got nil")
	}
}
