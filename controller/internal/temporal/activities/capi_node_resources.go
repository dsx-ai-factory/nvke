// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiexternal "sigs.k8s.io/cluster-api/controllers/external"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/spiffe"
)

type CreateCAPIInfraMachineForNodeInput struct {
	NodeID uuid.UUID
}

type WaitForCAPIMachineReadyInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

type ApplyCAPIMachineRebootAnnotationInput struct {
	NodeID uuid.UUID
}

type ApplyCAPIMachineRebootAnnotationResult struct {
	RebootStartedTime time.Time
}

type DeleteCAPIMachineRebootAnnotationInput struct {
	NodeID uuid.UUID
}

type capiMachineCreateContext struct {
	node      db.Node
	nodeSet   db.NodeSet
	nodePool  db.NodePool
	cluster   db.Cluster
	namespace string
}

func (a *Activities) CreateCAPIInfraMachineForNode(ctx context.Context, input CreateCAPIInfraMachineForNodeInput) error {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}
	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return err
	}
	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return err
	}
	if err := a.rejectUnmigratedCluster(ctx, cluster.Name); err != nil {
		return err
	}

	createCtx := &capiMachineCreateContext{
		node:      node,
		nodeSet:   nodeSet,
		nodePool:  nodePool,
		cluster:   cluster,
		namespace: constants.NamespaceNKESystem,
	}

	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	if err != nil || capi.IsEmptyManifest(capiNodeSetProvider.InfraMachineTemplate) {
		return a.createTemplateLessForgeCAPIMachineForNode(ctx, createCtx)
	}

	return a.createTemplateBackedCAPIMachineForNode(ctx, createCtx)
}

