// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/kube"
)

const (
	// Requeue interval while waiting for kubeadm init/join to complete.
	tenantClusterReadinessRetryAfter = 30 * time.Second

	nicoMachineKind = "NicoMachine"
)

// MachineProviderIDController fills the small cloud-controller-manager-style
// gap between the management cluster and the tenant cluster. Cluster API learns
// the instance identity from the providerID of the infrastructure machine, but
// kubelet registers the tenant Node without that providerID. This controller
// copies the identity onto the tenant Node so Cluster API can set
// Machine.status.nodeRef and transition to Ready.
//
// The controller serves every infrastructure provider. It watches the core
// Machine and reads spec.providerID from it, which the Cluster API machine
// controller copies from the infrastructure machine of any provider. Nothing
// here is specific to NICo, to the preprovisioned provider, or to CAPD. A
// provider that already writes the providerID onto the tenant Node, such as
// CAPD, writes the same value, so this controller then finds nothing to change.
//
// Only the Forge topology labels are provider-specific. They come from the
// status of a NicoMachine, so they stay behind a check of the referenced kind.
type MachineProviderIDController struct {
	k8sClient            client.Client
	tenantClientProvider kube.TenantClientProvider
	logger               *slog.Logger

	// watchNicoMachines adds a watch on NicoMachine objects. The Forge topology
	// labels come from the status of a NicoMachine, which CAPNICo fills in
	// after it sets the providerID, so a watch on the Machine alone can miss
	// that later update. The watch stays optional because a deployment without
	// the NICo provider has no such CRD, and a watch on an unknown kind stops
	// the manager from starting.
	watchNicoMachines bool
}

func NewMachineProviderIDController(
	k8sClient client.Client,
	tenantClientProvider kube.TenantClientProvider,
	logger *slog.Logger,
	watchNicoMachines bool,
) *MachineProviderIDController {
	return &MachineProviderIDController{
		k8sClient:            k8sClient,
		tenantClientProvider: tenantClientProvider,
		logger:               logger,
		watchNicoMachines:    watchNicoMachines,
	}
}

func (c *MachineProviderIDController) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}).
		Named("machine-provider-id")

	if c.watchNicoMachines {
		builder = builder.Watches(
			&nicov1.NicoMachine{},
			handler.EnqueueRequestsFromMapFunc(infraMachineToOwnerMachine),
		)
	}

	return builder.Complete(c)
}

// infraMachineToOwnerMachine maps an infrastructure machine to the Machine that
// owns it. Cluster API names the infrastructure machine after the Machine, and
// the owner reference confirms the link.
func infraMachineToOwnerMachine(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ownerRef := range obj.GetOwnerReferences() {
		if ownerRef.Kind != machineConst {
			continue
		}
		groupVersion, err := schema.ParseGroupVersion(ownerRef.APIVersion)
		if err != nil || groupVersion.Group != clusterv1.GroupVersion.Group {
			continue
		}
		return []reconcile.Request{{
			NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: ownerRef.Name},
		}}
	}
	return nil
}

