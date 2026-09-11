// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mgmtcluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// validKubeconfig is a syntactically valid, minimal kubeconfig. New() never
// dials the server it names, so this doesn't need to be reachable.
const validKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: mgmt
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: mgmt
  context:
    cluster: mgmt
    user: mgmt
current-context: mgmt
users:
- name: mgmt
  user:
    token: fake-token
`

func newTestClient(t *testing.T, objs ...runtime.Object) *Client {
	t.Helper()
	return newWithClient(fake.NewSimpleClientset(objs...))
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
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention \"not found\"", err)
	}
}

func TestGetClusterKubeconfigErrorsWhenForbidden(t *testing.T) {
	c := newTestClient(t)
	c.kube.(*fake.Clientset).PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), "my-cluster-admin-kubeconfig", errors.New("denied"))
	})

	_, err := c.GetClusterKubeconfig(context.Background(), "my-cluster")
	if err == nil {
		t.Fatal("expected error when the request is forbidden, got nil")
	}
	if !strings.Contains(err.Error(), "not allowed to read secret") {
		t.Errorf("error = %q, want it to mention the identity needs 'get' on secrets", err)
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

func TestNewErrorsWhenPathIsEmpty(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("expected error for an empty kubeconfig path, got nil")
	}
}

func TestNewErrorsWhenPathDoesNotExist(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for a nonexistent kubeconfig path, got nil")
	}
}

func TestNewBuildsClientFromValidKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(validKubeconfig), 0o600); err != nil {
		t.Fatalf("writing test kubeconfig: %v", err)
	}

	c, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil || c.kube == nil {
		t.Fatal("New returned a Client with no underlying client")
	}
}
