// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	nkeRBACFieldManager = "nke-agent-rbac"
	rbacAPIVersion      = "rbac.authorization.k8s.io/v1"
	nkeRBACNamePrefix   = "nke:"
	rbacAPIGroup        = "rbac.authorization.k8s.io"
	// rbacVersionV1 is the version segment of rbacAPIVersion, used on its own
	// in the GroupVersionResource entries in rbacKindSpecs.
	rbacVersionV1 = "v1"

	resourceClusterRoles        = "clusterroles"
	resourceClusterRoleBindings = "clusterrolebindings"
	resourceRoles               = "roles"
	resourceRoleBindings        = "rolebindings"

	rbacKindClusterRole        = "ClusterRole"
	rbacKindClusterRoleBinding = "ClusterRoleBinding"
	rbacKindRole               = "Role"
	rbacKindRoleBinding        = "RoleBinding"
)

type rbacKindSpec struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

var supportedRBACKinds = func() []string {
	return slices.Sorted(maps.Keys(rbacKindSpecs))
}()

var rbacKindSpecs = map[string]rbacKindSpec{
	rbacKindClusterRole: {
		gvr: schema.GroupVersionResource{
			Group:    rbacAPIGroup,
			Version:  rbacVersionV1,
			Resource: resourceClusterRoles,
		},
		namespaced: false,
	},
	rbacKindClusterRoleBinding: {
		gvr: schema.GroupVersionResource{
			Group:    rbacAPIGroup,
			Version:  rbacVersionV1,
			Resource: resourceClusterRoleBindings,
		},
		namespaced: false,
	},
	rbacKindRole: {
		gvr: schema.GroupVersionResource{
			Group:    rbacAPIGroup,
			Version:  rbacVersionV1,
			Resource: resourceRoles,
		},
		namespaced: true,
	},
	rbacKindRoleBinding: {
		gvr: schema.GroupVersionResource{
			Group:    rbacAPIGroup,
			Version:  rbacVersionV1,
			Resource: resourceRoleBindings,
		},
		namespaced: true,
	},
}

func parseAndValidateNKERBACManifest(manifest []byte) ([]*unstructured.Unstructured, error) {
	objects, err := parseKubernetesManifest(manifest)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}

	for index, obj := range objects {
		docIndex := index + 1
		if err := validateNKERBACObject(obj); err != nil {
			return nil, fmt.Errorf("validating Kubernetes object from document %d: %w", docIndex, err)
		}

		key := keyForObject(obj)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicated Kubernetes object %s", key)
		}
		seen[key] = struct{}{}
	}

	if len(objects) == 0 {
		return nil, errors.New("manifest must contain at least one resource")
	}

	return objects, nil
}

func parseKubernetesManifest(manifest []byte) ([]*unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	objects := []*unstructured.Unstructured{}
	docIndex := 0

	for {
		objMap := map[string]any{}
		err := decoder.Decode(&objMap)
		if err == io.EOF {
			break
		}
		docIndex++
		if err != nil {
			return nil, fmt.Errorf("parsing Kubernetes object from document %d: %w", docIndex, err)
		}
		if len(objMap) == 0 {
			continue
		}

		objects = append(objects, &unstructured.Unstructured{Object: objMap})
	}

	return objects, nil
}

func keyForObject(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind() // e.g. "rbac.authorization.k8s.io/v1, Kind=ClusterRole"
	return fmt.Sprintf("%s/%s/%s", gvk, obj.GetNamespace(), obj.GetName())
}

func validateNKERBACObject(obj *unstructured.Unstructured) error {
	apiVersion := obj.GetAPIVersion()
	if apiVersion == "" {
		return errors.New("apiVersion is required")
	}
	if apiVersion != rbacAPIVersion {
		return fmt.Errorf("unsupported apiVersion %s, expected %s", apiVersion, rbacAPIVersion)
	}

	kind := obj.GetKind()
	if kind == "" {
		return errors.New("kind is required")
	}
	kindSpec, ok := rbacKindSpecs[kind]
	if !ok {
		return fmt.Errorf("unsupported kind %s, supported kinds are %s", kind, strings.Join(supportedRBACKinds, ", "))
	}

	name := obj.GetName()
	if name == "" {
		return errors.New("metadata.name is required")
	}
	if !isAllowedNKERBACName(name) {
		return fmt.Errorf("resource %s named %q is not allowed, names must start with %q", kind, name, nkeRBACNamePrefix)
	}

	namespace := obj.GetNamespace()
	if kindSpec.namespaced && namespace == "" {
		return fmt.Errorf("resource %s named %q must set metadata.namespace", kind, name)
	}
	if !kindSpec.namespaced && namespace != "" {
		return fmt.Errorf("resource %s named %q must not set metadata.namespace", kind, name)
	}

	switch kind {
	case rbacKindClusterRoleBinding, rbacKindRoleBinding:
		if err := validateNKERBACRoleRef(obj); err != nil {
			return err
		}
	}

	return nil
}

