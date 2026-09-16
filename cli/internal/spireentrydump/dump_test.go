// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spireentrydump

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

var clusterStaticEntryListKinds = map[schema.GroupVersionResource]string{
	clusterStaticEntryGVR: "ClusterStaticEntryList",
}

func sourceCRD() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]any{
			"name": clusterStaticEntryCRD, "resourceVersion": "42", "uid": "source-uid",
		},
		"spec": map[string]any{
			"group": "spire.spiffe.io",
			"names": map[string]any{
				"kind": "ClusterStaticEntry", "plural": "clusterstaticentries",
			},
			"scope": "Cluster",
			"versions": []any{map[string]any{
				"name": "v1alpha1", "served": true, "storage": true,
			}},
		},
		"status": map[string]any{"storedVersions": []any{"v1alpha1"}},
	}}
}

func sourceEntry(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "spire.spiffe.io/v1alpha1",
		"kind":       "ClusterStaticEntry",
		"metadata": map[string]any{
			"name": name, "namespace": "must-not-cross", "resourceVersion": "42", "uid": "source-uid",
			"labels": map[string]any{"nke.nvidia.com/managed-by": "machine-spire-entry-controller"},
		},
		"spec": map[string]any{
			"className": "spire-mgmt-spire",
			"parentID":  "spiffe://example.test/spire/agent/tpm/hash",
			"spiffeID":  "spiffe://example.test/host/" + name,
			"selectors": []any{"unix:uid:0"},
		},
		"status": map[string]any{"entryId": "source-entry-id"},
	}}
}

func TestDumpWritesPortableResources(t *testing.T) {
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds,
		sourceCRD(), sourceEntry("host-b"), sourceEntry("host-a"),
	)
	dumper := &Dumper{source: source}
	dir := t.TempDir()

	count, err := dumper.Dump(context.Background(), dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	crd := readYAML(t, filepath.Join(dir, CRDFilename))
	assertNoServerMetadata(t, crd)
	if crd["kind"] != "CustomResourceDefinition" {
		t.Errorf("CRD kind = %v", crd["kind"])
	}
	if crd["spec"].(map[string]any)["scope"] != "Cluster" {
		t.Errorf("CRD spec was not preserved: %v", crd["spec"])
	}

	list := readYAML(t, filepath.Join(dir, EntriesFilename))
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("entry count = %d, want 2", len(items))
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["metadata"].(map[string]any)["name"] != "host-a" ||
		second["metadata"].(map[string]any)["name"] != "host-b" {
		t.Errorf("entries are not sorted by name: %v", items)
	}
	assertNoServerMetadata(t, first)
	if first["spec"].(map[string]any)["spiffeID"] != "spiffe://example.test/host/host-a" {
		t.Errorf("entry spec was not preserved: %v", first["spec"])
	}
	if first["metadata"].(map[string]any)["labels"].(map[string]any)["nke.nvidia.com/managed-by"] == "" {
		t.Error("entry label was not preserved")
	}

	for _, name := range []string{CRDFilename, EntriesFilename} {
		info, statErr := os.Stat(filepath.Join(dir, name))
		if statErr != nil {
			t.Fatalf("stat %s: %v", name, statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
}

func TestDumpAllowsNoClusterStaticEntries(t *testing.T) {
	source := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), clusterStaticEntryListKinds, sourceCRD(),
	)
	dir := t.TempDir()

	count, err := (&Dumper{source: source}).Dump(context.Background(), dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
	list := readYAML(t, filepath.Join(dir, EntriesFilename))
	if items := list["items"].([]any); len(items) != 0 {
		t.Errorf("items = %v, want empty", items)
	}
}

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var object map[string]any
	if err := yaml.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return object
}

func assertNoServerMetadata(t *testing.T, object map[string]any) {
	t.Helper()
	metadata := object["metadata"].(map[string]any)
	for _, key := range []string{"namespace", "resourceVersion", "uid"} {
		if _, found := metadata[key]; found {
			t.Errorf("metadata contains %s: %v", key, metadata)
		}
	}
	if _, found := object["status"]; found {
		t.Errorf("object contains status: %v", object)
	}
}