func (a *Activities) createTemplateBackedCAPIMachineForNode(ctx context.Context, createCtx *capiMachineCreateContext) error {
	capiNodeSetProvider, err := createCtx.nodeSet.CAPIProvider()
	if err != nil {
		return fmt.Errorf("failed to read node set capi provider: %w", err)
	}
	templateObj, err := capi.ParseManifest(capiNodeSetProvider.InfraMachineTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse infra.machineTemplate: %w", err)
	}

	templateRef := &corev1.ObjectReference{
		APIVersion: templateObj.GetAPIVersion(),
		Kind:       templateObj.GetKind(),
		Name:       templateObj.GetName(),
		Namespace:  createCtx.namespace,
	}
	infraMachine, err := capiexternal.GenerateTemplate(&capiexternal.GenerateTemplateInput{
		Template:    templateObj,
		TemplateRef: templateRef,
		Namespace:   createCtx.namespace,
		Name:        createCtx.node.Name,
		ClusterName: createCtx.cluster.Name,
	})
	if err != nil {
		return fmt.Errorf("failed to render infra machine template: %w", err)
	}
	if err := a.applyNicoMachineInstanceLabels(createCtx, infraMachine); err != nil {
		return err
	}

	if err := a.k8sClient.Patch(ctx, infraMachine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply infra machine %s: %w", infraMachine.GetName(), err)
	}

	spireJoinToken, spireAgentAlternateIDPath, err := a.createCAPISpireJoinToken(ctx, createCtx)
	if err != nil {
		return err
	}

	userData, err := a.renderCAPIBootstrapUserData(ctx, createCtx.nodeSet, spireJoinToken)
	if err != nil {
		return fmt.Errorf("failed to render CAPI bootstrap user data: %w", err)
	}

	bootstrapSecret := buildCAPIBootstrapSecret(createCtx, userData)
	if err := a.k8sClient.Patch(ctx, bootstrapSecret, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}

	infraMachineGV, err := schema.ParseGroupVersion(infraMachine.GetAPIVersion())
	if err != nil {
		return fmt.Errorf("failed to parse infra machine apiVersion %q: %w", infraMachine.GetAPIVersion(), err)
	}
	if infraMachineGV.Group == "" {
		return fmt.Errorf("infra machine apiVersion %q must include an API group", infraMachine.GetAPIVersion())
	}

	machine, err := buildCAPIMachineForNode(createCtx.node, createCtx.nodeSet, createCtx.nodePool, createCtx.cluster, createCtx.namespace, bootstrapSecret.Name, clusterv1.ContractVersionedObjectReference{
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
	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine %s: %w", machine.Name, err)
	}

	return nil
}

func (a *Activities) createCAPISpireJoinToken(ctx context.Context, createCtx *capiMachineCreateContext) (string, string, error) {
	capiNodeSetProvider, err := createCtx.nodeSet.CAPIProvider()
	if err != nil {
		return "", "", fmt.Errorf("failed to read node set capi provider: %w", err)
	}
	if capiNodeSetProvider.SpireAttestationMethod != db.SpireAttestationMethodJoinToken {
		return "", "", nil
	}
	// A site stood up without SPIFFE has no SPIRE server to mint against, and no
	// trust domain to build the alternate ID from. Asking anyway fails with
	// "trust domain is missing", which is not retryable, so the node never gets
	// a CAPI machine and the cluster sits in Provisioning for good.
	if !a.cfg.SpiffeEnabled {
		return "", "", nil
	}

	// The join token is directly included in the agent's generated primary SPIFFE ID, so copying that
	// value into the InfraMachine annotation and the derived ClusterStaticEntry would expose a
	// credential. So instead we specify a non-sensitive alternate SPIFFE ID in CreateJoinToken,
	// which we can safely pass around without exposing the join token.
	spireAgentAlternateIDPath := constants.JoinTokenAlternateSpireAgentPath + createCtx.node.ID.String()
	spireAgentAlternateID := fmt.Sprintf("spiffe://%s%s", a.trustDomain, spireAgentAlternateIDPath)
	spireJoinToken, err := spiffe.CreateJoinToken(ctx, a.cfg, spireAgentAlternateID)
	if err != nil {
		return "", "", fmt.Errorf("failed to create SPIRE server join token: %w", err)
	}

	return spireJoinToken, spireAgentAlternateIDPath, nil
}

func (a *Activities) createTemplateLessForgeCAPIMachineForNode(ctx context.Context, createCtx *capiMachineCreateContext) error {
	// Seed the initial instance type for a control-plane node that CWE did not
	// pre-fill (the Nico/CAPI path leaves selection to Nico). No-op otherwise.
	if err := a.seedControlPlaneInstanceType(ctx, createCtx); err != nil {
		return err
	}

	nicoMachine, err := a.buildNicoMachineForNode(createCtx)
	if err != nil {
		return err
	}
	if err := a.k8sClient.Patch(ctx, nicoMachine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply NicoMachine %s: %w", nicoMachine.Name, err)
	}

	userData, err := a.renderTemplateLessForgeCAPIBootstrapUserData(ctx, createCtx)
	if err != nil {
		return err
	}
	bootstrapSecret := buildCAPIBootstrapSecret(createCtx, userData)
	if err := a.k8sClient.Patch(ctx, bootstrapSecret, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}

	machine, err := buildCAPIMachineForNode(createCtx.node, createCtx.nodeSet, createCtx.nodePool, createCtx.cluster, createCtx.namespace, bootstrapSecret.Name, clusterv1.ContractVersionedObjectReference{
		APIGroup: nicov1.GroupVersion.Group,
		Kind:     "NicoMachine",
		Name:     nicoMachine.Name,
	})
	if err != nil {
		return err
	}
	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine %s: %w", machine.Name, err)
	}

	return nil
}