func isAllowedNKERBACName(name string) bool {
	return len(name) > len(nkeRBACNamePrefix) && strings.HasPrefix(name, nkeRBACNamePrefix)
}

func validateNKERBACRoleRef(obj *unstructured.Unstructured) error {
	roleRefName, found, err := unstructured.NestedString(obj.Object, "roleRef", "name")
	if err != nil {
		return fmt.Errorf("reading roleRef.name: %w", err)
	}
	if !found || roleRefName == "" {
		return nil
	}
	if !isAllowedNKERBACName(roleRefName) {
		return fmt.Errorf("%s roleRef.name %q is not allowed, names must start with %q", obj.GetKind(), roleRefName, nkeRBACNamePrefix)
	}

	return nil
}

func applyNKERBACResources(
	ctx context.Context,
	client dynamic.Interface,
	resources []*unstructured.Unstructured,
) *pb.UpdateNKERBACResponse {
	resp := &pb.UpdateNKERBACResponse{
		Results: make([]*pb.ResourceApplyResult, 0, len(resources)),
	}

	for _, resource := range resources {
		outcome, err := applyNKERBACResource(ctx, client, resource)
		result := &pb.ResourceApplyResult{
			Kind:      resource.GetKind(),
			Name:      resource.GetName(),
			Namespace: resource.GetNamespace(),
			Outcome:   outcome,
		}
		if err != nil {
			result.Outcome = pb.ResourceApplyResult_OUTCOME_FAILED
			result.ErrorMessage = err.Error()
		}
		resp.Results = append(resp.Results, result)
		if err != nil {
			break
		}
	}

	return resp
}

func applyNKERBACResource(
	ctx context.Context,
	client dynamic.Interface,
	resource *unstructured.Unstructured,
) (pb.ResourceApplyResult_Outcome, error) {
	name := resource.GetName()

	kindSpec, ok := rbacKindSpecs[resource.GetKind()]
	if !ok {
		return pb.ResourceApplyResult_OUTCOME_FAILED, fmt.Errorf("applying RBAC resource %s: unsupported kind", keyForObject(resource))
	}

	var resourceClient dynamic.ResourceInterface
	if kindSpec.namespaced {
		resourceClient = client.Resource(kindSpec.gvr).Namespace(resource.GetNamespace())
	} else {
		resourceClient = client.Resource(kindSpec.gvr)
	}

	var beforeGeneration int64
	existed := true

	before, err := resourceClient.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		existed = false
	} else if err != nil {
		return pb.ResourceApplyResult_OUTCOME_FAILED, fmt.Errorf("reading %s before apply: %w", keyForObject(resource), err)
	} else {
		beforeGeneration = before.GetGeneration()
	}

	applyObject := resource.DeepCopy()
	unstructured.RemoveNestedField(applyObject.Object, "metadata", "managedFields")

	after, err := resourceClient.Apply(
		ctx,
		name,
		applyObject,
		metav1.ApplyOptions{
			FieldManager: nkeRBACFieldManager,
			Force:        true,
		},
	)
	if err != nil {
		return pb.ResourceApplyResult_OUTCOME_FAILED, fmt.Errorf("applying %s: %w", keyForObject(resource), err)
	}

	if !existed {
		return pb.ResourceApplyResult_OUTCOME_CREATED, nil
	}
	if beforeGeneration != after.GetGeneration() {
		return pb.ResourceApplyResult_OUTCOME_UPDATED, nil
	}
	return pb.ResourceApplyResult_OUTCOME_UNCHANGED, nil
}
