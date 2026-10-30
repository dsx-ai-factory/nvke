// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
)

const (
	bootstrapAuthorizationRetryAfter = 30 * time.Second
	bootstrapAuthorizationBatchDelay = 5 * time.Second

	// bootstrapAuthorizationMaxSubjects limits concurrent joining subjects on
	// tenant bindings, well below Kubernetes object size limits (measured
	// around ~12,800 147-byte subjects in a kind probe). Clusters scaling
	// beyond this switch to group- or role-based bootstrap authorization.
	bootstrapAuthorizationMaxSubjects = 1000

	// Bound reconciliation error size when Machine condition updates fail in bulk.
	maxReportedBootstrapAuthorizationConditionErrors = 10

	bootstrapNodeBootstrapperBindingName = "nvke-bootstrap-node-bootstrapper"
	bootstrapKubeadmGetNodesBindingName  = "nvke-bootstrap-kubeadm-get-nodes"

	bootstrapKubeadmConfigBindingName = "nvke-bootstrap-kubeadm-nodes-kubeadm-config"
	bootstrapKubeletConfigBindingName = "nvke-bootstrap-kubeadm-kubelet-config"
	bootstrapKubeProxyBindingName     = "nvke-bootstrap-kube-proxy"
	bootstrapClusterInfoBindingName   = "nvke-bootstrap-kubeadm-bootstrap-signer-clusterinfo"
)

type bootstrapRoleBinding struct {
	name      string
	namespace string
	roleName  string
}

func desiredBootstrapRoleBindings() []bootstrapRoleBinding {
	return []bootstrapRoleBinding{
		{name: bootstrapKubeadmConfigBindingName, namespace: "kube-system", roleName: "kubeadm:nodes-kubeadm-config"},
		{name: bootstrapKubeletConfigBindingName, namespace: "kube-system", roleName: "kubeadm:kubelet-config"},
		{name: bootstrapKubeProxyBindingName, namespace: "kube-system", roleName: "kube-proxy"},
		{name: bootstrapClusterInfoBindingName, namespace: "kube-public", roleName: "kubeadm:bootstrap-signer-clusterinfo"},
	}
}

func desiredBootstrapClusterRoleBindings() []bootstrapRoleBinding {
	return []bootstrapRoleBinding{
		{name: bootstrapNodeBootstrapperBindingName, roleName: "system:node-bootstrapper"},
		{name: bootstrapKubeadmGetNodesBindingName, roleName: "kubeadm:get-nodes"},
	}
}

// AuthorizationController grants each joining Machine's resolved identity the tenant RBAC needed by kubeadm.
type AuthorizationController struct {
	k8sClient            client.Client
	tenantClientProvider kube.TenantClientProvider
	logger               *slog.Logger
}

// NewAuthorizationController returns a controller that synchronizes per-Machine bootstrap authorization.
func NewAuthorizationController(
	k8sClient client.Client,
	tenantClientProvider kube.TenantClientProvider,
	logger *slog.Logger,
) *AuthorizationController {
	return &AuthorizationController{
		k8sClient:            k8sClient,
		tenantClientProvider: tenantClientProvider,
		logger:               logger,
	}
}

// SetupWithManager wires the controller to reconcile a Cluster whenever it or one of its Machines changes.
func (c *AuthorizationController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}).
		Watches(
			&clusterv1.Machine{},
			delayedClusterRequestsForMachine(bootstrapAuthorizationBatchDelay),
			builder.WithPredicates(bootstrapAuthorizationMachinePredicate()),
		).
		Named(constants.FieldManagerBootstrapAuthorization).
		Complete(c)
}

// delayedClusterRequestsForMachine turns a Machine event into one delayed reconcile of that Machine's Cluster.
// The reconcile lists the Cluster's selected Machines and writes their combined subjects to the tenant RBAC bindings.
// AddAfter waits briefly, and the work queue keeps one request per Cluster, so a burst of Machine changes results in one Cluster reconcile.
func delayedClusterRequestsForMachine(delay time.Duration) handler.EventHandler {
	enqueue := func(obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		if req, ok := clusterRequestForMachine(obj); ok {
			q.AddAfter(req, delay)
		}
	}

	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(e.Object, q)
		},
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(e.ObjectOld, q)
			enqueue(e.ObjectNew, q)
		},
		DeleteFunc: func(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(e.Object, q)
		},
	}
}

