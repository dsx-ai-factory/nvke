// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// The controller watches LaunchLayerMachine, read as unstructured because
// CAPLL's Go API is not resolvable through the public module proxy yet.
const (
	launchLayerAPIGroup    = "infrastructure.cluster.x-k8s.io"
	launchLayerAPIVersion  = "v1alpha1"
	launchLayerMachineKind = "LaunchLayerMachine"

	// launchLayerSpireAgentPathRequeue covers what the path is built from
	// but no LaunchLayerMachine event announces, such as the LaunchLayerCluster
	// recording its VPC's organization.
	launchLayerSpireAgentPathRequeue = 30 * time.Second
)

func newLaunchLayerMachine() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: launchLayerAPIGroup, Version: launchLayerAPIVersion, Kind: launchLayerMachineKind})
	return obj
}

type launchLayerSpiffeReader interface {
	GetNode(ctx context.Context, id uuid.UUID) (db.Node, error)
	GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error)
}

// LaunchLayerSpiffeController writes the SPIRE agent ID path onto a
// LaunchLayer-backed Machine, so MachineSpireEntryController registers its
// ClusterStaticEntry before the node joins. It is the LaunchLayer counterpart
// of NicoSpiffeController; the path itself comes from the cluster's
// SpireAgentPathProvider, resolved through its ProviderClient.
type LaunchLayerSpiffeController struct {
	db        launchLayerSpiffeReader
	k8sClient client.Client
	logger    *slog.Logger
}

func NewLaunchLayerSpiffeController(database launchLayerSpiffeReader, k8sClient client.Client, logger *slog.Logger) *LaunchLayerSpiffeController {
	return &LaunchLayerSpiffeController{
		db:        database,
		k8sClient: k8sClient,
		logger:    logger,
	}
}

// LaunchLayerMachineCRDInstalled reports whether the management cluster serves
// the LaunchLayerMachine CRD.
func LaunchLayerMachineCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	return crdInstalled(mapper, schema.GroupVersion{Group: launchLayerAPIGroup, Version: launchLayerAPIVersion}, launchLayerMachineKind)
}

func (c *LaunchLayerSpiffeController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(newLaunchLayerMachine()).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(launchLayerMachineRequestsForMachine),
			builder.WithPredicates(machineHostIDWatchPredicate()),
		).
		Named("launchlayer-spiffe").
		Complete(c)
}

func launchLayerMachineRequestsForMachine(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok || machine.Spec.InfrastructureRef.APIGroup != launchLayerAPIGroup ||
		machine.Spec.InfrastructureRef.Kind != launchLayerMachineKind || machine.Spec.InfrastructureRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}}}
}

func (c *LaunchLayerSpiffeController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("namespace", req.Namespace, "launchLayerMachineName", req.Name)

	launchLayerMachine := newLaunchLayerMachine()
	if err := c.k8sClient.Get(ctx, req.NamespacedName, launchLayerMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get launchlayer machine: %w", err)
	}
	if launchLayerMachine.GetDeletionTimestamp() != nil {
		logger.Info("LaunchLayerMachine is being deleted, skipping")
		return ctrl.Result{}, nil
	}

	machine, err := util.GetOwnerMachine(ctx, c.k8sClient, metav1.ObjectMeta{
		Namespace:       launchLayerMachine.GetNamespace(),
		OwnerReferences: launchLayerMachine.GetOwnerReferences(),
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner machine: %w", err)
	}
	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerRef on LaunchLayerMachine")
		return ctrl.Result{}, nil
	}
	hostID := keys.MachineHostID.Value(machine.GetAnnotations())
	if hostID == "" {
		logger.Debug("Machine has no host-id annotation, skipping SPIRE agent path reconciliation")
		return ctrl.Result{}, nil
	}
	nodeID, err := uuid.Parse(hostID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid host ID %q on Machine %s/%s: %w", hostID, machine.Namespace, machine.Name, err)
	}
	node, err := c.db.GetNode(ctx, nodeID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get node %s for Machine %s/%s: %w", nodeID, machine.Namespace, machine.Name, err)
	}
	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get cluster %s for node %s: %w", node.ClusterID, nodeID, err)
	}
	provider, err := clients.GetProviderClient(cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	spireAgentSpiffeIDPath, err := provider.SpireAgentPathProvider().SpireAgentPath(ctx, c.k8sClient, node, machine)
	if errors.Is(err, shared.ErrSpireAgentPathPending) {
		logger.Info("Waiting for the SPIRE agent ID path", "reason", err)
		return ctrl.Result{RequeueAfter: launchLayerSpireAgentPathRequeue}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if spireAgentSpiffeIDPath == "" {
		logger.Info("Node has no SPIRE agent ID path to record", "attestationMethod", node.SpireAttestationMethod)
		return ctrl.Result{}, nil
	}
	if existingPath := keys.MachineSpireAgentSpiffeIDPath.Value(machine.GetAnnotations()); existingPath != "" {
		if existingPath != spireAgentSpiffeIDPath {
			logger.Info("Machine already has a SPIRE agent ID path for another attestation identity, skipping annotation update")
		}
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
	if err := c.k8sClient.Patch(ctx, applyMachine, client.Apply, client.FieldOwner(constants.FieldManagerLaunchLayerSpiffe), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply machine: %w", err)
	}

	return ctrl.Result{}, nil
}