func (a *Activities) applyNicoMachineInstanceLabels(
	createCtx *capiMachineCreateContext,
	infraMachine *unstructured.Unstructured,
) error {
	gv, err := schema.ParseGroupVersion(infraMachine.GetAPIVersion())
	if err != nil {
		return fmt.Errorf("parse infrastructure machine apiVersion %q: %w", infraMachine.GetAPIVersion(), err)
	}
	if gv.Group != nicov1.GroupVersion.Group || infraMachine.GetKind() != "NicoMachine" {
		return nil
	}

	labels := buildNicoInstanceLabels(
		createCtx.node,
		createCtx.cluster,
		createCtx.nodePool.ID,
	)
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

type DeleteCAPIMachineForNodeInput struct {
	NodeID             uuid.UUID
	MachineHealthIssue *string

	// Optionally wait for the machine and bootstrap secret to be deleted.
	Wait bool
}

func (a *Activities) DeleteCAPIMachineForNode(ctx context.Context, input DeleteCAPIMachineForNodeInput) error {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}
	if err := a.rejectUnmigratedClusterForNode(ctx, input.NodeID); err != nil {
		return err
	}

	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: constants.NamespaceNKESystem,
		},
	}

	// If a MachineHealthIssue is provided, apply the Machine annotation, before deleting the CAPI Machine.
	// The CAPI Machine delete lifecycle ensures that the underlying infraMachine is deleted before
	// the owning CAPI Machine. This allows infrastructure providers to optionally observe this annotation
	// and take appropriate action, such as flagging the instance for repair (Forge/NICo), or simply
	// ignore it (CAPD) and delete the instance as normal.
	if input.MachineHealthIssue != nil {
		logger.Info("Applying machine health issue annotation", "nodeID", input.NodeID, "machine", machine.Name, "machineHealthIssue", input.MachineHealthIssue)
		machine.Annotations = map[string]string{constants.MachineAnnotationKeyMachineHealthIssue: *input.MachineHealthIssue}

		if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWEMachineHealthIssue), client.ForceOwnership); err != nil {
			return fmt.Errorf("failed to apply machine health issue annotation: %w", err)
		}
	}

	logger.Info("Deleting CAPI machine", "nodeID", input.NodeID, "machine", machine.Name)
	if err := a.k8sClient.Delete(ctx, machine); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete machine %s: %w", machine.Name, err)
	}
	if input.Wait {
		if err := waitForObjectDeleted(ctx, a.k8sClient, machine); err != nil {
			return fmt.Errorf("failed waiting for machine %s deletion: %w", machine.Name, err)
		}
	}

	bootstrapSecret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-bootstrap-data", node.Name),
			Namespace: constants.NamespaceNKESystem,
		},
	}
	if err := a.k8sClient.Delete(ctx, bootstrapSecret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}
	if input.Wait {
		if err := waitForObjectDeleted(ctx, a.k8sClient, bootstrapSecret); err != nil {
			return fmt.Errorf("failed waiting for bootstrap secret %s deletion: %w", bootstrapSecret.Name, err)
		}
	}

	return nil
}

func (a *Activities) WaitForCAPIMachineReady(ctx context.Context, input WaitForCAPIMachineReadyInput) (string, error) {
	return a.waitForCAPIMachineReady(ctx, input)
}