// bootstrapAuthorizationMachinePredicate keeps a Machine event only when it can change the tenant bootstrap subjects.
// This controller writes Machine conditions, and CAPI providers update Machine status often.
// Updates that leave the subject list unchanged are dropped, so they do not schedule another Cluster reconcile.
func bootstrapAuthorizationMachinePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return machineUsesBootstrapAuthorization(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, oldOK := e.ObjectOld.(*clusterv1.Machine)
			newMachine, newOK := e.ObjectNew.(*clusterv1.Machine)
			if !oldOK || !newOK {
				return false
			}
			oldSelected := machineUsesBootstrapAuthorization(oldMachine)
			newSelected := machineUsesBootstrapAuthorization(newMachine)
			if !oldSelected && !newSelected {
				return false
			}
			return oldSelected != newSelected ||
				oldMachine.Spec.ClusterName != newMachine.Spec.ClusterName ||
				oldMachine.Labels[clusterv1.ClusterNameLabel] != newMachine.Labels[clusterv1.ClusterNameLabel] ||
				keys.MachineBootstrapIdentity.Value(oldMachine.Annotations) != keys.MachineBootstrapIdentity.Value(newMachine.Annotations) ||
				oldMachine.Status.NodeRef != newMachine.Status.NodeRef ||
				oldMachine.DeletionTimestamp.IsZero() != newMachine.DeletionTimestamp.IsZero()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return machineUsesBootstrapAuthorization(e.Object)
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

func machineUsesBootstrapAuthorization(obj client.Object) bool {
	machine, ok := obj.(*clusterv1.Machine)
	return ok && keys.SyncBootstrapAuthorization.Value(machine.Labels) == "true"
}

func clusterRequestForMachine(obj client.Object) (reconcile.Request, bool) {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok || machine.Spec.ClusterName == "" {
		return reconcile.Request{}, false
	}
	return reconcile.Request{NamespacedName: client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.ClusterName,
	}}, true
}

func (c *AuthorizationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := c.logger.With("clusterNamespace", req.Namespace, "clusterName", req.Name)

	cluster := &clusterv1.Cluster{}
	if err := c.k8sClient.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get CAPI cluster: %w", err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	machines := &clusterv1.MachineList{}
	if err := c.k8sClient.List(
		ctx,
		machines,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel:               cluster.Name,
			keys.SyncBootstrapAuthorization.String(): "true",
		},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list Machines for cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}

	selected := selectedBootstrapAuthorizationMachines(machines.Items, cluster.Name)

	tenantClient, err := c.tenantClientProvider.ClientForCluster(ctx, cluster)
	if err != nil {
		if len(selected) == 0 {
			// Nothing is opted in, so there is no authorization to grant and no binding cleanup to attempt.
			// This includes Clusters the provider cannot reach, such as ones outside the management namespace.
			return ctrl.Result{}, nil
		}
		if apierrors.IsNotFound(err) || errors.Is(err, kube.ErrTenantClientNotReady) {
			logger.Info("Waiting for tenant access before synchronizing bootstrap authorization")
			return ctrl.Result{RequeueAfter: bootstrapAuthorizationRetryAfter}, c.setConditions(
				ctx, selected, metav1.ConditionFalse, BootstrapAuthorizationWaitingForTenantAccessReason, "Waiting for access to the tenant Kubernetes API",
			)
		}
		conditionErr := c.setConditions(ctx, selected, metav1.ConditionFalse, BootstrapAuthorizationSyncFailedReason, err.Error())
		return ctrl.Result{}, errors.Join(fmt.Errorf("failed to create tenant cluster client: %w", err), conditionErr)
	}

	// With no selected Machines, synchronize (remove any stale subjects) only if managed bindings still exist.
	if len(selected) == 0 {
		bindingsExist, err := bootstrapAuthorizationBindingsExist(ctx, tenantClient)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !bindingsExist {
			return ctrl.Result{}, nil
		}
	}

	plan := planBootstrapAuthorization(selected, bootstrapAuthorizationMaxSubjects)
	logger = logger.With(
		"joinedMachineCount", len(plan.joined),
		"toAuthorizeMachineCount", len(plan.toAuthorize),
		"waitingForIdentityMachineCount", len(plan.waitingForIdentity),
		"duplicateIdentityMachineCount", len(plan.duplicateIdentity),
		"waitingForAuthorizationMachineCount", len(plan.waitingForAuthorization),
	)
	logger.Debug("Planned tenant bootstrap authorization")

	changed, err := syncBootstrapAuthorizationBindings(ctx, tenantClient, plan.subjectNames())
	if err != nil {
		conditionErr := c.setConditions(ctx, selected, metav1.ConditionFalse, BootstrapAuthorizationSyncFailedReason, err.Error())
		return ctrl.Result{}, errors.Join(err, conditionErr)
	}

	if err := c.setSyncedConditions(ctx, plan); err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		logger.Info("Synchronized tenant bootstrap authorization")
	}
	return ctrl.Result{}, nil
}

