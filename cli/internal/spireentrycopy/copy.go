// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package spireentrycopy copies SPIRE ClusterStaticEntry policy between clusters.
package spireentrycopy

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	clusterStaticEntryCRD   = "clusterstaticentries.spire.spiffe.io"
	crdEstablishmentTimeout = time.Minute
)

var (
	clusterStaticEntryGVR = schema.GroupVersionResource{
		Group: "spire.spiffe.io", Version: "v1alpha1", Resource: "clusterstaticentries",
	}
	customResourceDefinitionGVR = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
)

// Copier copies ClusterStaticEntry specifications from one Kubernetes API to another.
type Copier struct {
	source     dynamic.Interface
	target     dynamic.Interface
	waitForCRD func(context.Context) error
}

// New returns a Copier configured from the source and target kubeconfig files.
func New(sourceKubeconfig, targetKubeconfig string) (*Copier, error) {
	source, err := clientForKubeconfig(sourceKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building source Kubernetes client: %w", err)
	}
	target, err := clientForKubeconfig(targetKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building target Kubernetes client: %w", err)
	}
	copier := &Copier{source: source, target: target}
	copier.waitForCRD = copier.waitForClusterStaticEntryCRD
	return copier, nil
}

func clientForKubeconfig(path string) (dynamic.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", path, err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating client for kubeconfig %s: %w", path, err)
	}
	return client, nil
}

// Copy prepares the target for ClusterStaticEntry resources and copies every source entry.
func (c *Copier) Copy(ctx context.Context) (int, error) {
	if err := c.copyClusterStaticEntryCRD(ctx); err != nil {
		return 0, err
	}
	if c.waitForCRD != nil {
		if err := c.waitForCRD(ctx); err != nil {
			return 0, err
		}
	}

	entries, err := c.source.Resource(clusterStaticEntryGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("listing source ClusterStaticEntries: %w", err)
	}

	for i := range entries.Items {
		entry := sanitizedEntry(&entries.Items[i])
		if err := c.copyEntry(ctx, entry); err != nil {
			return i, fmt.Errorf("applying ClusterStaticEntry %s to target: %w", entry.GetName(), err)
		}
	}

	return len(entries.Items), nil
}

func (c *Copier) copyClusterStaticEntryCRD(ctx context.Context) error {
	source, err := c.source.Resource(customResourceDefinitionGVR).Get(
		ctx, clusterStaticEntryCRD, metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("reading source ClusterStaticEntry CRD: %w", err)
	}
	spec, found := source.Object["spec"]
	if !found {
		return fmt.Errorf("source ClusterStaticEntry CRD has no spec")
	}
	copiedCRD := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]any{
			"name": clusterStaticEntryCRD,
		},
		"spec": runtime.DeepCopyJSONValue(spec),
	}}
	if err := c.createOrUpdate(ctx, customResourceDefinitionGVR, copiedCRD); err != nil {
		return fmt.Errorf("applying target ClusterStaticEntry CRD: %w", err)
	}
	return nil
}

func (c *Copier) waitForClusterStaticEntryCRD(ctx context.Context) error {
	err := wait.PollUntilContextTimeout(ctx, time.Second, crdEstablishmentTimeout, true,
		func(ctx context.Context) (bool, error) {
			crd, err := c.target.Resource(customResourceDefinitionGVR).Get(
				ctx, clusterStaticEntryCRD, metav1.GetOptions{},
			)
			if err != nil {
				return false, err
			}
			conditions, found, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
			if err != nil || !found {
				return false, err
			}
			for _, raw := range conditions {
				condition, ok := raw.(map[string]any)
				if ok && condition["type"] == "Established" && condition["status"] == "True" {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for target ClusterStaticEntry CRD: %w", err)
	}
	return nil
}

func (c *Copier) copyEntry(ctx context.Context, entry *unstructured.Unstructured) error {
	return c.createOrUpdate(ctx, clusterStaticEntryGVR, entry)
}

func (c *Copier) createOrUpdate(
	ctx context.Context,
	resource schema.GroupVersionResource,
	object *unstructured.Unstructured,
) error {
	objects := c.target.Resource(resource)
	current, err := objects.Get(ctx, object.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = objects.Create(ctx, object, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	object.SetResourceVersion(current.GetResourceVersion())
	object.SetAnnotations(current.GetAnnotations())
	object.SetFinalizers(current.GetFinalizers())
	object.SetOwnerReferences(current.GetOwnerReferences())
	labels := current.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	for key, value := range object.GetLabels() {
		labels[key] = value
	}
	object.SetLabels(labels)
	if status, found := current.Object["status"]; found {
		object.Object["status"] = runtime.DeepCopyJSONValue(status)
	}
	_, err = objects.Update(ctx, object, metav1.UpdateOptions{})
	return err
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
