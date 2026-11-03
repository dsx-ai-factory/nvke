// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
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
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(nicoMachineRequestsForMachine),
			builder.WithPredicates(machineIdentityWatchPredicate()),
		).
		Named("nico-spiffe").
		Complete(c)
}

func machineIdentityWatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return keys.MachineHostID.Value(e.Object.GetAnnotations()) != ""
		},
		// Ignore updates from this controller's own SPIRE agent-path annotation.
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldAnnotations, newAnnotations := e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()
			for _, key := range []keys.Key{keys.MachineHostID, keys.MachineSpireAttestationMethod, keys.MachineSpiffeEnabled} {
				if key.Value(oldAnnotations) != key.Value(newAnnotations) {
					return true
				}
			}
			return false
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func nicoMachineRequestsForMachine(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok || machine.Spec.InfrastructureRef.APIGroup != nicov1.GroupVersion.Group ||
		machine.Spec.InfrastructureRef.Kind != "NicoMachine" || machine.Spec.InfrastructureRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}}}
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

	machine, err := util.GetOwnerMachine(ctx, c.k8sClient, nicoMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner machine: %w", err)
	}
	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerRef on NicoMachine")
		return ctrl.Result{}, nil
	}
	hostID := keys.MachineHostID.Value(machine.GetAnnotations())
	if hostID == "" {
		logger.Debug("Machine has no host-id annotation, skipping SPIRE agent path reconciliation")
		return ctrl.Result{}, nil
	}
	if _, err := uuid.Parse(hostID); err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid host ID %q on Machine %s/%s: %w", hostID, machine.Namespace, machine.Name, err)
	}
	attestationMethod, ok := keys.MachineSpireAttestationMethod.Get(machine.GetAnnotations())
	if !ok {
		logger.Info("Waiting for spire-attestation-method annotation to be set on Machine", "hostID", hostID)
		return ctrl.Result{}, nil
	}
	method := config.SpireAttestationMethod(attestationMethod)
	spireAgentSpiffeIDPath, expectedPathPrefix := c.spireAgentSpiffeIDPath(nicoMachine, method)
	if spireAgentSpiffeIDPath == "" {
		if method == config.SpireAttestationMethodJoinToken {
			return ctrl.Result{}, nil // The join-token path is set when the Machine is created.
		}
		logger.Info("Waiting for SPIRE agent identity source on NicoMachine", "attestationMethod", method)
		return ctrl.Result{}, nil
	}

	existingPath := keys.MachineSpireAgentSpiffeIDPath.Value(machine.GetAnnotations())
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
				keys.MachineSpireAgentSpiffeIDPath.String(): spireAgentSpiffeIDPath,
			},
		},
	}

	logger.Info("Applying Machine annotation", "machineNamespace", applyMachine.Namespace, "machineName", applyMachine.Name, "annotation", keys.MachineSpireAgentSpiffeIDPath.String())
	if err := c.k8sClient.Patch(ctx, applyMachine, client.Apply, client.FieldOwner(constants.FieldManagerNicoSpiffe), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply machine: %w", err)
	}

	return ctrl.Result{}, nil
}

func (c *NicoSpiffeController) spireAgentSpiffeIDPath(nicoMachine *nicov1.NicoMachine, method config.SpireAttestationMethod) (string, string) {
	switch method {
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