func selectedBootstrapAuthorizationMachines(machines []clusterv1.Machine, clusterName string) []*clusterv1.Machine {
	selected := make([]*clusterv1.Machine, 0, len(machines))
	for i := range machines {
		machine := &machines[i]
		if machine.Spec.ClusterName != clusterName || !machine.DeletionTimestamp.IsZero() {
			continue
		}
		selected = append(selected, machine)
	}
	return selected
}

type bootstrapAuthorizationPlan struct {
	joined      []*clusterv1.Machine
	toAuthorize []*clusterv1.Machine

	waitingForIdentity      []*clusterv1.Machine
	duplicateIdentity       []*clusterv1.Machine
	waitingForAuthorization []*clusterv1.Machine
}

func (p bootstrapAuthorizationPlan) subjectNames() []string {
	names := make([]string, 0, len(p.toAuthorize))
	for _, machine := range p.toAuthorize {
		names = append(names, keys.MachineBootstrapIdentity.Value(machine.Annotations))
	}
	sort.Strings(names)
	return names
}

// planBootstrapAuthorization partitions selected Machines and limits the tenant
// RBAC subject list to the controller's subject budget. Without a bound, an
// oversized aggregate update can be rejected as a whole, preventing removal of
// subjects for joined Machines as well as admission of waiting Machines.
// Keeping the target bounded lets each reconcile remove completed joins, retain
// in-progress joins, and fill released capacity with waiting Machines.
func planBootstrapAuthorization(machines []*clusterv1.Machine, maxSubjects int) bootstrapAuthorizationPlan {
	plan := bootstrapAuthorizationPlan{}

	identityCounts := make(map[string]int, len(machines))
	for _, machine := range machines {
		if identity := keys.MachineBootstrapIdentity.Value(machine.Annotations); identity != "" {
			identityCounts[identity]++
		}
	}

	candidates := make([]*clusterv1.Machine, 0, len(machines))
	for _, machine := range machines {
		identity := keys.MachineBootstrapIdentity.Value(machine.Annotations)
		switch {
		case machine.Status.NodeRef.IsDefined():
			plan.joined = append(plan.joined, machine)
		case identity != "" && identityCounts[identity] > 1:
			// NVKE's intended design is that an identity describes a unique Machine.
			plan.duplicateIdentity = append(plan.duplicateIdentity, machine)
		case identity == "":
			plan.waitingForIdentity = append(plan.waitingForIdentity, machine)
		default:
			candidates = append(candidates, machine)
		}
	}

	// Keep previously authorized Machines ahead of older Machines whose identities resolve later so an in-progress join is not displaced.
	// Order the remainder oldest-first, using the object key to make the unspecified Machine list order deterministic.
	sort.Slice(candidates, func(i, j int) bool {
		iPreviouslyAuthorized := bootstrapAuthorizationIsSynced(candidates[i])
		jPreviouslyAuthorized := bootstrapAuthorizationIsSynced(candidates[j])
		if iPreviouslyAuthorized != jPreviouslyAuthorized {
			return iPreviouslyAuthorized
		}
		if !candidates[i].CreationTimestamp.Equal(&candidates[j].CreationTimestamp) {
			return candidates[i].CreationTimestamp.Before(&candidates[j].CreationTimestamp)
		}
		return client.ObjectKeyFromObject(candidates[i]).String() < client.ObjectKeyFromObject(candidates[j]).String()
	})

	// Partition the candidates into toAuthorize and waitingForAuthorization.
	authorizeCount := min(len(candidates), maxSubjects)
	plan.toAuthorize = slices.Clone(candidates[:authorizeCount])
	plan.waitingForAuthorization = slices.Clone(candidates[authorizeCount:])

	return plan
}