func (a *Activities) waitForCAPIMachineReady(ctx context.Context, input WaitForCAPIMachineReadyInput) (string, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return "", fmt.Errorf("failed to get node: %w", err)
	}
	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return "", err
	}
	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return "", err
	}

	machine := &clusterv1.Machine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      node.Name,
	}, machine); err != nil {
		return "", fmt.Errorf("failed to get CAPI machine: %w", err)
	}
	if err := a.updateCAPIMachineWaitNodeCondition(ctx, input.NodeID, machine, input.NodeProvisioningConditionMessage); err != nil {
		return "", err
	}

	ipAddress := firstMachineIPAddress(machine.Status.Addresses)
	if ipAddress == "" {
		// This is a workaround for CAPD, where the IP address does not propagate up to the Machine object
		// until status is provisioned, but CAPD also won't say the infra machine is provisioned until it's
		// able to patch provider ID in the k8s API. We only publish the CAPI kubeconfig secret after the
		// initial control plane node has completed kubeadm init, and we don't init that first CP node until
		// after we have an IP for it (to connect to node agent). So we still have a chicken and egg problem
		// for the first node in CAPD.
		// We work around this by getting the IP address from the infa machine.
		// We could remove the need for this workaround by changing upstream CAPD to do the provider ID
		// patching after the node is marked provisioned, which is the same order that would happen if
		// provider ID were set by e.g. a CCM.
		ipAddress, err = firstInfrastructureRefIPAddress(ctx, a.k8sClient, machine)
		if err != nil {
			return "", fmt.Errorf("failed to get IP address from infrastructure ref for CAPI machine %s: %w", machine.Name, err)
		}
	}
	if ipAddress == "" {
		// Template-backed nodes have no rotation fallback (nil); templateless
		// Nico nodes advance to the next pinned option in place.
		if fb := a.provisioningFallbackFor(nodeSet); fb != nil {
			if _, err := fb.tryNext(ctx, node, nodeSet, machine); err != nil {
				return "", err
			}
		}

		return "", fmt.Errorf("capi machine %s has no IP addresses in status or infrastructure ref", machine.Name)
	}

	// Record the type the infra provider settled on for templateless Nico nodes
	// (which carry no CWE-selected type). Only authoritative once ready, and must
	// never block provisioning; CAPD and template-backed nodes are unaffected.
	var observedInstanceTypeID uuid.UUID
	if node.InstanceTypeID == uuid.Nil {
		observedInstanceTypeID, err = instanceTypeIDFromInfraRef(ctx, a.k8sClient, machine)
		if err != nil {
			logger.Warn("Could not resolve instance type for CAPI machine; skipping instance type stamp",
				"nodeID", input.NodeID, "machine", machine.Name, "error", err)
			observedInstanceTypeID = uuid.Nil
		}
	}

	if err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		nodeForUpdate, err := tx.GetNodeForUpdate(ctx, input.NodeID)
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}
		nodeForUpdate.IP = ipAddress
		// Sticky: only stamp when the node has no type yet, so we never clobber
		// a CWE-selected (Forge) type or an already-recorded value.
		if observedInstanceTypeID != uuid.Nil && nodeForUpdate.InstanceTypeID == uuid.Nil {
			nodeForUpdate.InstanceTypeID = observedInstanceTypeID
		}
		if _, err := tx.UpdateNode(ctx, &nodeForUpdate); err != nil {
			return fmt.Errorf("failed to update node IP address: %w", err)
		}
		return nil
	}); err != nil {
		return "", err
	}

	logger.Info("CAPI machine ready",
		"clusterID", cluster.ID,
		"nodePoolID", nodePool.ID,
		"nodeID", input.NodeID,
		"nodeName", node.Name,
		"ip", ipAddress,
	)

	return ipAddress, nil
}

func (a *Activities) updateCAPIMachineWaitNodeCondition(ctx context.Context, nodeID uuid.UUID, machine *clusterv1.Machine, message string) error {
	// In the legacy NICo path, Pending meant no backing instance existed yet,
	// commonly because capacity was unavailable. For CAPI, providerID is the
	// provider-agnostic signal that backing infrastructure has been allocated.
	if machine.Spec.ProviderID == "" {
		if conditionMessage := capiMachineInfrastructureReadyMessage(machine); conditionMessage != "" {
			message = conditionMessage
		}
		return a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
			NodeID: nodeID,
			Conditions: NodeConditions{
				Pending: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
				Provisioning: &ConditionUpdate{
					Status:  db.ConditionStatusFalse,
					Message: "",
				},
			},
		})
	}

	return a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: NodeConditions{
			Pending: &ConditionUpdate{
				Status:  db.ConditionStatusFalse,
				Message: "",
			},
			Provisioning: &ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	})
}

func capiMachineInfrastructureReadyMessage(machine *clusterv1.Machine) string {
	condition := capiconditions.Get(machine, clusterv1.MachineInfrastructureReadyCondition)
	if condition == nil {
		return ""
	}
	return condition.Message
}

func firstMachineIPAddress(addresses clusterv1.MachineAddresses) string {
	for _, address := range addresses {
		if address.Type == clusterv1.MachineInternalIP && address.Address != "" {
			return address.Address
		}
	}
	for _, address := range addresses {
		if address.Type == clusterv1.MachineExternalIP && address.Address != "" {
			return address.Address
		}
	}
	for _, address := range addresses {
		if address.Address != "" {
			return address.Address
		}
	}
	return ""
}

