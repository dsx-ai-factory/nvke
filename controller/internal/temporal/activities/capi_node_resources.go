// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiexternal "sigs.k8s.io/cluster-api/controllers/external"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/spiffe"
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

// CreateCAPIInfraMachineForNode resolves the node's provider client via
// clients.GetProviderClient — the only place this flow decides which
// provider it's dealing with — and delegates the entire create (render,
// apply, SPIRE token, bootstrap, generic CAPI Machine) to that client's
// CreateMachine. Provider-specific behavior lives in internal/clients/capi
// and internal/clients/nico, not here.
func (a *Activities) CreateCAPIInfraMachineForNode(ctx context.Context, input CreateCAPIInfraMachineForNodeInput) error {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return nonRetryableIfNotFound("failed to get node", err)
	}
	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return err
	}
	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return err
	}

	provider, err := clients.GetProviderClient(cluster)
	if err != nil {
		return err
	}
	machineClient := provider.MachineMaterialization()

	deps := shared.MachineCreateDeps{
		K8sClient:                    a.k8sClient,
		FieldManagerName:             a.cfg.FieldManagerName,
		ProfilesDir:                  a.cfg.ProfilesDir,
		Namespace:                    a.cfg.ManagementNamespaceOrDefault(),
		Config:                       a.cfg,
		ControllerClientCABundle:     a.controllerClientCABundle,
		GetTrustDomainBundle:         a.getTrustDomainBundle,
		NodeAgentBootstrapFiles:      a.nodeAgentBootstrapFiles,
		CreateSpireJoinToken:         a.createCAPISpireJoinToken,
		SeedControlPlaneInstanceType: a.seedControlPlaneInstanceType,
	}

	if err := machineClient.CreateMachine(ctx, deps, node, nodeSet, nodePool, cluster); err != nil {
		if errors.Is(err, shared.ErrUnsupportedMachineConfig) {
			return temporal.NewNonRetryableApplicationError("failed to create machine", "UnsupportedMachineConfig", err)
		}
		return err
	}
	return nil
}

func (a *Activities) createCAPISpireJoinToken(ctx context.Context, node db.Node) (string, string, error) {
	if node.SpireAttestationMethod != db.SpireAttestationMethodJoinToken {
		return "", "", nil
	}
	// A site stood up without SPIFFE has no SPIRE server to mint against, and no
	// trust domain to build the alternate ID from. Asking anyway fails with
	// "trust domain is missing", which is not retryable, so the node never gets
	// a CAPI machine and the cluster sits in Provisioning for good.
	if !node.SpiffeEnabled {
		return "", "", nil
	}

	// The join token is directly included in the agent's generated primary SPIFFE ID, so copying that
	// value into the InfraMachine annotation and the derived ClusterStaticEntry would expose a
	// credential. So instead we specify a non-sensitive alternate SPIFFE ID in CreateJoinToken,
	// which we can safely pass around without exposing the join token.
	spireAgentAlternateIDPath := constants.JoinTokenAlternateSpireAgentPath + node.ID.String()
	spireAgentAlternateID := fmt.Sprintf("spiffe://%s%s", a.trustDomain, spireAgentAlternateIDPath)
	spireJoinToken, err := spiffe.CreateJoinToken(ctx, a.cfg, spireAgentAlternateID)
	if err != nil {
		return "", "", fmt.Errorf("failed to create SPIRE server join token: %w", err)
	}

	return spireJoinToken, spireAgentAlternateIDPath, nil
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
		return nonRetryableIfNotFound("failed to get node", err)
	}
	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: a.cfg.ManagementNamespaceOrDefault(),
		},
	}

	// If a MachineHealthIssue is provided, apply the Machine annotation, before deleting the CAPI Machine.
	// The CAPI Machine delete lifecycle ensures that the underlying infraMachine is deleted before
	// the owning CAPI Machine. This allows infrastructure providers to optionally observe this annotation
	// and take appropriate action, such as flagging the instance for repair (Forge/NICo), or simply
	// ignore it (CAPD) and delete the instance as normal.
	if input.MachineHealthIssue != nil {
		logger.Info("Applying machine health issue annotation", "nodeID", input.NodeID, "machine", machine.Name, "machineHealthIssue", input.MachineHealthIssue)
		// cluster-api-provider-nico watches exactly one key, named by its
		// --repair-annotation flag, and silently ignores a request under any
		// other, so that flag must name this key.
		machine.Annotations = map[string]string{keys.MachineHealthIssue.String(): *input.MachineHealthIssue}

		if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(a.cfg.FieldManagerMachineHealthIssueName), client.ForceOwnership); err != nil {
			return fmt.Errorf("failed to apply machine health issue annotation: %w", err)
		}
	}

	forgeInstanceID, forgeMachineID := forgeIdentityFromNode(node)
	logger.Info("Deleting CAPI machine",
		"nodeID", input.NodeID,
		"machine", machine.Name,
		"forgeInstanceID", forgeInstanceID,
		"forgeMachineID", forgeMachineID,
	)
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
			Namespace: a.cfg.ManagementNamespaceOrDefault(),
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
		return "", nonRetryableIfNotFound("failed to get node", err)
	}
	nodeSet, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return "", err
	}
	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return "", err
	}
	provider, err := clients.GetProviderClient(cluster)
	if err != nil {
		return "", err
	}

	machine := &clusterv1.Machine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: a.cfg.ManagementNamespaceOrDefault(),
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
		advanceDeps := shared.MachineAdvanceDeps{
			K8sClient:                  a.k8sClient,
			ControlPlaneConfigSnapshot: a.controlPlaneConfigSnapshot,
		}
		if _, err := provider.MachineMaterialization().TryAdvanceStuckMachine(ctx, advanceDeps, node, nodeSet, machine); err != nil {
			return "", err
		}

		return "", fmt.Errorf("capi machine %s has no IP addresses in status or infrastructure ref", machine.Name)
	}

	// Record the type the infra provider settled on for templateless Nico nodes
	// (which carry no controller-selected type). Only authoritative once ready, and must
	// never block provisioning; CAPD and template-backed nodes are unaffected.
	var observedInstanceTypeID uuid.UUID
	if node.NicoInstanceTypeID() == uuid.Nil {
		observedInstanceTypeID, err = instanceTypeIDFromInfraRef(ctx, a.k8sClient, machine)
		if err != nil {
			logger.Warn("Could not resolve instance type for CAPI machine; skipping instance type stamp",
				"nodeID", input.NodeID, "machine", machine.Name, "error", err)
			observedInstanceTypeID = uuid.Nil
		}
	}

	// The provider client reads the backing instance's identity off the
	// infrastructure machine. Lookup must never block provisioning.
	infraMachine, err := capiexternal.GetObjectFromContractVersionedRef(ctx, a.k8sClient, machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		logger.Warn("Could not read infrastructure machine for CAPI machine; skipping identity stamp",
			"nodeID", input.NodeID, "machine", machine.Name, "error", err)
		infraMachine = nil
	}

	if err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		nodeForUpdate, err := tx.GetNodeForUpdate(ctx, input.NodeID)
		if err != nil {
			return nonRetryableIfNotFound("failed to get node", err)
		}
		nodeForUpdate.IP = ipAddress
		// Sticky: only stamp the type when the node has no type yet, so we never
		// clobber a controller-selected (Forge) type or an already-recorded value.
		stampInstanceType := observedInstanceTypeID != uuid.Nil && nodeForUpdate.NicoInstanceTypeID() == uuid.Nil
		if stampInstanceType {
			nico := db.NicoNodeProvider{}
			if nodeForUpdate.Nico != nil {
				nico = *nodeForUpdate.Nico
			}
			nico.InstanceTypeID = observedInstanceTypeID
			nodeForUpdate.SetNico(nico)
		}
		if infraMachine != nil {
			if err := provider.NewNodeProvider().ApplyObservedNodeIdentity(&nodeForUpdate, infraMachine); err != nil {
				logger.Warn("Could not resolve instance identity for CAPI machine; skipping identity stamp",
					"nodeID", input.NodeID, "machine", machine.Name, "error", err)
			}
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
		// Propagated directly, not re-wrapped: applyCAPIMachineRebootAnnotation
		// already adds context, and re-wrapping with fmt.Errorf would strip the
		// non-retryable marking nonRetryableIfNotFound sets for a missing node.
		return ApplyCAPIMachineRebootAnnotationResult{}, err
	}

	return ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: rebootTime}, nil
}

