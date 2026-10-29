// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

func TestReservedUsersReadsAllBindingDocuments(t *testing.T) {
	t.Parallel()
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: mgmtns.Namespace(), Name: ConfigMapName()},
		Data: map[string]string{nvkeRBACKey: `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
subjects:
- kind: User
  name: first
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
subjects:
- kind: ServiceAccount
  name: ignored
- kind: User
  name: second
`},
	}
	client := fake.NewClientBuilder().WithObjects(configMap).Build()
	users, err := ReservedUsers(context.Background(), client)
	if err != nil {
		t.Fatalf("ReservedUsers() error = %v", err)
	}
	if len(users) != 2 || users[0] != "first" || users[1] != "second" {
		t.Fatalf("ReservedUsers() = %v", users)
	}
}

func TestReservedUsersFailsWhenManifestHasNoUsers(t *testing.T) {
	t.Parallel()
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: mgmtns.Namespace(), Name: ConfigMapName()},
		Data:       map[string]string{nvkeRBACKey: "kind: ClusterRole\n"},
	}
	client := fake.NewClientBuilder().WithObjects(configMap).Build()
	if _, err := ReservedUsers(context.Background(), client); err == nil {
		t.Fatal("ReservedUsers() error = nil, want missing User subjects")
	}
}
