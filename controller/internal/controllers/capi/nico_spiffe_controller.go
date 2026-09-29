// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
)

type NicoSpiffeController struct {
	cfg       config.Config
	k8sClient client.Client
	logger    *slog.Logger
}

func NewNicoSpiffeController(cfg config.Config, k8sClient client.Client, logger *slog.Logger) *NicoSpiffeController {
	return &NicoSpiffeController{
		cfg:       cfg,
		k8sClient: k8sClient,
		logger:    logger,
	}
}

func (c *NicoSpiffeController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nicov1.NicoMachine{}).
		Named("nico-spiffe").
		Complete(c)
}

func (c *NicoSpiffeController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		logger.Info("NicoMachine is being deleted, skipping")
		return ctrl.Result{}, nil
	}

	spireAgentSpiffeIDPath, expectedPathPrefix := c.spireAgentSpiffeIDPath(nicoMachine)
	if spireAgentSpiffeIDPath == "" {
		logger.Info("Waiting for SPIRE agent identity source on NicoMachine", "attestationMethod", c.cfg.NodeAgent.SpireAttestationMethod)
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

	existingPath := machine.GetAnnotations()[constants.MachineAnnotationKeySpireAgentSpiffeIDPath]
	if existingPath != "" && !strings.HasPrefix(existingPath, expectedPathPrefix) {
		logger.Info("Machine already has a SPIRE agent ID path for another attestation method, skipping annotation update")
		return ctrl.Result{}, nil
	}

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
	if err := c.k8sClient.Patch(ctx, applyMachine, client.Apply, client.FieldOwner(constants.FieldManagerNicoSpiffe), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply machine: %w", err)
	}

	return ctrl.Result{}, nil
}

func (c *NicoSpiffeController) spireAgentSpiffeIDPath(nicoMachine *nicov1.NicoMachine) (string, string) {
	switch c.cfg.NodeAgent.SpireAttestationMethod {
	case config.SpireAttestationMethodJoinToken:
		return "", ""
	case config.SpireAttestationMethodTPM:
		if nicoMachine.Status.TpmEkPubHash == "" {
			return "", constants.TPMSpireAgentPath
		}
		return constants.TPMSpireAgentPath + nicoMachine.Status.TpmEkPubHash, constants.TPMSpireAgentPath
	case config.SpireAttestationMethodDSXIMDS:
		if nicoMachine.Status.PrimaryDPUMachineID == "" {
			return "", constants.DSXIMDSSpireAgentPath
		}
		pathPrefix := constants.DSXIMDSSpireAgentPath + c.cfg.NodeAgent.DSXImds.OperatorTenant + "/bm/"
		return pathPrefix + nicoMachine.Status.PrimaryDPUMachineID, pathPrefix
	default:
		return "", ""
	}
}
