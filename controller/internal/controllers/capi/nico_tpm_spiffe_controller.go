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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	nicov1 "gitlab-master.nvidia.com/nke/cluster-api-provider-nico/api/v1alpha1"
)

type NicoTpmSpiffeController struct {
	cfg       config.Config
	k8sClient client.Client
	logger    *slog.Logger
}

func NewNicoTpmSpiffeController(cfg config.Config, k8sClient client.Client, logger *slog.Logger) *NicoTpmSpiffeController {
	return &NicoTpmSpiffeController{
		cfg:       cfg,
		k8sClient: k8sClient,
		logger:    logger,
	}
}

func (c *NicoTpmSpiffeController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nicov1.NicoMachine{}).
		Named("nico-tpm-spiffe").
		Complete(c)
}

func (c *NicoTpmSpiffeController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("namespace", req.Namespace, "nicoMachineName", req.Name)

	// Always reconcile the NicoMachine-derived tpm-pubhashes configmap, so that the SPIRE server has the latest set of pubhashes.
	// Even if the NicoMachine that caused the reconcile is not ready, has no TPM EK public hash set yet, is being or has been deleted.
	if err := c.reconcileTpmPubhashesConfigmap(ctx, logger); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile tpm pubhashes configmap: %w", err)
	}

	nicoMachine := &nicov1.NicoMachine{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get nico machine: %w", err)
	}
	logger = logger.With("resourceVersion", nicoMachine.ResourceVersion)

	if !nicoMachine.DeletionTimestamp.IsZero() {
		logger.Info("NicoMachine is being deleted, skipping")
		return ctrl.Result{}, nil
	}

	if nicoMachine.Status.TpmEkPubHash == "" {
		logger.Info("Waiting for TPM EK public hash to be set on NicoMachine")
		return ctrl.Result{}, nil
	}

	machine, err := util.GetOwnerMachine(ctx, c.k8sClient, nicoMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner machine: %w", err)
	}
	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerRef on NicoMachine")
		return ctrl.Result{}, nil
	}

	// If the annotation already exists and does not start with the TPM spire agent path (e.g. join-token based path), skip the update.
	existingSpireAgentSpiffeIDPath := machine.GetAnnotations()[constants.MachineAnnotationKeySpireAgentSpiffeIDPath]
	if existingSpireAgentSpiffeIDPath != "" && !strings.HasPrefix(existingSpireAgentSpiffeIDPath, constants.TPMSpireAgentPath) {
		logger.Info("Machine already has non-TPM spire agent ID path, skipping annotation update")
		return ctrl.Result{}, nil
	}

	// e.g. /spire/agent/tpm/<pubhash>
	spireAgentSpiffeIDPath := fmt.Sprintf("%s%s", constants.TPMSpireAgentPath, nicoMachine.Status.TpmEkPubHash)

	// Minimal Machine Object for SSA
	applyMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      machine.Name,
			Namespace: machine.Namespace,
			Annotations: map[string]string{
				constants.MachineAnnotationKeySpireAgentSpiffeIDPath: spireAgentSpiffeIDPath,
			},
		},
	}

	logger.Info("Applying Machine annotation", "machineNamespace", applyMachine.Namespace, "machineName", applyMachine.Name, "annotation", constants.MachineAnnotationKeySpireAgentSpiffeIDPath)
	if err := c.k8sClient.Patch(ctx, applyMachine, client.Apply, client.FieldOwner(constants.FieldManagerNicoTpmSpiffe), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply machine: %w", err)
	}

	return ctrl.Result{}, nil
}

func (c *NicoTpmSpiffeController) reconcileTpmPubhashesConfigmap(ctx context.Context, logger *slog.Logger) error {
	// Compute the full NicoMachine-derived pubhash set on every reconcile and apply it
	// with a dedicated field manager. Multiple NicoMachines can trigger this controller,
	// and applying only the current machine's key would cause those reconciles to
	// overwrite each other via SSA.
	nicoMachines := &nicov1.NicoMachineList{}
	if err := c.k8sClient.List(ctx, nicoMachines); err != nil {
		return fmt.Errorf("failed to list nico machines: %w", err)
	}

	data := map[string]string{}
	for _, nicoMachine := range nicoMachines.Items {
		if nicoMachine.Status.TpmEkPubHash == "" {
			continue
		}
		if !nicoMachine.DeletionTimestamp.IsZero() {
			continue
		}
		data[nicoMachine.Status.TpmEkPubHash] = fmt.Sprintf("%s=%s", constants.LabelKeyNkeManagedBy, constants.FieldManagerNicoTpmSpiffe)
	}

	applyTpmPubhashesConfigmap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.cfg.SpireTPMPubhashesConfigMapName,
			Namespace: c.cfg.SpireServerNamespace,
		},
		Data: data,
	}

	logger.Info("Applying tpm pubhashes configmap")
	if err := c.k8sClient.Patch(ctx, applyTpmPubhashesConfigmap, client.Apply, client.FieldOwner(constants.FieldManagerNicoTpmSpiffe), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply tpm pubhashes configmap: %w", err)
	}

	return nil
}
