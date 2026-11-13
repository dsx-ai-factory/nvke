// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	kubeletClientCSRWatchName = "kubelet-client-csr"
)

// Informer index names shared by IndexField and MatchingFields.
// These are local cache map names. They are not annotation keys, and they are not sent to the API server.
const (
	machineBootstrapIdentityField = "machine.bootstrapIdentity"
	machineKubeletNodeNameField   = "machine.kubeletNodeName"
	kubeletClientCSRUsernameField = "csr.kubeletClientUsername"
)

// KubeletClientCSRController approves initial kubelet client CSRs for Machines that opt in during bootstrap.
type KubeletClientCSRController struct {
	k8sClient    client.Client
	clusterCache clustercache.ClusterCache
	controller   controller.Controller
	logger       *slog.Logger
}

func NewKubeletClientCSRController(k8sClient client.Client, clusterCache clustercache.ClusterCache, logger *slog.Logger) *KubeletClientCSRController {
	if logger == nil {
		logger = slog.Default()
	}
	return &KubeletClientCSRController{
		k8sClient:    k8sClient,
		clusterCache: clusterCache,
		logger:       logger,
	}
}

func (c *KubeletClientCSRController) SetupWithManager(mgr ctrl.Manager) error {
	// This controller uses two caches. Index Machines here, in the management cluster cache.
	// Index tenant CSRs separately, in each tenant cluster cache, via KubeletClientCSRCacheIndexes.
	if err := indexKubeletClientCSRMachines(context.Background(), mgr.GetCache()); err != nil {
		return err
	}
	built, err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Machine{}, builder.WithPredicates(kubeletClientCSRMachinePredicate())).
		WatchesRawSource(c.clusterCache.GetClusterSource(kubeletClientCSRWatchName, c.clusterToMachineRequests)).
		Named(constants.FieldManagerKubeletClientCSR).
		Build(c)
	if err != nil {
		return fmt.Errorf("setting up kubelet client CSR controller: %w", err)
	}
	c.controller = built
	return nil
}

func kubeletClientCSRMachinePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return machineOptsIntoKubeletClientCSR(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, oldOK := e.ObjectOld.(*clusterv1.Machine)
			newMachine, newOK := e.ObjectNew.(*clusterv1.Machine)
			if !oldOK || !newOK {
				return false
			}
			oldSelected := machineOptsIntoKubeletClientCSR(oldMachine)
			newSelected := machineOptsIntoKubeletClientCSR(newMachine)
			if !oldSelected && !newSelected {
				return false
			}
			return oldSelected != newSelected ||
				oldMachine.Spec.ClusterName != newMachine.Spec.ClusterName ||
				oldMachine.Labels[clusterv1.ClusterNameLabel] != newMachine.Labels[clusterv1.ClusterNameLabel] ||
				keys.MachineBootstrapIdentity.Value(oldMachine.Annotations) != keys.MachineBootstrapIdentity.Value(newMachine.Annotations) ||
				keys.MachineExpectedNodeName.Value(oldMachine.Annotations) != keys.MachineExpectedNodeName.Value(newMachine.Annotations) ||
				oldMachine.Status.NodeRef != newMachine.Status.NodeRef
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func kubeletClientCSRPredicate() predicate.TypedPredicate[client.Object] {
	return predicate.TypedFuncs[client.Object]{
		CreateFunc:  func(e event.TypedCreateEvent[client.Object]) bool { return isBootstrapKubeletClientCSR(e.Object) },
		UpdateFunc:  func(e event.TypedUpdateEvent[client.Object]) bool { return isBootstrapKubeletClientCSR(e.ObjectNew) },
		DeleteFunc:  func(event.TypedDeleteEvent[client.Object]) bool { return false },
		GenericFunc: func(e event.TypedGenericEvent[client.Object]) bool { return isBootstrapKubeletClientCSR(e.Object) },
	}
}

func machineOptsIntoKubeletClientCSR(obj client.Object) bool {
	machine, ok := obj.(*clusterv1.Machine)
	return ok && keys.ApproveKubeletClientCSR.Value(machine.Labels) == "true"
}

func isKubeletClientCSR(csr *certificatesv1.CertificateSigningRequest) bool {
	return csr.DeletionTimestamp.IsZero() && csr.Spec.SignerName == certificatesv1.KubeAPIServerClientKubeletSignerName
}

func isBootstrapKubeletClientCSR(obj client.Object) bool {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
	if !ok || !isKubeletClientCSR(csr) {
		return false
	}
	username := csr.Spec.Username
	return username != "" && !strings.HasPrefix(username, "system:")
}

func indexKubeletClientCSRMachines(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &clusterv1.Machine{}, machineBootstrapIdentityField, extractMachineBootstrapIdentity); err != nil {
		return fmt.Errorf("indexing Machines by bootstrap identity: %w", err)
	}
	if err := indexer.IndexField(ctx, &clusterv1.Machine{}, machineKubeletNodeNameField, extractMachineKubeletNodeName); err != nil {
		return fmt.Errorf("indexing Machines by kubelet node name: %w", err)
	}
	return nil
}

