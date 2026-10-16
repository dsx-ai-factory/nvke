// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvkerbac reads the operator-managed nke-rbac manifest.
package nvkerbac

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ConfigMapName = "nke-rbac"
	ConfigMapKey  = "manifest.yaml"
)

// ReadManifest returns the manifest from the ConfigMap in namespace, the
// shared management-plane namespace (config.Config.ManagementNamespaceOrDefault).
// A missing ConfigMap gives an apierrors.IsNotFound error.
func ReadManifest(ctx context.Context, reader ctrlclient.Reader, namespace string) ([]byte, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: namespace, Name: ConfigMapName}
	if err := reader.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("getting NVKE RBAC ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
	}

	manifest, ok := cm.Data[ConfigMapKey]
	if !ok {
		return nil, fmt.Errorf("NVKE RBAC ConfigMap %s/%s missing required key %q", key.Namespace, key.Name, ConfigMapKey)
	}

	result := bytes.TrimSpace([]byte(manifest))
	if len(result) == 0 {
		return nil, fmt.Errorf("NVKE RBAC ConfigMap %s/%s key %q is empty", key.Namespace, key.Name, ConfigMapKey)
	}
	return result, nil
}

// UserSubjects returns the User subjects of the manifest's role bindings, deduplicated.
func UserSubjects(manifest []byte) ([]string, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	seen := make(map[string]bool)
	var users []string
	for {
		var doc struct {
			Kind     string           `json:"kind"`
			Subjects []rbacv1.Subject `json:"subjects"`
		}
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return users, nil
			}
			return nil, fmt.Errorf("decoding NVKE RBAC manifest: %w", err)
		}
		if doc.Kind != "RoleBinding" && doc.Kind != "ClusterRoleBinding" {
			continue
		}
		for _, subject := range doc.Subjects {
			if subject.Kind == rbacv1.UserKind && subject.Name != "" && !seen[subject.Name] {
				seen[subject.Name] = true
				users = append(users, subject.Name)
			}
		}
	}
}
