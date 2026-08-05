// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package tenantaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// rbacConfigMapName is the operator-managed ConfigMap holding the tenant RBAC
	// manifest that gets applied into OIDC tenant clusters.
	rbacConfigMapName = "nke-rbac"
	// rbacConfigMapKey is the ConfigMap data key containing the multi-document
	// tenant RBAC manifest.
	rbacConfigMapKey = "manifest.yaml"
	// rbacFieldManager is the Server-Side Apply field owner used when applying the
	// RBAC manifest into a tenant cluster.
	rbacFieldManager = "nke"
)

// readRBACManifest reads the operator-managed nke-rbac ConfigMap from the
// management cluster and returns the multi-document tenant RBAC manifest.
func (c *Controller) readRBACManifest(ctx context.Context) ([]byte, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: SecretsNamespace, Name: rbacConfigMapName}
	if err := c.k8sClient.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("getting NKE RBAC ConfigMap %s/%s: %w", SecretsNamespace, rbacConfigMapName, err)
	}

	manifest, ok := cm.Data[rbacConfigMapKey]
	if !ok {
		return nil, fmt.Errorf("NKE RBAC ConfigMap %s/%s missing required key %q", SecretsNamespace, rbacConfigMapName, rbacConfigMapKey)
	}

	result := bytes.TrimSpace([]byte(manifest))
	if len(result) == 0 {
		return nil, fmt.Errorf("NKE RBAC ConfigMap %s/%s key %q is empty", SecretsNamespace, rbacConfigMapName, rbacConfigMapKey)
	}

	return result, nil
}

// applyManifestSSA splits a multi-document YAML manifest and Server-Side-Applies
// each document into the given client under the supplied field manager.
func applyManifestSSA(ctx context.Context, c ctrlclient.Client, manifest []byte, fieldManager string) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decoding RBAC manifest: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		// Server-Side Apply each document. The object is decoded straight from the
		// manifest YAML (not converted from a typed API object), so wrapping it with
		// ApplyConfigurationFromUnstructured is safe — there is no ambiguity between
		// an unset field and an explicit zero value.
		if err := c.Apply(
			ctx,
			ctrlclient.ApplyConfigurationFromUnstructured(obj),
			ctrlclient.FieldOwner(fieldManager),
			ctrlclient.ForceOwnership,
		); err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
}