func extractMachineBootstrapIdentity(obj client.Object) []string {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok {
		return nil
	}
	identity := keys.MachineBootstrapIdentity.Value(machine.Annotations)
	if identity == "" {
		return nil
	}
	return []string{identity}
}

func extractMachineKubeletNodeName(obj client.Object) []string {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok {
		return nil
	}
	name := kubeletClientNodeName(machine)
	if name == "" {
		return nil
	}
	return []string{name}
}

// KubeletClientCSRCacheIndexes are added to each tenant cluster cache as it connects.
// The username index lets a reconcile list one identity's CSRs without listing the tenant.
func KubeletClientCSRCacheIndexes() []clustercache.CacheOptionsIndex {
	return []clustercache.CacheOptionsIndex{
		{
			Object:       &certificatesv1.CertificateSigningRequest{},
			Field:        kubeletClientCSRUsernameField,
			ExtractValue: extractKubeletClientCSRUsername,
		},
	}
}

func extractKubeletClientCSRUsername(obj client.Object) []string {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
	if !ok || !isBootstrapKubeletClientCSR(csr) {
		return nil
	}
	return []string{csr.Spec.Username}
}

// kubeletClientNodeName is the node name a client certificate for this Machine would authenticate as.
// A joined Machine already occupies its NodeRef name.
func kubeletClientNodeName(machine *clusterv1.Machine) string {
	if machine.Status.NodeRef.IsDefined() {
		return machine.Status.NodeRef.Name
	}
	return expectedKubeletNodeName(machine)
}

func expectedKubeletNodeName(machine *clusterv1.Machine) string {
	if name := keys.MachineExpectedNodeName.Value(machine.Annotations); name != "" {
		return name
	}
	return machine.Name
}

func (c *KubeletClientCSRController) clusterToMachineRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	list := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, list,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel:            cluster.Name,
			keys.ApproveKubeletClientCSR.String(): "true",
		},
	); err != nil {
		c.logger.Error("listing Machines for kubelet client CSR", "cluster", client.ObjectKeyFromObject(cluster), "error", err)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

func machineRequests(machines []clusterv1.Machine) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(machines))
	for i := range machines {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&machines[i])})
	}
	return requests
}

//nolint:gocyclo,cyclop // The reconcile order is the behavior. Splitting it hides which outcome wins.
func (c *KubeletClientCSRController) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, request.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting Machine %s: %w", request.NamespacedName, err)
	}
	if !machine.DeletionTimestamp.IsZero() || !machineOptsIntoKubeletClientCSR(machine) {
		return ctrl.Result{}, nil
	}

	joined := machine.Status.NodeRef.IsDefined()
	if joined {
		return reconcile.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionTrue,
			KubeletClientCertificateIssuedReason,
			"Node has joined",
		)
	}

	identity := keys.MachineBootstrapIdentity.Value(machine.Annotations)
	if identity == "" {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRWaitingForBootstrapIdentityReason,
			"Waiting for the Machine bootstrap identity",
		)
	}

	cluster, err := capiutil.GetClusterFromMetadata(ctx, c.k8sClient, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}

	machinesByIdentity, err := c.machinesByBootstrapIdentity(ctx, machine.Namespace, cluster.Name, identity)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(machinesByIdentity) > 1 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRInvalidReason,
			"Bootstrap identity is assigned to multiple Machines",
		)
	}

	machinesByNodeName, err := c.machinesByKubeletNodeName(ctx, machine.Namespace, cluster.Name, expectedKubeletNodeName(machine))
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(machinesByNodeName) > 1 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRInvalidReason,
			"Expected node name is assigned to multiple Machines",
		)
	}

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
	if err := c.watchKubeletClientCSRs(ctx, clusterKey); err != nil {
		return ctrl.Result{}, ignoreNotConnected(err)
	}

	csrs, err := clientCSRsForMachine(ctx, tenantClient, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(csrs) == 0 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRWaitingReason,
			"Waiting for the kubelet client CSR",
		)
	}

	csrDecisions := decideKubeletClientCSRs(machine, csrs)

	// One issued certificate completes bootstrap. Kubelet stores a single client key and waits on the CSR
	// named from that key, so leftover pending requests are not approved.
	if len(csrDecisions.issued) > 0 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionTrue,
			KubeletClientCertificateIssuedReason,
			"Initial kubelet client certificate issued",
		)
	}

	if len(csrDecisions.pending) > 0 {
		// Do not approve a client certificate for a node name that already exists.
		nodeName := expectedKubeletNodeName(machine)
		exists, err := kubeletNodeExists(ctx, tenantClient, nodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if exists {
			return ctrl.Result{}, setKubeletClientCertificateCondition(
				ctx, c.k8sClient, machine,
				metav1.ConditionFalse,
				KubeletClientCSRInvalidReason,
				fmt.Sprintf("Node %s already exists", nodeName),
			)
		}
		if err := approveKubeletClientCSRs(ctx, tenantClient, machine, csrDecisions.pending, c.logger); err != nil {
			condErr := setKubeletClientCertificateCondition(
				ctx, c.k8sClient, machine,
				metav1.ConditionFalse,
				KubeletClientCSRFailedReason,
				err.Error(),
			)
			return ctrl.Result{}, errors.Join(err, condErr)
		}
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRWaitingForSigningReason,
			fmt.Sprintf("Approved %d kubelet client CSRs", len(csrDecisions.pending)),
		)
	}

	if len(csrDecisions.approved) > 0 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRWaitingForSigningReason,
			fmt.Sprintf("%d kubelet client CSRs are approved but not yet signed", len(csrDecisions.approved)),
		)
	}

	if len(csrDecisions.invalid) > 0 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRInvalidReason,
			fmt.Sprintf("%d kubelet client CSRs are invalid, latest reason: %s", len(csrDecisions.invalid), csrDecisions.invalidReason),
		)
	}

	if len(csrDecisions.denied) > 0 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRInvalidReason,
			fmt.Sprintf("%d kubelet client CSRs were denied", len(csrDecisions.denied)),
		)
	}

	if len(csrDecisions.failed) > 0 {
		return ctrl.Result{}, setKubeletClientCertificateCondition(
			ctx, c.k8sClient, machine,
			metav1.ConditionFalse,
			KubeletClientCSRInvalidReason,
			fmt.Sprintf("%d kubelet client CSRs failed", len(csrDecisions.failed)),
		)
	}

	return ctrl.Result{}, fmt.Errorf("kubelet client CSRs for Machine %s matched no decision", request.NamespacedName)
}