func firstInfrastructureRefIPAddress(ctx context.Context, k8sClient client.Client, machine *clusterv1.Machine) (string, error) {
	infraObj, err := capiexternal.GetObjectFromContractVersionedRef(ctx, k8sClient, machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		return "", err
	}

	rawAddresses, found, err := unstructured.NestedSlice(infraObj.Object, "status", "addresses")
	if err != nil || !found {
		return "", err
	}

	addresses := make(clusterv1.MachineAddresses, 0, len(rawAddresses))
	for _, rawAddress := range rawAddresses {
		addressMap, ok := rawAddress.(map[string]any)
		if !ok {
			continue
		}
		addressValue, _ := addressMap["address"].(string)
		typeValue, _ := addressMap["type"].(string)
		if addressValue == "" {
			continue
		}
		addresses = append(addresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineAddressType(typeValue),
			Address: addressValue,
		})
	}

	return firstMachineIPAddress(addresses), nil
}

// instanceTypeIDFromInfraRef reads the effective instance type off the Machine's
// infrastructure object (e.g. NicoMachine.spec.instanceTypeID). It is read
// generically from the unstructured infra object so it naturally no-ops for
// providers that do not carry an instance type (CAPD): those return uuid.Nil
// with no error. An empty value returns uuid.Nil so the caller skips stamping.
func instanceTypeIDFromInfraRef(ctx context.Context, k8sClient client.Client, machine *clusterv1.Machine) (uuid.UUID, error) {
	infraObj, err := capiexternal.GetObjectFromContractVersionedRef(ctx, k8sClient, machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		return uuid.Nil, err
	}

	raw, found, err := unstructured.NestedString(infraObj.Object, "spec", "instanceTypeID")
	if err != nil {
		return uuid.Nil, err
	}
	if !found || raw == "" {
		return uuid.Nil, nil
	}

	instanceTypeID, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("infra machine %s has invalid instanceTypeID %q: %w", infraObj.GetName(), raw, err)
	}
	return instanceTypeID, nil
}

func (a *Activities) ApplyCAPIMachineRebootAnnotation(ctx context.Context, input ApplyCAPIMachineRebootAnnotationInput) (ApplyCAPIMachineRebootAnnotationResult, error) {
	logger := activity.GetLogger(ctx)

	rebootTime := time.Now().UTC()
	formattedRebootTime := rebootTime.Format(time.RFC3339)

	logger.Info("Applying CAPI machine reboot annotation", "nodeID", input.NodeID, "rebootTime", formattedRebootTime)
	if err := a.applyCAPIMachineRebootAnnotation(ctx, input.NodeID, &formattedRebootTime); err != nil {
		return ApplyCAPIMachineRebootAnnotationResult{}, fmt.Errorf("failed to apply machine reboot annotation: %w", err)
	}

	return ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: rebootTime}, nil
}

func (a *Activities) DeleteCAPIMachineRebootAnnotation(ctx context.Context, input DeleteCAPIMachineRebootAnnotationInput) error {
	logger := activity.GetLogger(ctx)

	logger.Info("Deleting CAPI machine reboot annotation", "nodeID", input.NodeID)
	if err := a.applyCAPIMachineRebootAnnotation(ctx, input.NodeID, nil); err != nil {
		return fmt.Errorf("failed to delete machine reboot annotation: %w", err)
	}

	return nil
}

func (a *Activities) applyCAPIMachineRebootAnnotation(ctx context.Context, nodeID uuid.UUID, annotation *string) error {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}
	if err := a.rejectUnmigratedClusterForNode(ctx, nodeID); err != nil {
		return err
	}

	namespace := constants.NamespaceNKESystem
	machineName := node.Name

	// Note: With SSA, a field manager can give up ownership of a field by applying an object that no longer includes that field.
	// If no other manager owns that field, Kubernetes removes it. Since FieldManagerCWERebootMachine owns only the reboot annotation,
	// we can simply apply an object with no reboot annotation present at all to delete it.
	annotations := map[string]string{}

	if annotation != nil {
		annotations[constants.MachineAnnotationKeyReboot] = *annotation
	}

	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        machineName,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}

	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWERebootMachine), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine reboot annotation: %w", err)
	}

	return nil
}