func bootstrapAuthorizationIsSynced(machine *clusterv1.Machine) bool {
	condition := meta.FindStatusCondition(machine.Status.Conditions, BootstrapAuthorizationSyncedCondition)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == BootstrapAuthorizationSyncedReason
}

func bootstrapAuthorizationBindingsExist(ctx context.Context, tenantClient client.Client) (bool, error) {
	for _, desired := range desiredBootstrapClusterRoleBindings() {
		err := tenantClient.Get(ctx, client.ObjectKey{Name: desired.name}, &rbacv1.ClusterRoleBinding{})
		if err == nil {
			return true, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to check tenant ClusterRoleBinding %s: %w", desired.name, err)
		}
	}
	for _, desired := range desiredBootstrapRoleBindings() {
		err := tenantClient.Get(ctx, client.ObjectKey{Namespace: desired.namespace, Name: desired.name}, &rbacv1.RoleBinding{})
		if err == nil {
			return true, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to check tenant RoleBinding %s/%s: %w", desired.namespace, desired.name, err)
		}
	}
	return false, nil
}

func syncBootstrapAuthorizationBindings(ctx context.Context, tenantClient client.Client, subjectNames []string) (bool, error) {
	subjects := make([]rbacv1.Subject, 0, len(subjectNames))
	for _, name := range subjectNames {
		subjects = append(subjects, rbacv1.Subject{
			APIGroup: rbacv1.GroupName,
			Kind:     rbacv1.UserKind,
			Name:     name,
		})
	}

	changed := false
	for _, binding := range desiredBootstrapClusterRoleBindings() {
		bindingChanged, err := applyBootstrapClusterRoleBinding(ctx, tenantClient, binding, subjects)
		if err != nil {
			return false, err
		}
		changed = changed || bindingChanged
	}

	for _, binding := range desiredBootstrapRoleBindings() {
		bindingChanged, err := applyBootstrapRoleBinding(ctx, tenantClient, binding, subjects)
		if err != nil {
			return false, err
		}
		changed = changed || bindingChanged
	}

	return changed, nil
}

func applyBootstrapClusterRoleBinding(ctx context.Context, tenantClient client.Client, binding bootstrapRoleBinding, subjects []rbacv1.Subject) (bool, error) {
	roleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: binding.roleName}
	current := &rbacv1.ClusterRoleBinding{}
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: binding.name},
		RoleRef:    roleRef,
		Subjects:   subjects,
	}

	created, err := getOrCreateBootstrapBinding(ctx, tenantClient, current, desired)
	if err != nil {
		return false, fmt.Errorf("failed to get or create tenant ClusterRoleBinding %s: %w", binding.name, err)
	}

	if created {
		return true, nil
	}

	if current.RoleRef != roleRef {
		return false, fmt.Errorf("tenant ClusterRoleBinding %s has roleRef %+v, expected %+v", binding.name, current.RoleRef, roleRef)
	}

	if slices.Equal(current.Subjects, subjects) {
		return false, nil
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.Subjects = subjects
	if err := tenantClient.Patch(ctx, current, patch); err != nil {
		return false, fmt.Errorf("failed to patch tenant ClusterRoleBinding %s: %w", binding.name, err)
	}

	return true, nil
}

func applyBootstrapRoleBinding(ctx context.Context, tenantClient client.Client, binding bootstrapRoleBinding, subjects []rbacv1.Subject) (bool, error) {
	roleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: binding.roleName}
	current := &rbacv1.RoleBinding{}
	desired := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: binding.name, Namespace: binding.namespace},
		RoleRef:    roleRef,
		Subjects:   subjects,
	}

	created, err := getOrCreateBootstrapBinding(ctx, tenantClient, current, desired)
	if err != nil {
		return false, fmt.Errorf("failed to get or create tenant RoleBinding %s/%s: %w", binding.namespace, binding.name, err)
	}

	if created {
		return true, nil
	}

	if current.RoleRef != roleRef {
		return false, fmt.Errorf(
			"tenant RoleBinding %s/%s has roleRef %+v, expected %+v", binding.namespace, binding.name, current.RoleRef, roleRef,
		)
	}

	if slices.Equal(current.Subjects, subjects) {
		return false, nil
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.Subjects = subjects
	if err := tenantClient.Patch(ctx, current, patch); err != nil {
		return false, fmt.Errorf("failed to patch tenant RoleBinding %s/%s: %w", binding.namespace, binding.name, err)
	}

	return true, nil
}

