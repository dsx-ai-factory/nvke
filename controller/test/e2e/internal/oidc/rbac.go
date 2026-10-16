// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

// The nke-rbac ConfigMap lives in the site's management namespace, which
// mgmtns.Namespace resolves -- it is per-site configurable, so it is never
// assumed here.
const (
	nvkeRBACConfigMap = "nke-rbac"
	nvkeRBACKey       = "manifest.yaml"
)

// ReservedUsers reads every User subject bound by the live nke-rbac manifest.
func ReservedUsers(ctx context.Context, reader client.Reader) ([]string, error) {
	var configMap corev1.ConfigMap
	key := types.NamespacedName{Namespace: mgmtns.Namespace(), Name: nvkeRBACConfigMap}
	if err := reader.Get(ctx, key, &configMap); err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", key.Namespace, key.Name, err)
	}
	manifest, ok := configMap.Data[nvkeRBACKey]
	if !ok || len(bytes.TrimSpace([]byte(manifest))) == 0 {
		return nil, fmt.Errorf("ConfigMap %s/%s has no %q manifest", key.Namespace, key.Name, nvkeRBACKey)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(manifest), 4096)
	seen := map[string]bool{}
	var users []string
	for {
		var document struct {
			Kind     string           `json:"kind"`
			Subjects []rbacv1.Subject `json:"subjects"`
		}
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode nke-rbac manifest: %w", err)
		}
		if document.Kind != "RoleBinding" && document.Kind != "ClusterRoleBinding" {
			continue
		}
		for _, subject := range document.Subjects {
			if subject.Kind == rbacv1.UserKind && subject.Name != "" && !seen[subject.Name] {
				seen[subject.Name] = true
				users = append(users, subject.Name)
			}
		}
	}
	if len(users) == 0 {
		return nil, errors.New("nke-rbac manifest has no User subjects")
	}
	return users, nil
}
