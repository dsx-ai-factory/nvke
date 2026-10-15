// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	fixtureName = "oidc-reference-reader"
)

// AdminFixture provides resource requests used to prove 200 and 403 outcomes.
type AdminFixture struct {
	Allowed ResourceRequest
	Denied  ResourceRequest
}

// PrepareAdminFixture grants one username permission to list ConfigMaps.
// The runner stops envtest to discard the namespace and RBAC, including partial setup.
func PrepareAdminFixture(ctx context.Context, client kubernetes.Interface, namespace, username string) (AdminFixture, error) {
	fixture := AdminFixture{
		Allowed: ResourceRequest{Resource: "configmaps", Namespace: namespace},
		Denied:  ResourceRequest{Resource: "secrets", Namespace: namespace},
	}
	namespaceObject := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, namespaceObject, metav1.CreateOptions{}); err != nil {
		return AdminFixture{}, fmt.Errorf("create probe namespace: %w", err)
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureName, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"list"},
		}},
	}
	if _, err := client.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		return AdminFixture{}, fmt.Errorf("create probe role: %w", err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureName, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: fixtureName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: username, APIGroup: rbacv1.GroupName}},
	}
	if _, err := client.RbacV1().RoleBindings(namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		return AdminFixture{}, fmt.Errorf("create probe role binding: %w", err)
	}
	return fixture, nil
}
