// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeletcsr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	kubeletServingCSRWatchName = "kubelet-serving-csr"

	machineNodeRefNameField = "machine.nodeRefName"
	servingCSRUsernameField = "csr.kubeletServingUsername"

	validateCSRAction = "ValidateCSR"
)

// ServingCSRController approves kubelet serving certificates for Machines that opt in after joining.
type ServingCSRController struct {
	k8sClient    client.Client
	clusterCache clustercache.ClusterCache
	controller   controller.Controller
	recorder     events.EventRecorder
	logger       *slog.Logger
}

func NewServingCSRController(k8sClient client.Client, clusterCache clustercache.ClusterCache, logger *slog.Logger) *ServingCSRController {
	if logger == nil {
		logger = slog.Default()
	}
	return &ServingCSRController{
		k8sClient:    k8sClient,
		clusterCache: clusterCache,
		logger:       logger,
	}
}

func (c *ServingCSRController) SetupWithManager(mgr ctrl.Manager) error {
	c.recorder = mgr.GetEventRecorder(constants.FieldManagerKubeletServingCSR)
	if err := indexMachinesByNodeRefName(context.Background(), mgr.GetCache()); err != nil {
		return err
	}
	built, err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}, builder.WithPredicates(servingMachinePredicate())).
		WatchesRawSource(c.clusterCache.GetClusterSource(kubeletServingCSRWatchName, c.clusterToMachineRequests)).
		Named(constants.FieldManagerKubeletServingCSR).
		Build(c)
	if err != nil {
		return fmt.Errorf("setting up kubelet serving CSR controller: %w", err)
	}
	c.controller = built
	return nil
}

func servingMachinePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return machineOptsIntoServingCSR(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, oldOK := e.ObjectOld.(*clusterv1.Machine)
			newMachine, newOK := e.ObjectNew.(*clusterv1.Machine)
			if !oldOK || !newOK {
				return false
			}
			oldSelected := machineOptsIntoServingCSR(oldMachine)
			newSelected := machineOptsIntoServingCSR(newMachine)
			if !oldSelected && !newSelected {
				return false
			}
			return oldSelected != newSelected ||
				oldMachine.DeletionTimestamp.IsZero() != newMachine.DeletionTimestamp.IsZero() ||
				oldMachine.Spec.ClusterName != newMachine.Spec.ClusterName ||
				oldMachine.Labels[clusterv1.ClusterNameLabel] != newMachine.Labels[clusterv1.ClusterNameLabel] ||
				oldMachine.Status.NodeRef != newMachine.Status.NodeRef ||
				!slices.Equal(oldMachine.Status.Addresses, newMachine.Status.Addresses)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func machineOptsIntoServingCSR(obj client.Object) bool {
	machine, ok := obj.(*clusterv1.Machine)
	return ok && keys.ApproveKubeletServingCSRs.Value(machine.Labels) == "true"
}

func indexMachinesByNodeRefName(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &clusterv1.Machine{}, machineNodeRefNameField, extractMachineNodeRefName); err != nil {
		return fmt.Errorf("indexing Machines by nodeRef name: %w", err)
	}
	return nil
}

func extractMachineNodeRefName(obj client.Object) []string {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok || !machine.Status.NodeRef.IsDefined() {
		return nil
	}
	return []string{machine.Status.NodeRef.Name}
}

// ServingCSRCacheIndexes are added to each tenant cluster cache as it connects.
// The username index lets a reconcile list one identity's CSRs without listing the tenant.
func ServingCSRCacheIndexes() []clustercache.CacheOptionsIndex {
	return []clustercache.CacheOptionsIndex{
		{
			Object:       &certificatesv1.CertificateSigningRequest{},
			Field:        servingCSRUsernameField,
			ExtractValue: extractServingCSRUsername,
		},
	}
}

func extractServingCSRUsername(obj client.Object) []string {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
	if !ok || !isNodeKubeletServingCSR(obj) {
		return nil
	}
	return []string{csr.Spec.Username}
}

