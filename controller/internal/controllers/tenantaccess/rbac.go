// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/nkerbac"
)

const (
	// rbacConfigMapName is the operator-managed ConfigMap holding the tenant RBAC
	// manifest that gets applied into OIDC tenant clusters.
	rbacConfigMapName = nkerbac.ConfigMapName
	// rbacConfigMapKey is the ConfigMap data key containing the multi-document
	// tenant RBAC manifest.
	rbacConfigMapKey = nkerbac.ConfigMapKey
	// rbacFieldManager is the Server-Side Apply field owner used when applying the
	// RBAC manifest into a tenant cluster.
	rbacFieldManager = "nke"
)

// readRBACManifest reads the operator-managed nke-rbac ConfigMap from the
// management cluster and returns the multi-document tenant RBAC manifest.
func (c *Controller) readRBACManifest(ctx context.Context) ([]byte, error) {
	return nkerbac.ReadManifest(ctx, c.k8sClient)
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
