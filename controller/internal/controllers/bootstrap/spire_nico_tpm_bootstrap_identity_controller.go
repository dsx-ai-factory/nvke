// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	spireNicoTPMBootstrapIdentityFinalizer = keys.Domain + "/spire-nico-tpm-bootstrap-identity"
)

// SpireNicoTPMBootstrapIdentityController gives a Machine a temporary SPIRE workload identity for kubeadm join.
// It acts on CAPI Machines whose concrete KubeadmConfig selects the SPIRE NICo TPM bootstrap identity resolver with this label:
//
//	nvke.nvidia.com/bootstrap-identity-resolver: spire-nico-tpm
//
// The workload entry config belongs to the node OS, so it is an annotation on the KubeadmConfig rather than a
// controller setting. One controller reconciles every cluster, and each cluster can ship its own binary path.
// A different node attestor is a different bootstrap-identity-resolver value: this resolver only builds parentID
// from the NicoMachine TPM EK public hash.
//
// Omit the annotation to use the reference OS defaults. A present annotation replaces hint and selectors and
// does not merge with those defaults. Hint may be omitted. Selectors must contain one or more values, and SPIRE
// validates and ANDs them:
//
//	nvke.nvidia.com/bootstrap-identity-spire-workload-entry-config: >
//	  {"hint":"kubeadm-join","selectors":["unix:path:/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth"]}
//
// For a NicoMachine with status.tpmEkPubHash set, it derives the SPIRE agent parent ID:
//
//	spec:
//	  parentID: spiffe://<trust-domain>/spire/agent/tpm/<hash>
//
// It creates a ClusterStaticEntry that allows the kubeadm exec-auth plugin to receive this workload identity:
//
//	spec:
//	  parentID: spiffe://<trust-domain>/spire/agent/tpm/<hash>
//	  spiffeID: spiffe://<trust-domain>/bootstrap/<machine-uid>
//	  selectors:
//	    - unix:path:/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth
//	  hint: kubeadm-join
//
// It publishes the workload SPIFFE ID as nvke.nvidia.com/bootstrap-identity on the Machine. It does not publish
// nvke.nvidia.com/spire-agent-spiffe-id-path. Once CAPI reports a nodeRef, kubeadm join has completed and the
// controller removes the temporary ClusterStaticEntry and bootstrap identity annotation.
type SpireNicoTPMBootstrapIdentityController struct {
	cfg         config.Config
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

// NewSpireNicoTPMBootstrapIdentityController returns a controller that publishes SPIRE NICo TPM bootstrap identities.
func NewSpireNicoTPMBootstrapIdentityController(
	cfg config.Config,
	k8sClient client.Client,
	logger *slog.Logger,
	trustDomain string,
) *SpireNicoTPMBootstrapIdentityController {
	return &SpireNicoTPMBootstrapIdentityController{
		cfg:         cfg,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

// SetupWithManager wires the controller to reconcile Machines, KubeadmConfigs, and NicoMachines that select the resolver.
func (c *SpireNicoTPMBootstrapIdentityController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}).
		Watches(
			&kubeadmv1.KubeadmConfig{},
			handler.EnqueueRequestsFromMapFunc(machineRequestForKubeadmConfig),
		).
		Watches(
			&nicov1.NicoMachine{},
			handler.EnqueueRequestsFromMapFunc(machineRequestForNicoMachine),
			builder.WithPredicates(nicoMachineTPMHashChanged()),
		).
		Named(constants.FieldManagerSpireNicoTPMBootstrapIdentity).
		Complete(c)
}

func machineRequestForKubeadmConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	kubeadmConfig, ok := obj.(*kubeadmv1.KubeadmConfig)
	if !ok {
		return nil
	}
	return machineRequestForControllerOwner(kubeadmConfig)
}

func machineRequestForNicoMachine(ctx context.Context, obj client.Object) []reconcile.Request {
	nicoMachine, ok := obj.(*nicov1.NicoMachine)
	if !ok {
		return nil
	}
	return machineRequestForControllerOwner(nicoMachine)
}

func machineRequestForControllerOwner(obj client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.APIVersion != clusterv1.GroupVersion.String() || owner.Kind != "Machine" || owner.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      owner.Name,
	}}}
}

func nicoMachineTPMHashChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, ok := e.ObjectOld.(*nicov1.NicoMachine)
			if !ok {
				return false
			}
			newMachine, ok := e.ObjectNew.(*nicov1.NicoMachine)
			if !ok {
				return false
			}
			return oldMachine.Status.TpmEkPubHash != newMachine.Status.TpmEkPubHash
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func (c *SpireNicoTPMBootstrapIdentityController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("machineNamespace", req.Namespace, "machineName", req.Name)

	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get machine: %w", err)
	}

	if !machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, c.cleanupBootstrapIdentity(ctx, machine)
	}

	bootstrapSpiffeID := spireBootstrapIdentity(c.trustDomain, machine)

	kubeadmConfig, selected, err := c.kubeadmConfigForMachine(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !selected {
		return ctrl.Result{}, c.cleanupBootstrapIdentity(ctx, machine)
	}

	if machine.Status.NodeRef.IsDefined() {
		// The node has joined the cluster, so temporary join identity is no longer needed.
		return ctrl.Result{}, c.cleanupBootstrapIdentity(ctx, machine)
	}

	workloadEntryConfig, err := spireWorkloadEntryConfigForKubeadmConfig(kubeadmConfig)
	if err != nil {
		// The invalid annotation requires a KubeadmConfig update. Set the condition without requeuing.
		return ctrl.Result{}, SetBootstrapIdentityResolvedCondition(
			ctx,
			c.k8sClient,
			machine,
			metav1.ConditionFalse,
			BootstrapIdentityConfigurationInvalidReason,
			err.Error(),
			constants.FieldManagerSpireNicoTPMBootstrapIdentity,
		)
	}

	parentIDResolution, err := c.resolveSpireAgentParentID(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if parentIDResolution.parentID == "" {
		logger.Info("SPIRE agent parent ID not resolved", "reason", parentIDResolution.reason, "message", parentIDResolution.message)
		return ctrl.Result{}, SetBootstrapIdentityResolvedCondition(
			ctx,
			c.k8sClient,
			machine,
			metav1.ConditionFalse,
			parentIDResolution.reason,
			parentIDResolution.message,
			constants.FieldManagerSpireNicoTPMBootstrapIdentity,
		)
	}

	if err := c.ensureFinalizer(ctx, machine); err != nil {
		return ctrl.Result{}, err
	}

	if err := c.publishBootstrapIdentity(ctx, machine, bootstrapSpiffeID, parentIDResolution.parentID, workloadEntryConfig); err != nil {
		conditionErr := SetBootstrapIdentityResolvedCondition(
			ctx,
			c.k8sClient,
			machine,
			metav1.ConditionFalse,
			BootstrapIdentityFailedReason,
			err.Error(),
			constants.FieldManagerSpireNicoTPMBootstrapIdentity,
		)
		return ctrl.Result{}, errors.Join(err, conditionErr)
	}

	return ctrl.Result{}, SetBootstrapIdentityResolvedCondition(
		ctx,
		c.k8sClient,
		machine,
		metav1.ConditionTrue,
		BootstrapIdentityResolvedReason,
		fmt.Sprintf("Published bootstrap identity %s", bootstrapSpiffeID),
		constants.FieldManagerSpireNicoTPMBootstrapIdentity,
	)
}

func (c *SpireNicoTPMBootstrapIdentityController) kubeadmConfigForMachine(ctx context.Context, machine *clusterv1.Machine) (*kubeadmv1.KubeadmConfig, bool, error) {
	if !capi.IsKubeadmConfigRef(machine.Spec.Bootstrap.ConfigRef) {
		return nil, false, nil
	}

	kubeadmConfig := &kubeadmv1.KubeadmConfig{}
	kubeadmKey := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.Bootstrap.ConfigRef.Name}
	if err := c.k8sClient.Get(ctx, kubeadmKey, kubeadmConfig); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get kubeadm config %s/%s: %w", kubeadmKey.Namespace, kubeadmKey.Name, err)
	}

	selected := keys.BootstrapIdentityResolver.Value(kubeadmConfig.GetLabels()) == constants.LabelValueBootstrapIdentityResolverSpireNicoTPM
	return kubeadmConfig, selected, nil
}

type spireAgentParentIDResolution struct {
	parentID string
	reason   string
	message  string
}