func isNodeKubeletServingCSR(obj client.Object) bool {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
	if !ok {
		return false
	}
	return csr.DeletionTimestamp.IsZero() &&
		csr.Spec.SignerName == certificatesv1.KubeletServingSignerName &&
		isKubeletNodeUsername(csr.Spec.Username)
}

func servingCSRPredicate() predicate.TypedPredicate[client.Object] {
	return predicate.TypedFuncs[client.Object]{
		CreateFunc:  func(e event.TypedCreateEvent[client.Object]) bool { return isNodeKubeletServingCSR(e.Object) },
		UpdateFunc:  func(e event.TypedUpdateEvent[client.Object]) bool { return isNodeKubeletServingCSR(e.ObjectNew) },
		DeleteFunc:  func(event.TypedDeleteEvent[client.Object]) bool { return false },
		GenericFunc: func(e event.TypedGenericEvent[client.Object]) bool { return isNodeKubeletServingCSR(e.Object) },
	}
}

func (c *ServingCSRController) clusterToMachineRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	list := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, list,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel:              cluster.Name,
			keys.ApproveKubeletServingCSRs.String(): "true",
		},
	); err != nil {
		c.logger.Error("listing Machines for kubelet serving CSR", "cluster", client.ObjectKeyFromObject(cluster), "error", err)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

//nolint:gocyclo,cyclop // The reconcile order is the behavior. Splitting it hides which outcome wins.
func (c *ServingCSRController) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, request.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting Machine %s: %w", request.NamespacedName, err)
	}
	if !machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !machineOptsIntoServingCSR(machine) {
		return ctrl.Result{}, c.reportOptedOut(ctx, machine)
	}

	joined := machine.Status.NodeRef.IsDefined()
	if !joined {
		return ctrl.Result{}, c.reportCondition(
			ctx, machine,
			metav1.ConditionFalse,
			KubeletServingCSRWaitingForNodeReason,
			"Waiting for Machine node association",
		)
	}

	cluster, err := capiutil.GetClusterFromMetadata(ctx, c.k8sClient, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}

	nodeName := machine.Status.NodeRef.Name
	machinesByNodeRefName, err := c.machinesByNodeRefName(ctx, machine.Namespace, cluster.Name, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(machinesByNodeRefName) != 1 {
		return ctrl.Result{}, c.reportCondition(
			ctx, machine,
			metav1.ConditionFalse,
			KubeletServingCSRNodeNameConflictReason,
			"Node name is not assigned to exactly one Machine",
		)
	}

	if len(machine.Status.Addresses) == 0 {
		return ctrl.Result{}, c.reportCondition(
			ctx, machine,
			metav1.ConditionFalse,
			KubeletServingCSRWaitingForAddressesReason,
			"Waiting for trusted Machine addresses",
		)
	}
	trustedIdentities := trustedServingIdentitiesForMachine(machine)

	// Register the CSR watch from Reconcile, not SetupWithManager. The tenant cache is created when the
	// Cluster infrastructure is provisioned, and clusterCache drops its watches on disconnect.
	// Reconcile re-establishes the Watch. If that name already exists on the current connection, the call is a no-op.
	//
	// A tenant that is not connected yet is not a failure. GetClusterSource, registered in SetupWithManager,
	// enqueues this Machine when the connection is up.
	clusterKey := client.ObjectKeyFromObject(cluster)
	tenantClient, err := c.clusterCache.GetClient(ctx, clusterKey)
	if err != nil {
		return ctrl.Result{}, ignoreNotConnected(fmt.Errorf("getting tenant client for Cluster %s: %w", clusterKey, err))
	}
	if err := c.watchServingCSRs(ctx, clusterKey); err != nil {
		return ctrl.Result{}, ignoreNotConnected(err)
	}

	csrs, err := servingCSRsForMachine(ctx, tenantClient, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	csrDecisions := decideServingCSRs(csrs, nodeName, trustedIdentities)
	c.eventRejectedServingCSRs(machine, csrDecisions.invalid)

	if len(csrDecisions.pending) > 0 {
		if err := approveKubeletCSRs(ctx, tenantClient, machine, csrDecisions.pending, c.logger, c.recorder); err != nil {
			condErr := c.reportCondition(
				ctx, machine,
				metav1.ConditionFalse,
				KubeletServingCSRFailedReason,
				err.Error(),
			)
			return ctrl.Result{}, errors.Join(err, condErr)
		}
	}

	return ctrl.Result{}, c.reportCondition(ctx, machine, metav1.ConditionTrue, KubeletServingCSRApprovalReadyReason, "Ready to approve kubelet serving CSRs")
}

func (c *ServingCSRController) reportCondition(ctx context.Context, machine *clusterv1.Machine, status metav1.ConditionStatus, reason, message string) error {
	return setMachineCondition(
		ctx, c.k8sClient, machine,
		KubeletServingCSRApprovalReadyCondition,
		status,
		reason,
		message,
		constants.FieldManagerKubeletServingCSR,
		c.recorder,
	)
}

// reportOptedOut reports that the Machine is not opted in to kubelet serving CSR approval.
// The condition is only updated to False if it already exists, thus a machine that was never opted in will not have a condition.
func (c *ServingCSRController) reportOptedOut(ctx context.Context, machine *clusterv1.Machine) error {
	if meta.FindStatusCondition(machine.Status.Conditions, KubeletServingCSRApprovalReadyCondition) == nil {
		return nil
	}

	return c.reportCondition(ctx, machine, metav1.ConditionFalse, KubeletServingCSROptedOutReason, "Machine is not opted in to kubelet serving CSR approval")
}

func (c *ServingCSRController) eventRejectedServingCSRs(machine *clusterv1.Machine, invalid []invalidServingCSR) {
	for _, rejected := range invalid {
		c.event(machine, corev1.EventTypeWarning, KubeletServingCSRRejectedReason, validateCSRAction, rejected.csr.Name+": "+rejected.reason)
	}
}

func (c *ServingCSRController) event(machine *clusterv1.Machine, eventType, reason, action, message string) {
	if c.recorder == nil {
		return
	}
	c.recorder.Eventf(machine, nil, eventType, reason, action, "%s", message)
}

func (c *ServingCSRController) machinesByNodeRefName(ctx context.Context, namespace, clusterName, nodeName string) ([]clusterv1.Machine, error) {
	list := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: clusterName,
		},
		client.MatchingFields{machineNodeRefNameField: nodeName},
	); err != nil {
		return nil, fmt.Errorf("listing Machines by nodeRef name for Cluster %s/%s: %w", namespace, clusterName, err)
	}
	return list.Items, nil
}

