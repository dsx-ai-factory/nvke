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

	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

const (
	// rbacConfigMapKey is the ConfigMap data key containing the multi-document
	// tenant RBAC manifest.
	rbacConfigMapKey = nvkerbac.ConfigMapKey
	// rbacFieldManager is the Server-Side Apply field owner used when applying the
	// RBAC manifest into a tenant cluster.
	rbacFieldManager = "nvke"
	// legacyRBACName is the name the pre-rename manifest gave the controller's own
	// Role, ClusterRole and their bindings.
	legacyRBACName = "nke:cwe"
)

// legacyRBACObject is a tenant RBAC object an earlier release applied under the
// field manager "nke". Nothing owns it once the manifest is renamed, yet it
// keeps its permissions, so it is deleted explicitly.
type legacyRBACObject struct {
	kind      string
	namespace string
	name      string
}

// legacyRBACObjects returns everything the pre-rename manifest created: the
// nke:cwe set and the nke:capi-cluster-admin binding.
func legacyRBACObjects() []legacyRBACObject {
	return []legacyRBACObject{
		{"ClusterRole", "", legacyRBACName},
		{"ClusterRoleBinding", "", legacyRBACName},
		{"ClusterRoleBinding", "", "nke:capi-cluster-admin"},
		{"Role", "kube-system", legacyRBACName},
		{"RoleBinding", "kube-system", legacyRBACName},
		{"Role", "kube-node-lease", legacyRBACName},
		{"RoleBinding", "kube-node-lease", legacyRBACName},
	}
}

// pruneLegacyRBAC deletes the legacy objects the manifest does not itself
// carry, so a site that pins the old tenantRbacName keeps what it applies. Call
// it only after the manifest applied: the new permissions must exist before the
// old ones go. Deleting an absent object is not an error, which makes the prune
// safe to repeat on every reconcile.
func pruneLegacyRBAC(ctx context.Context, c ctrlclient.Client, manifest []byte) error {
	kept := make(map[legacyRBACObject]bool)
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decoding RBAC manifest: %w", err)
		}
		kept[legacyRBACObject{obj.GetKind(), obj.GetNamespace(), obj.GetName()}] = true
	}

	for _, legacy := range legacyRBACObjects() {
		if kept[legacy] {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("rbac.authorization.k8s.io/v1")
		obj.SetKind(legacy.kind)
		obj.SetNamespace(legacy.namespace)
		obj.SetName(legacy.name)
		err := c.Delete(ctx, obj)
		if ctrlclient.IgnoreNotFound(err) != nil {
			return fmt.Errorf("pruning legacy %s %s: %w", legacy.kind, legacy.name, err)
		}
	}
	return nil
}

// readRBACManifest reads the operator-managed RBAC ConfigMap from the
// management cluster and returns the multi-document tenant RBAC manifest.
func (c *Controller) readRBACManifest(ctx context.Context) ([]byte, error) {
	return nvkerbac.ReadManifest(ctx, c.k8sClient, c.cfg.RBACConfigMapSource())
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
