// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spireentrycopy

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

var clusterStaticEntryListKinds = map[schema.GroupVersionResource]string{
	clusterStaticEntryGVR: "ClusterStaticEntryList",
}

func sourceCRD() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]any{
			"name":            clusterStaticEntryCRD,
			"resourceVersion": "source-version",
			"uid":             "source-uid",
		},
		"spec": map[string]any{
			"group": "spire.spiffe.io",
			"names": map[string]any{
				"kind":     "ClusterStaticEntry",
				"listKind": "ClusterStaticEntryList",
				"plural":   "clusterstaticentries",
			},
			"scope": "Cluster",
			"versions": []any{map[string]any{
				"name": "v1alpha1", "served": true, "storage": true,
			}},
		},
		"status": map[string]any{"storedVersions": []any{"v1alpha1"}},
	}}
}

func TestCopyPreservesPolicyAndSanitizesServerMetadata(t *testing.T) {
	sourceEntry := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "spire.spiffe.io/v1alpha1",
		"kind":       "ClusterStaticEntry",
		"metadata": map[string]any{
			"name":            "host-1234",
			"namespace":       "must-not-cross",
			"resourceVersion": "42",
			"uid":             "source-uid",
			"labels": map[string]any{
				"nke.nvidia.com/managed-by": "machine-spire-entry-controller",
			},
		},
		"spec": map[string]any{
			"className": "spire-mgmt-spire",
			"parentID":  "spiffe://example.test/spire/agent/tpm/hash",
			"spiffeID":  "spiffe://example.test/host/1234",
			"selectors": []any{"unix:uid:0"},
		},
		"status": map[string]any{"entryId": "source-entry-id"},
	}}
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds, sourceCRD(), sourceEntry,
	)
	target := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds,
	)

	copier := &Copier{source: source, target: target}
	count, err := copier.Copy(context.Background())
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	got, err := target.Resource(clusterStaticEntryGVR).Get(
		context.Background(), sourceEntry.GetName(), metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get copied entry: %v", err)
	}
	if got.GetNamespace() != "" {
		t.Errorf("namespace = %q, want empty", got.GetNamespace())
	}
	if got.GetUID() != "" {
		t.Errorf("uid = %q, want empty", got.GetUID())
	}
	if _, found := got.Object["status"]; found {
		t.Error("copied entry contains source status")
	}
	if got.GetLabels()["nke.nvidia.com/managed-by"] != "machine-spire-entry-controller" {
		t.Errorf("managed-by label was not preserved: %v", got.GetLabels())
	}
	if got.Object["spec"].(map[string]any)["spiffeID"] != "spiffe://example.test/host/1234" {
		t.Errorf("spec was not preserved: %v", got.Object["spec"])
	}
	copiedCRD, err := target.Resource(customResourceDefinitionGVR).Get(
		context.Background(), clusterStaticEntryCRD, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get copied CRD: %v", err)
	}
	if copiedCRD.Object["spec"].(map[string]any)["scope"] != "Cluster" {
		t.Errorf("CRD definition was not copied: %v", copiedCRD.Object["spec"])
	}
	if copiedCRD.GetUID() != "" {
		t.Errorf("CRD uid = %q, want empty", copiedCRD.GetUID())
	}
	if _, found := copiedCRD.Object["status"]; found {
		t.Error("copied CRD contains source status")
	}
}

func TestCopyAllowsNoClusterStaticEntries(t *testing.T) {
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds, sourceCRD(),
	)
	target := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds,
	)

	copier := &Copier{source: source, target: target}
	count, err := copier.Copy(context.Background())
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestCopyUpdatesExistingEntryAndPreservesTargetMetadata(t *testing.T) {
	sourceEntry := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "spire.spiffe.io/v1alpha1",
		"kind":       "ClusterStaticEntry",
		"metadata": map[string]any{
			"name": "host-1234",
		},
		"spec": map[string]any{
			"className": "spire-mgmt-spire",
			"parentID":  "spiffe://example.test/spire/agent/tpm/new-hash",
			"spiffeID":  "spiffe://example.test/host/1234",
			"selectors": []any{"unix:uid:0"},
		},
	}}
	targetEntry := sourceEntry.DeepCopy()
	targetEntry.Object["spec"].(map[string]any)["parentID"] = "spiffe://example.test/spire/agent/tpm/old-hash"
	targetEntry.SetAnnotations(map[string]string{"target.example/owned": "true"})
	targetEntry.SetFinalizers([]string{"target.example/cleanup"})
	targetEntry.SetLabels(map[string]string{"target.example/owned": "true"})

	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds, sourceCRD(), sourceEntry,
	)
	target := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds, targetEntry,
	)

	copier := &Copier{source: source, target: target}
	if _, err := copier.Copy(context.Background()); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	got, err := target.Resource(clusterStaticEntryGVR).Get(
		context.Background(), sourceEntry.GetName(), metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get copied entry: %v", err)
	}
	if got.Object["spec"].(map[string]any)["parentID"] != "spiffe://example.test/spire/agent/tpm/new-hash" {
		t.Errorf("source policy did not replace target policy: %v", got.Object["spec"])
	}
	if got.GetAnnotations()["target.example/owned"] != "true" {
		t.Errorf("target annotations were not preserved: %v", got.GetAnnotations())
	}
	if len(got.GetFinalizers()) != 1 || got.GetFinalizers()[0] != "target.example/cleanup" {
		t.Errorf("target finalizers were not preserved: %v", got.GetFinalizers())
	}
	if got.GetLabels()["target.example/owned"] != "true" {
		t.Errorf("target labels were not preserved: %v", got.GetLabels())
	}
}
