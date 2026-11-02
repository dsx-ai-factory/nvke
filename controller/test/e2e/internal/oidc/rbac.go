// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

// The RBAC ConfigMap's namespace and name are both per-site configurable
// (nvkeSystemNamespace.name and rbacConfigMap.name), so neither is assumed
// here: mgmtns.Namespace resolves the one, ConfigMapName the other.
const (
	// ConfigMapNameEnvVar names the RBAC ConfigMap of the site under test. Set
	// it when running against a site that pinned a name other than
	// DefaultConfigMapName -- a site installed before the name became
	// configurable uses "nke-rbac".
	ConfigMapNameEnvVar = "NVKE_E2E_RBAC_CONFIGMAP"

	// DefaultConfigMapName is the name a current chart renders.
	DefaultConfigMapName = "nvke-rbac"

	nvkeRBACKey = "manifest.yaml"
)

// ConfigMapName returns the RBAC ConfigMap name of the site under test.
func ConfigMapName() string {
	if trimmed := strings.TrimSpace(os.Getenv(ConfigMapNameEnvVar)); trimmed != "" {
		return trimmed
	}
	return DefaultConfigMapName
}

// ReservedUsers reads every User subject bound by the live RBAC manifest.
func ReservedUsers(ctx context.Context, reader client.Reader) ([]string, error) {
	var configMap corev1.ConfigMap
	key := types.NamespacedName{Namespace: mgmtns.Namespace(), Name: ConfigMapName()}
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
			return nil, fmt.Errorf("decode RBAC manifest: %w", err)
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
		return nil, fmt.Errorf("ConfigMap %s/%s has no User subjects", key.Namespace, key.Name)
	}
	return users, nil
}