func (c *MachineProviderIDController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("namespace", req.Namespace, "machineName", req.Name)

	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get machine: %w", err)
	}
	logger = logger.With("resourceVersion", machine.ResourceVersion)

	if !machine.DeletionTimestamp.IsZero() {
		logger.Info("Machine is being deleted, skipping provider ID reconciliation")
		return ctrl.Result{}, nil
	}
	if machine.Spec.ClusterName == "" {
		return ctrl.Result{}, fmt.Errorf("machine %s/%s has empty spec.clusterName", machine.Namespace, machine.Name)
	}

	// The Cluster API machine controller copies this value from the
	// infrastructure machine of whichever provider backs this Machine, so no
	// provider-specific read is needed here.
	providerID := machine.Spec.ProviderID
	if providerID == "" {
		logger.Info("Waiting for providerID to be set on the Machine")
		return ctrl.Result{}, nil
	}

	cluster := &clusterv1.Cluster{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.ClusterName,
	}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("CAPI Cluster no longer exists, skipping provider ID reconciliation", "clusterName", machine.Spec.ClusterName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get CAPI cluster: %w", err)
	}

	// Forge topology labels belong to the NICo provider. For any other provider
	// they are not this controller's to manage, so they are left untouched
	// rather than reconciled towards an empty value.
	manageTopologyLabels := isNicoBacked(machine)
	var topologyLabels map[string]string
	if manageTopologyLabels {
		labels, err := c.forgeTopologyLabels(ctx, machine)
		if err != nil {
			return ctrl.Result{}, err
		}
		topologyLabels = labels
	}

	tenantClient, err := c.tenantClientProvider.ClientForCluster(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Waiting for CAPI kubeconfig secret", "clusterName", cluster.Name)
			return ctrl.Result{RequeueAfter: tenantClusterReadinessRetryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to create tenant cluster client: %w", err)
	}

	// In the current NKE CAPI path, CWE creates Machine.Name from db.Node.Name
	// and CWE passes that same value to kubeadm as nodeRegistration.name.
	node := &corev1.Node{}
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: machine.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Waiting for tenant Node to exist", "nodeName", machine.Name)
			return ctrl.Result{RequeueAfter: tenantClusterReadinessRetryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get tenant node: %w", err)
	}

	if node.Spec.ProviderID != "" && node.Spec.ProviderID != providerID {
		return ctrl.Result{}, fmt.Errorf(
			"tenant node %s already has providerID %q, expected %q",
			node.Name,
			node.Spec.ProviderID,
			providerID,
		)
	}

	patch := client.MergeFrom(node.DeepCopy())
	changed := reconcileNodeMetadata(node, providerID, topologyLabels, manageTopologyLabels)
	if !changed {
		logger.Debug("Tenant Node already has expected providerID and topology labels", "nodeName", node.Name)
		return ctrl.Result{}, nil
	}
	if err := tenantClient.Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch tenant node metadata: %w", err)
	}

	logger.Info("Patched tenant Node metadata", "nodeName", node.Name, "providerID", node.Spec.ProviderID)
	return ctrl.Result{}, nil
}

// isNicoBacked reports whether a NicoMachine backs this Machine, which is the
// only case that carries Forge topology.
func isNicoBacked(machine *clusterv1.Machine) bool {
	infraRef := machine.Spec.InfrastructureRef
	return infraRef.Kind == nicoMachineKind && infraRef.APIGroup == nicov1.GroupVersion.Group
}

// forgeTopologyLabels reads the Forge topology off the NicoMachine that backs a
// Machine. A NicoMachine that Cluster API already deleted yields an empty set:
// the Machine still carries the providerID, so the node metadata is reconciled
// without the topology labels rather than not at all.
func (c *MachineProviderIDController) forgeTopologyLabels(
	ctx context.Context,
	machine *clusterv1.Machine,
) (map[string]string, error) {
	infraRef := machine.Spec.InfrastructureRef

	nicoMachine := &nicov1.NicoMachine{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      infraRef.Name,
	}, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, infraRef.Name, err)
	}

	return map[string]string{
		constants.LabelKeyMachineID:    nicoMachine.Status.MachineID,
		constants.LabelKeyNodeSiteID:   nicoMachine.Status.SiteID,
		constants.LabelKeyNodeSiteName: kube.NormalizeLabelValue(nicoMachine.Status.SiteName),
		constants.LabelKeyNodeVPCID:    nicoMachine.Status.VPCID,
		constants.LabelKeyNodeVPCName:  kube.NormalizeLabelValue(nicoMachine.Status.VPCName),
	}, nil
}

// reconcileNodeMetadata writes the providerID onto the tenant Node, and the
// Forge topology labels when this controller manages them. With manageTopology
// false every label is left untouched.
func reconcileNodeMetadata(
	node *corev1.Node,
	providerID string,
	topologyLabels map[string]string,
	manageTopology bool,
) bool {
	changed := false
	if node.Spec.ProviderID != providerID {
		node.Spec.ProviderID = providerID
		changed = true
	}

	if !manageTopology {
		return changed
	}

	managedTopologyLabelKeys := [...]string{
		constants.LabelKeyMachineID,
		constants.LabelKeyNodeSiteID,
		constants.LabelKeyNodeSiteName,
		constants.LabelKeyNodeVPCID,
		constants.LabelKeyNodeVPCName,
	}
	for _, key := range managedTopologyLabelKeys {
		value := topologyLabels[key]
		if value == "" {
			if _, exists := node.Labels[key]; exists {
				delete(node.Labels, key)
				changed = true
			}
			continue
		}
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		if node.Labels[key] != value {
			node.Labels[key] = value
			changed = true
		}
	}

	return changed
}
