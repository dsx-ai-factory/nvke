// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvkerbac reads the operator-managed tenant RBAC manifest.
package nvkerbac

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// LegacyConfigMapName is the name the chart rendered before the ConfigMap
	// name became a value, and so the name every site installed before it keeps.
	// It is what config.Config.RBACConfigMapSource falls back to: a site that has
	// not pinned a name must not be moved to a new one by a binary upgrade alone.
	LegacyConfigMapName = "nke-rbac"
	ConfigMapKey        = "manifest.yaml"
	// IdentitySourceAnnotation distinguishes operator-issued users from management identities.
	IdentitySourceAnnotation = "nvke.nvidia.com/identity-source"
)

// Source locates the operator-managed RBAC ConfigMap. Both fields are
// per-site configurable (nvkeSystemNamespace.name and rbacConfigMap.name), and
// a name resolved against the wrong namespace reads nothing, so they travel
// together rather than as two strings a caller can pair up wrongly.
// config.Config.RBACConfigMapSource builds the one every caller uses.
type Source struct {
	Namespace string
	Name      string
}

// ReadManifest returns the manifest from the ConfigMap src names.
// A missing ConfigMap gives an apierrors.IsNotFound error.
func ReadManifest(ctx context.Context, reader ctrlclient.Reader, src Source) ([]byte, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: src.Namespace, Name: src.Name}
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

// UserSubjects returns management User subjects to reserve, excluding bindings
// explicitly marked as operator-issued. Unmarked bindings preserve legacy behavior.
func UserSubjects(manifest []byte) ([]string, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	seen := make(map[string]bool)
	var users []string
	for {
		var doc struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
			Subjects []rbacv1.Subject  `json:"subjects"`
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
		switch source := doc.Metadata.Annotations[IdentitySourceAnnotation]; source {
		case "operator":
			continue
		case "", "management":
		default:
			return nil, fmt.Errorf("%s %s: %s must be management or operator, got %q", doc.Kind, doc.Metadata.Name, IdentitySourceAnnotation, source)
		}
		for _, subject := range doc.Subjects {
			if subject.Kind == rbacv1.UserKind && subject.Name != "" && !seen[subject.Name] {
				seen[subject.Name] = true
				users = append(users, subject.Name)
			}
		}
	}
}
