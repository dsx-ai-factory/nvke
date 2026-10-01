// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	capiexternal "sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// RenderInfraMachine renders the caller-supplied CAPI infrastructure machine
// template for a node. profilesDir is unused here — only the NICo provider
// reads from the profiles directory.
func (*Client) RenderInfraMachine(_, namespace string, node db.Node, nodeSet db.NodeSet, _ db.NodePool, cluster db.Cluster) (client.Object, error) {
	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	if err != nil {
		return nil, fmt.Errorf("failed to read node set capi provider: %w", err)
	}
	templateObj, err := parseManifest(capiNodeSetProvider.InfraMachineTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse infra.machineTemplate: %w", err)
	}

	templateRef := &corev1.ObjectReference{
		APIVersion: templateObj.GetAPIVersion(),
		Kind:       templateObj.GetKind(),
		Name:       templateObj.GetName(),
		Namespace:  namespace,
	}
	infraMachine, err := capiexternal.GenerateTemplate(&capiexternal.GenerateTemplateInput{
		Template:    templateObj,
		TemplateRef: templateRef,
		Namespace:   namespace,
		Name:        node.Name,
		ClusterName: cluster.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render infra machine template: %w", err)
	}
	return infraMachine, nil
}

// parseManifest parses a caller-supplied infra machine template manifest.
// Duplicated from internal/capi.ParseManifest rather than imported: that
// package already imports internal/clients for the ClusterScaffold wrapper,
// so importing it back here would be a cycle.
func parseManifest(raw []byte) (*unstructured.Unstructured, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: obj}
	if u.GetAPIVersion() == "" || u.GetKind() == "" {
		return nil, fmt.Errorf("manifest must include apiVersion and kind")
	}
	return u, nil
}