func (c *KubeletClientCSRController) watchKubeletClientCSRs(ctx context.Context, clusterKey client.ObjectKey) error {
	if err := c.clusterCache.Watch(ctx, clusterKey, clustercache.NewWatcher(clustercache.WatcherOptions{
		Name:         kubeletClientCSRWatchName,
		Watcher:      c.controller,
		Kind:         &certificatesv1.CertificateSigningRequest{},
		EventHandler: handler.EnqueueRequestsFromMapFunc(c.requestsForClientCSR(clusterKey)),
		Predicates:   []predicate.TypedPredicate[client.Object]{kubeletClientCSRPredicate()},
	})); err != nil {
		return fmt.Errorf("watching kubelet client CSRs for Cluster %s: %w", clusterKey, err)
	}
	return nil
}

func kubeletNodeExists(ctx context.Context, tenantClient client.Client, nodeName string) (bool, error) {
	node := &corev1.Node{}
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting Node %s: %w", nodeName, err)
	}
	return true, nil
}

func ignoreNotConnected(err error) error {
	if errors.Is(err, clustercache.ErrClusterNotConnected) {
		return nil
	}
	return err
}

func (c *KubeletClientCSRController) requestsForClientCSR(clusterKey client.ObjectKey) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
		if !ok || !isBootstrapKubeletClientCSR(csr) {
			return nil
		}
		machines, err := c.machinesByBootstrapIdentity(ctx, clusterKey.Namespace, clusterKey.Name, csr.Spec.Username)
		if err != nil {
			c.logger.Error("listing Machines for kubelet client CSR", "cluster", clusterKey, "error", err)
			return nil
		}
		return machineRequests(machines)
	}
}

func (c *KubeletClientCSRController) machinesByBootstrapIdentity(ctx context.Context, namespace, clusterName, identity string) ([]clusterv1.Machine, error) {
	list := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
		client.MatchingFields{machineBootstrapIdentityField: identity},
	); err != nil {
		return nil, fmt.Errorf("listing Machines by bootstrap identity for Cluster %s/%s: %w", namespace, clusterName, err)
	}
	return list.Items, nil
}

func (c *KubeletClientCSRController) machinesByKubeletNodeName(ctx context.Context, namespace, clusterName, nodeName string) ([]clusterv1.Machine, error) {
	list := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
		client.MatchingFields{machineKubeletNodeNameField: nodeName},
	); err != nil {
		return nil, fmt.Errorf("listing Machines by kubelet node name for Cluster %s/%s: %w", namespace, clusterName, err)
	}
	return list.Items, nil
}

func clientCSRsForMachine(ctx context.Context, tenantClient client.Client, machine *clusterv1.Machine) ([]certificatesv1.CertificateSigningRequest, error) {
	identity := keys.MachineBootstrapIdentity.Value(machine.Annotations)
	list := &certificatesv1.CertificateSigningRequestList{}
	if err := tenantClient.List(ctx, list, client.MatchingFields{kubeletClientCSRUsernameField: identity}); err != nil {
		return nil, fmt.Errorf("listing kubelet client CSRs: %w", err)
	}
	return list.Items, nil
}