func getOrCreateBootstrapBinding(ctx context.Context, tenantClient client.Client, current, desired client.Object) (bool, error) {
	key := client.ObjectKeyFromObject(desired)

	if err := tenantClient.Get(ctx, key, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("getting current binding: %w", err)
		}
		if err := tenantClient.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("creating desired binding: %w", err)
		}
		return true, nil
	}

	return false, nil
}

// bootstrapAuthorizationConditionErrors keeps up to maxReportedBootstrapAuthorizationConditionErrors
// condition update errors and counts the rest, so one reconcile does not log or return an unbounded join.
type bootstrapAuthorizationConditionErrors struct {
	errs    []error
	omitted int
}

func (e *bootstrapAuthorizationConditionErrors) add(err error) {
	if err == nil {
		return
	}
	if len(e.errs) < maxReportedBootstrapAuthorizationConditionErrors {
		e.errs = append(e.errs, err)
		return
	}
	e.omitted++
}

func (e *bootstrapAuthorizationConditionErrors) err() error {
	if e.omitted == 0 {
		return errors.Join(e.errs...)
	}
	errs := slices.Clone(e.errs)
	errs = append(errs, fmt.Errorf("%d additional Machine condition update errors omitted", e.omitted))
	return errors.Join(errs...)
}

func (c *AuthorizationController) setSyncedConditions(ctx context.Context, plan bootstrapAuthorizationPlan) error {
	var conditionErrors bootstrapAuthorizationConditionErrors

	// Plan groups are disjoint, so each selected Machine is checked once.
	// Unchanged conditions do not issue status patches.
	updateErr := errors.Join(
		c.setConditionsCollectingErrors(
			ctx,
			plan.joined,
			metav1.ConditionTrue,
			BootstrapAuthorizationSyncedReason,
			"Removed bootstrap authorization after the Machine joined",
			&conditionErrors,
		),
		c.setConditionsCollectingErrors(
			ctx,
			plan.toAuthorize,
			metav1.ConditionTrue,
			BootstrapAuthorizationSyncedReason,
			"Granted bootstrap authorization to the Machine identity",
			&conditionErrors,
		),

		c.setConditionsCollectingErrors(
			ctx,
			plan.waitingForIdentity,
			metav1.ConditionFalse,
			BootstrapAuthorizationWaitingForIdentityReason,
			"Waiting for the Machine bootstrap identity",
			&conditionErrors,
		),
		c.setConditionsCollectingErrors(
			ctx,
			plan.duplicateIdentity,
			metav1.ConditionFalse,
			BootstrapAuthorizationDuplicateIdentityReason,
			"Bootstrap identity is assigned to multiple selected Machines",
			&conditionErrors,
		),
		c.setConditionsCollectingErrors(
			ctx,
			plan.waitingForAuthorization,
			metav1.ConditionFalse,
			BootstrapAuthorizationSubjectLimitExceededReason,
			fmt.Sprintf("Waiting for bootstrap authorization; the cluster limit is %d concurrent subjects", bootstrapAuthorizationMaxSubjects),
			&conditionErrors,
		),
	)
	return errors.Join(conditionErrors.err(), updateErr)
}

func (c *AuthorizationController) setConditions(
	ctx context.Context,
	machines []*clusterv1.Machine,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	var conditionErrors bootstrapAuthorizationConditionErrors
	updateErr := c.setConditionsCollectingErrors(ctx, machines, status, reason, message, &conditionErrors)
	return errors.Join(conditionErrors.err(), updateErr)
}

func (c *AuthorizationController) setConditionsCollectingErrors(
	ctx context.Context,
	machines []*clusterv1.Machine,
	status metav1.ConditionStatus,
	reason string,
	message string,
	conditionErrors *bootstrapAuthorizationConditionErrors,
) error {
	for _, machine := range machines {
		if err := ctx.Err(); err != nil {
			return err
		}
		conditionErrors.add(setBootstrapAuthorizationSyncedCondition(ctx, c.k8sClient, machine, status, reason, message))
	}
	return nil
}