func (c *SpireNicoTPMBootstrapIdentityController) resolveSpireAgentParentID(ctx context.Context, machine *clusterv1.Machine) (spireAgentParentIDResolution, error) {
	if !capi.IsNicoMachineRef(machine.Spec.InfrastructureRef) {
		return spireAgentParentIDResolution{
			reason:  BootstrapIdentityInfrastructureInvalidReason,
			message: "Machine infrastructure ref is not a NicoMachine",
		}, nil
	}

	nicoMachine := &nicov1.NicoMachine{}
	nicoKey := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.InfrastructureRef.Name}
	if err := c.k8sClient.Get(ctx, nicoKey, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return spireAgentParentIDResolution{
				reason:  BootstrapIdentityWaitingForInfrastructureReason,
				message: fmt.Sprintf("Waiting for referenced NicoMachine %s", nicoKey.Name),
			}, nil
		}
		return spireAgentParentIDResolution{}, fmt.Errorf("failed to get nico machine %s/%s: %w", nicoKey.Namespace, nicoKey.Name, err)
	}

	if !capi.IsControlledByMachine(nicoMachine, machine) {
		return spireAgentParentIDResolution{
			reason:  BootstrapIdentityInfrastructureInvalidReason,
			message: fmt.Sprintf("Referenced NicoMachine %s is not controlled by Machine %s", nicoMachine.Name, machine.Name),
		}, nil
	}

	if nicoMachine.Status.TpmEkPubHash == "" {
		return spireAgentParentIDResolution{
			reason:  BootstrapIdentityWaitingForIdentityDataReason,
			message: fmt.Sprintf("Waiting for NICo TPM EK public hash on NicoMachine %s", nicoMachine.Name),
		}, nil
	}

	// Common Example: spiffe://<trust-domain>/spire/agent/tpm/<hash>
	parentID := fmt.Sprintf("spiffe://%s%s%s", c.trustDomain, constants.TPMSpireAgentPath, nicoMachine.Status.TpmEkPubHash)

	return spireAgentParentIDResolution{
		parentID: parentID,
	}, nil
}

func (c *SpireNicoTPMBootstrapIdentityController) publishBootstrapIdentity(ctx context.Context, machine *clusterv1.Machine, bootstrapSpiffeID, parentID string, entryConfig spireWorkloadEntryConfig) error {
	if err := applyClusterStaticEntry(
		ctx,
		c.k8sClient,
		machine,
		c.cfg.SpireClassName,
		bootstrapSpiffeID,
		parentID,
		entryConfig,
		constants.FieldManagerSpireNicoTPMBootstrapIdentity,
	); err != nil {
		return err
	}

	if err := applyBootstrapIdentityAnnotation(
		ctx,
		c.k8sClient,
		machine,
		bootstrapSpiffeID,
		constants.FieldManagerSpireNicoTPMBootstrapIdentity,
	); err != nil {
		return err
	}

	return nil
}

func (c *SpireNicoTPMBootstrapIdentityController) ensureFinalizer(ctx context.Context, machine *clusterv1.Machine) error {
	if controllerutil.ContainsFinalizer(machine, spireNicoTPMBootstrapIdentityFinalizer) {
		return nil
	}

	patchBase := machine.DeepCopy()
	controllerutil.AddFinalizer(machine, spireNicoTPMBootstrapIdentityFinalizer)
	patch := client.MergeFromWithOptions(patchBase, client.MergeFromWithOptimisticLock{})
	if err := c.k8sClient.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("failed to add SPIRE NICo TPM bootstrap identity finalizer: %w", err)
	}
	return nil
}

func (c *SpireNicoTPMBootstrapIdentityController) cleanupBootstrapIdentity(ctx context.Context, machine *clusterv1.Machine) error {
	expectedIdentity := spireBootstrapIdentity(c.trustDomain, machine)
	ownsIdentity := keys.MachineBootstrapIdentity.Value(machine.GetAnnotations()) == expectedIdentity
	hasFinalizer := controllerutil.ContainsFinalizer(machine, spireNicoTPMBootstrapIdentityFinalizer)

	if !ownsIdentity && !hasFinalizer {
		return nil
	}

	if err := deleteClusterStaticEntry(ctx, c.k8sClient, machine); err != nil {
		return err
	}

	if err := removeBootstrapIdentityAndFinalizer(ctx, c.k8sClient, machine, expectedIdentity, spireNicoTPMBootstrapIdentityFinalizer); err != nil {
		return err
	}

	c.logger.Info("Removed SPIRE NICo TPM bootstrap identity state", "machineNamespace", machine.Namespace, "machineName", machine.Name, "clusterStaticEntry", clusterStaticEntryName(machine))
	return nil
}