func (c *ServingCSRController) watchServingCSRs(ctx context.Context, clusterKey client.ObjectKey) error {
	if err := c.clusterCache.Watch(ctx, clusterKey, clustercache.NewWatcher(clustercache.WatcherOptions{
		Name:         kubeletServingCSRWatchName,
		Watcher:      c.controller,
		Kind:         &certificatesv1.CertificateSigningRequest{},
		EventHandler: handler.EnqueueRequestsFromMapFunc(c.requestsForServingCSR(clusterKey)),
		Predicates:   []predicate.TypedPredicate[client.Object]{servingCSRPredicate()},
	})); err != nil {
		return fmt.Errorf("watching kubelet serving CSRs for Cluster %s: %w", clusterKey, err)
	}
	return nil
}

func (c *ServingCSRController) requestsForServingCSR(clusterKey client.ObjectKey) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
		if !ok || !isNodeKubeletServingCSR(obj) {
			return nil
		}
		nodeName := kubeletNodeNameFromUsername(csr.Spec.Username)
		machines, err := c.machinesByNodeRefName(ctx, clusterKey.Namespace, clusterKey.Name, nodeName)
		if err != nil {
			c.logger.Error("listing Machines for kubelet serving CSR", "cluster", clusterKey, "error", err)
			return nil
		}
		return machineRequests(machines)
	}
}
