// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiexternal "sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// renderInfraMachine renders the caller-supplied CAPI infrastructure machine
// template for a node. profilesDir is unused here — only the NICo provider
// reads from the profiles directory.
func (*Client) renderInfraMachine(_, namespace string, node db.Node, nodeSet db.NodeSet, _ db.NodePool, cluster db.Cluster) (client.Object, error) {
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

// CreateMachine renders and applies the template-backed infra machine, mints
// a SPIRE join token if the node needs one, renders and applies the
// bootstrap secret, and applies the generic CAPI Machine pointing at the
// infra object.
func (c *Client) CreateMachine(ctx context.Context, deps shared.MachineCreateDeps, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) error {
	infraMachineObj, err := c.renderInfraMachine(deps.ProfilesDir, deps.Namespace, node, nodeSet, nodePool, cluster)
	if err != nil {
		return fmt.Errorf("failed to render infra machine template: %w", err)
	}
	infraMachine, ok := infraMachineObj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected unstructured infra machine, got %T", infraMachineObj)
	}
	if err := applyNicoMachineInstanceLabels(node, cluster, nodePool.ID, infraMachine); err != nil {
		return err
	}

	if err := deps.K8sClient.Patch(ctx, infraMachine, client.Apply, client.FieldOwner(deps.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply infra machine %s: %w", infraMachine.GetName(), err)
	}

	spireJoinToken, spireAgentAlternateIDPath, err := deps.CreateSpireJoinToken(ctx, node)
	if err != nil {
		return err
	}

	userData, err := c.renderBootstrapUserData(ctx, deps, node, nodeSet, spireJoinToken)
	if err != nil {
		return fmt.Errorf("failed to render CAPI bootstrap user data: %w", err)
	}

	bootstrapSecret := shared.BuildBootstrapSecret(node, cluster, deps.Namespace, userData)
	if err := deps.K8sClient.Patch(ctx, bootstrapSecret, client.Apply, client.FieldOwner(deps.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}

	infraMachineGV, err := schema.ParseGroupVersion(infraMachine.GetAPIVersion())
	if err != nil {
		return fmt.Errorf("failed to parse infra machine apiVersion %q: %w", infraMachine.GetAPIVersion(), err)
	}
	if infraMachineGV.Group == "" {
		return fmt.Errorf("infra machine apiVersion %q must include an API group", infraMachine.GetAPIVersion())
	}

	machine, err := shared.BuildMachine(node, nodeSet, nodePool, cluster, deps.Namespace, bootstrapSecret.Name, clusterv1.ContractVersionedObjectReference{
		APIGroup: infraMachineGV.Group,
		Kind:     infraMachine.GetKind(),
		Name:     infraMachine.GetName(),
	})
	if err != nil {
		return err
	}
	if spireJoinToken != "" {
		machine.Annotations[constants.MachineAnnotationKeySpireAgentSpiffeIDPath] = spireAgentAlternateIDPath
	}
	if err := shared.ApplyMachine(ctx, deps.K8sClient, deps.FieldManagerName, machine); err != nil {
		return err
	}

	return nil
}

// applyNicoMachineInstanceLabels merges the controller's identity labels onto a
// rendered infra machine, for the edge case where a CAPI-provider cluster's
// own template happens to render a NicoMachine. A no-op for any other kind.
func applyNicoMachineInstanceLabels(node db.Node, cluster db.Cluster, nodePoolID uuid.UUID, infraMachine *unstructured.Unstructured) error {
	gv, err := schema.ParseGroupVersion(infraMachine.GetAPIVersion())
	if err != nil {
		return fmt.Errorf("parse infrastructure machine apiVersion %q: %w", infraMachine.GetAPIVersion(), err)
	}
	if gv.Group != nicov1.GroupVersion.Group || infraMachine.GetKind() != "NicoMachine" {
		return nil
	}

	labels := nico.BuildInstanceLabels(node, cluster, nodePoolID)
	existingLabels, _, err := unstructured.NestedStringMap(infraMachine.Object, "spec", "labels")
	if err != nil {
		return fmt.Errorf("read NicoMachine instance labels: %w", err)
	}
	if existingLabels == nil {
		existingLabels = map[string]string{}
	}
	for key, value := range labels {
		existingLabels[key] = value
	}
	if err := unstructured.SetNestedStringMap(infraMachine.Object, existingLabels, "spec", "labels"); err != nil {
		return fmt.Errorf("set NicoMachine instance labels: %w", err)
	}
	return nil
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
