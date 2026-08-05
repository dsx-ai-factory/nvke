// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package capi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
)

type MachineSpireEntryController struct {
	cfg         config.Config
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

func NewMachineSpireEntryController(
	cfg config.Config,
	k8sClient client.Client,
	logger *slog.Logger,
	trustDomain string,
) *MachineSpireEntryController {
	return &MachineSpireEntryController{
		cfg:         cfg,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

func (c *MachineSpireEntryController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}).
		Named("machine-spire-entry").
		Complete(c)
}

func (c *MachineSpireEntryController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("machineNamespace", req.Namespace, "machineName", req.Name)

	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get machine: %w", err)
	}

	hostID := machine.GetAnnotations()[constants.MachineAnnotationKeyHostID]
	if !machine.DeletionTimestamp.IsZero() {
		if hostID == "" {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, c.deleteClusterStaticEntry(ctx, hostID)
	}

	if hostID == "" {
		logger.Debug("Machine has no host-id annotation, skipping ClusterStaticEntry reconciliation")
		return ctrl.Result{}, nil
	}

	spireAgentIDPath := machine.GetAnnotations()[constants.MachineAnnotationKeySpireAgentSpiffeIDPath]
	if spireAgentIDPath == "" {
		logger.Info("Waiting for spire agent ID path to be set on Machine", "hostID", hostID)
		return ctrl.Result{}, nil
	}
	parentID := spiffeIDFromPath(c.trustDomain, spireAgentIDPath)

	logger.Info("Applying ClusterStaticEntry for machine", "hostID", hostID, "parentID", parentID)
	cseName := machineSpireEntryName(hostID)
	workloadSpiffeID := fmt.Sprintf("spiffe://%s/host/%s", c.trustDomain, hostID)
	if err := c.applyClusterStaticEntry(ctx, cseName, workloadSpiffeID, parentID); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("ClusterStaticEntry applied for machine", "hostID", hostID, "parentID", parentID)

	return ctrl.Result{}, nil
}

func (c *MachineSpireEntryController) applyClusterStaticEntry(ctx context.Context, name, workloadSpiffeID, parentID string) error {
	cse := &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.GroupVersion.String(),
			Kind:       "ClusterStaticEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.LabelKeyNkeManagedBy: constants.FieldManagerMachineSpireEntry,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: c.cfg.SpireClassName,
			ParentID:  parentID,
			Selectors: []string{"unix:uid:0"},
			SPIFFEID:  workloadSpiffeID,
		},
	}

	if err := c.k8sClient.Patch(
		ctx,
		cse,
		client.Apply,
		client.FieldOwner(constants.FieldManagerMachineSpireEntry),
		client.ForceOwnership,
	); err != nil {
		return fmt.Errorf("failed to apply cluster static entry %s: %w", name, err)
	}

	return nil
}

func (c *MachineSpireEntryController) deleteClusterStaticEntry(ctx context.Context, hostID string) error {
	cse := &spirev1alpha1.ClusterStaticEntry{}
	cse.SetName(machineSpireEntryName(hostID))
	if err := c.k8sClient.Delete(ctx, cse); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cluster static entry %s: %w", cse.GetName(), err)
	}
	return nil
}

func machineSpireEntryName(hostID string) string {
	return fmt.Sprintf("host-%s", hostID)
}

func spiffeIDFromPath(trustDomain, path string) string {
	if strings.HasPrefix(path, "/") {
		return fmt.Sprintf("spiffe://%s%s", trustDomain, path)
	}
	return fmt.Sprintf("spiffe://%s/%s", trustDomain, path)
}
