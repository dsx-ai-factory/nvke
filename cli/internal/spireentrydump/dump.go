// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package spireentrydump writes SPIRE ClusterStaticEntry policy to disk.
package spireentrydump

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const (
	// CRDFilename is the exported ClusterStaticEntry CRD filename.
	CRDFilename = "clusterstaticentry-crd.yaml"
	// EntriesFilename is the exported ClusterStaticEntry list filename.
	EntriesFilename = "clusterstaticentries.yaml"

	clusterStaticEntryCRD = "clusterstaticentries.spire.spiffe.io"
)

var (
	clusterStaticEntryGVR = schema.GroupVersionResource{
		Group: "spire.spiffe.io", Version: "v1alpha1", Resource: "clusterstaticentries",
	}
	customResourceDefinitionGVR = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
)

// Dumper reads ClusterStaticEntry resources from a Kubernetes API and writes portable YAML files.
type Dumper struct {
	source dynamic.Interface
}

// New returns a Dumper configured from the source kubeconfig file.
func New(sourceKubeconfig string) (*Dumper, error) {
	config, err := clientcmd.BuildConfigFromFlags("", sourceKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", sourceKubeconfig, err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating client for kubeconfig %s: %w", sourceKubeconfig, err)
	}
	return &Dumper{source: client}, nil
}

// Dump writes the source CRD and ClusterStaticEntries in outputDir and returns the entry count.
func (d *Dumper) Dump(ctx context.Context, outputDir string) (int, error) {
	crd, err := d.source.Resource(customResourceDefinitionGVR).Get(
		ctx, clusterStaticEntryCRD, metav1.GetOptions{},
	)
	if err != nil {
		return 0, fmt.Errorf("reading source ClusterStaticEntry CRD: %w", err)
	}
	if _, found := crd.Object["spec"]; !found {
		return 0, fmt.Errorf("source ClusterStaticEntry CRD has no spec")
	}

	entries, err := d.source.Resource(clusterStaticEntryGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("listing source ClusterStaticEntries: %w", err)
	}
	sort.Slice(entries.Items, func(i, j int) bool {
		return entries.Items[i].GetName() < entries.Items[j].GetName()
	})

	crdYAML, err := yaml.Marshal(sanitizedCRD(crd).Object)
	if err != nil {
		return 0, fmt.Errorf("encoding ClusterStaticEntry CRD: %w", err)
	}
	entryObjects := make([]any, 0, len(entries.Items))
	for i := range entries.Items {
		entryObjects = append(entryObjects, sanitizedEntry(&entries.Items[i]).Object)
	}
	list := map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      entryObjects,
	}
	entriesYAML, err := yaml.Marshal(list)
	if err != nil {
		return 0, fmt.Errorf("encoding ClusterStaticEntries: %w", err)
	}

	if err := writeFile(filepath.Join(outputDir, CRDFilename), crdYAML); err != nil {
		return 0, err
	}
	if err := writeFile(filepath.Join(outputDir, EntriesFilename), entriesYAML); err != nil {
		return 0, err
	}
	return len(entries.Items), nil
}

func writeFile(path string, contents []byte) error {
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func sanitizedCRD(source *unstructured.Unstructured) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]any{
			"name": clusterStaticEntryCRD,
		},
		"spec": runtime.DeepCopyJSONValue(source.Object["spec"]),
	}}
}

func sanitizedEntry(source *unstructured.Unstructured) *unstructured.Unstructured {
	entry := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "spire.spiffe.io/v1alpha1",
		"kind":       "ClusterStaticEntry",
		"metadata": map[string]any{
			"name": source.GetName(),
		},
		"spec": runtime.DeepCopyJSONValue(source.Object["spec"]),
	}}
	if labels := source.GetLabels(); len(labels) > 0 {
		entry.SetLabels(labels)
	}
	return entry
}