func (a *Activities) DeleteCAPIMachineRebootAnnotation(ctx context.Context, input DeleteCAPIMachineRebootAnnotationInput) error {
	logger := activity.GetLogger(ctx)

	logger.Info("Deleting CAPI machine reboot annotation", "nodeID", input.NodeID)
	if err := a.applyCAPIMachineRebootAnnotation(ctx, input.NodeID, nil); err != nil {
		// Propagated directly for the same reason as ApplyCAPIMachineRebootAnnotation.
		return err
	}

	return nil
}

func (a *Activities) applyCAPIMachineRebootAnnotation(ctx context.Context, nodeID uuid.UUID, annotation *string) error {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return nonRetryableIfNotFound("failed to get node", err)
	}
	cluster, err := a.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return err
	}
	provider, err := clients.GetProviderClient(cluster)
	if err != nil {
		// Deterministic: an unsupported cluster.ProviderType cannot become
		// supported by retrying, so retrying it is pure wasted attempts.
		return temporal.NewNonRetryableApplicationError("failed to get provider client", "UnsupportedProvider", err)
	}
	namespace := a.cfg.ManagementNamespaceOrDefault()
	machineName := node.Name

	// Note: With SSA, a field manager can give up ownership of a field by applying an object that no longer includes that field.
	// If no other manager owns that field, Kubernetes removes it. Since FieldManagerRebootMachineName owns only the reboot annotation,
	// we can simply apply an object with no reboot annotation present at all to delete it.
	annotations := map[string]string{}

	if annotation != nil {
		for _, key := range provider.RebootAnnotationProvider().HardRebootAnnotationKeys() {
			annotations[key] = *annotation
		}
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

	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(a.cfg.FieldManagerRebootMachineName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine reboot annotation: %w", err)
	}

	return nil
}

// forgeIdentityFromNode returns the node's Forge instance and machine IDs, or
// empty strings when the node carries no nico payload.
func forgeIdentityFromNode(node db.Node) (string, string) {
	if node.Nico == nil {
		return "", ""
	}
	return node.Nico.ForgeInstanceID, node.Nico.ForgeMachineID
}
