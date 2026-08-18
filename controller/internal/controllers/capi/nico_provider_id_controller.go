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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/kube"
)

const (
	// Requeue interval while waiting for kubeadm init/join to complete.
	tenantClusterReadinessRetryAfter = 30 * time.Second
)

// NicoProviderIDController fills the small cloud-controller-manager-style gap
// between the management cluster and the tenant cluster. CAPI learns the NICo
// instance identity from NicoMachine.spec.providerID, but kubelet registers the
// tenant Node without that providerID. This controller copies the identity onto
// the tenant Node so CAPI can set Machine.status.nodeRef and transition to Ready.
type NicoProviderIDController struct {
	k8sClient            client.Client
	tenantClientProvider kube.TenantClientProvider
	logger               *slog.Logger
}

func NewNicoProviderIDController(
	k8sClient client.Client,
	tenantClientProvider kube.TenantClientProvider,
	logger *slog.Logger,
) *NicoProviderIDController {
	return &NicoProviderIDController{
		k8sClient:            k8sClient,
		tenantClientProvider: tenantClientProvider,
		logger:               logger,
	}
}

func (c *NicoProviderIDController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nicov1.NicoMachine{}).
		Named("nico-provider-id").
		Complete(c)
}

func (c *NicoProviderIDController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("namespace", req.Namespace, "nicoMachineName", req.Name)

	nicoMachine := &nicov1.NicoMachine{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get nico machine: %w", err)
	}
	logger = logger.With("resourceVersion", nicoMachine.ResourceVersion)

	if !nicoMachine.DeletionTimestamp.IsZero() {
		logger.Info("NicoMachine is being deleted, skipping provider ID reconciliation")
		return ctrl.Result{}, nil
	}
	if nicoMachine.Spec.ProviderID == "" {
		logger.Info("Waiting for providerID to be set on NicoMachine")
		return ctrl.Result{}, nil
	}

	// The owner Machine anchors the CAPI cluster relationship and defines the
	// tenant Node name used by NKE bootstrap.
	machine, err := capiutil.GetOwnerMachine(ctx, c.k8sClient, nicoMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner machine: %w", err)
	}
	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerRef on NicoMachine")
		return ctrl.Result{}, nil
	}
	if !machine.DeletionTimestamp.IsZero() {
		logger.Info("Machine is being deleted, skipping provider ID reconciliation", "machineName", machine.Name)
		return ctrl.Result{}, nil
	}
	if machine.Spec.ClusterName == "" {
		return ctrl.Result{}, fmt.Errorf("machine %s/%s has empty spec.clusterName", machine.Namespace, machine.Name)
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

	if node.Spec.ProviderID != "" && node.Spec.ProviderID != nicoMachine.Spec.ProviderID {
		return ctrl.Result{}, fmt.Errorf(
			"tenant node %s already has providerID %q, expected %q",
			node.Name,
			node.Spec.ProviderID,
			nicoMachine.Spec.ProviderID,
		)
	}

	patch := client.MergeFrom(node.DeepCopy())
	changed := reconcileNodeMetadata(node, nicoMachine)
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

func reconcileNodeMetadata(node *corev1.Node, nicoMachine *nicov1.NicoMachine) bool {
	changed := false
	if node.Spec.ProviderID != nicoMachine.Spec.ProviderID {
		node.Spec.ProviderID = nicoMachine.Spec.ProviderID
		changed = true
	}

	desired := map[string]string{
		constants.LabelKeyMachineID:    nicoMachine.Status.MachineID,
		constants.LabelKeyNodeSiteID:   nicoMachine.Status.SiteID,
		constants.LabelKeyNodeSiteName: kube.NormalizeLabelValue(nicoMachine.Status.SiteName),
		constants.LabelKeyNodeVPCID:    nicoMachine.Status.VPCID,
		constants.LabelKeyNodeVPCName:  kube.NormalizeLabelValue(nicoMachine.Status.VPCName),
	}
	managedTopologyLabelKeys := [...]string{
		constants.LabelKeyMachineID,
		constants.LabelKeyNodeSiteID,
		constants.LabelKeyNodeSiteName,
		constants.LabelKeyNodeVPCID,
		constants.LabelKeyNodeVPCName,
	}
	for _, key := range managedTopologyLabelKeys {
		value := desired[key]
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
