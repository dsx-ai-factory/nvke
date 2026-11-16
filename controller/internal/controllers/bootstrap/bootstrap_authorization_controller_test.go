// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const testBootstrapAuthorizationClusterName = "tenant-a"

func TestUnit_BootstrapAuthorizationControllerReconcileSynchronizesBindings(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	activeB := newMachineForBootstrapAuthorizationTest("worker-b", "spiffe://example.org/bootstrap/b")
	activeA := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	waiting := newMachineForBootstrapAuthorizationTest("worker-waiting", "")
	joined := newMachineForBootstrapAuthorizationTest("worker-joined", "spiffe://example.org/bootstrap/joined")
	joined.Status.NodeRef = clusterv1.MachineNodeReference{Name: "worker-joined"}
	unselected := newMachineForBootstrapAuthorizationTest("worker-unselected", "spiffe://example.org/bootstrap/unselected")
	keys.SyncBootstrapAuthorization.Delete(unselected.Labels)

	controller, tenantClient := newBootstrapAuthorizationControllerForTest(
		t,
		[]client.Object{cluster, activeB, activeA, waiting, joined, unselected},
	)

	result, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	wantSubjects := []rbacv1.Subject{
		{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "spiffe://example.org/bootstrap/a"},
		{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "spiffe://example.org/bootstrap/b"},
	}
	assertBootstrapClusterRoleBinding(t, tenantClient, bootstrapNodeBootstrapperBindingName, "system:node-bootstrapper", wantSubjects)
	assertBootstrapClusterRoleBinding(t, tenantClient, bootstrapKubeadmGetNodesBindingName, "kubeadm:get-nodes", wantSubjects)
	for _, binding := range desiredBootstrapRoleBindings() {
		assertBootstrapRoleBinding(t, tenantClient, binding, wantSubjects)
	}

	assertBootstrapAuthorizationCondition(t, controller.k8sClient, activeA, metav1.ConditionTrue, BootstrapAuthorizationSyncedReason)
	assertBootstrapAuthorizationCondition(t, controller.k8sClient, activeB, metav1.ConditionTrue, BootstrapAuthorizationSyncedReason)
	assertBootstrapAuthorizationCondition(t, controller.k8sClient, waiting, metav1.ConditionFalse, BootstrapAuthorizationWaitingForIdentityReason)
	assertBootstrapAuthorizationCondition(t, controller.k8sClient, joined, metav1.ConditionTrue, BootstrapAuthorizationSyncedReason)

	gotUnselected := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(unselected), gotUnselected))
	require.Nil(t, meta.FindStatusCondition(gotUnselected.Status.Conditions, BootstrapAuthorizationSyncedCondition))
}

func TestUnit_BootstrapAuthorizationControllerReconcileRemovesJoinedMachineSubject(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	machine := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	machine.Status.NodeRef = clusterv1.MachineNodeReference{Name: machine.Name}
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapNodeBootstrapperBindingName},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "spiffe://example.org/bootstrap/a",
		}},
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", APIGroup: rbacv1.GroupName, Name: "system:node-bootstrapper"},
	}
	controller, tenantClient := newBootstrapAuthorizationControllerForTest(
		t,
		[]client.Object{cluster, machine},
		existing,
	)

	result, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	got := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: bootstrapNodeBootstrapperBindingName}, got))
	require.Empty(t, got.Subjects)
	assertBootstrapAuthorizationCondition(t, controller.k8sClient, machine, metav1.ConditionTrue, BootstrapAuthorizationSyncedReason)
}

func TestUnit_BootstrapAuthorizationControllerReconcileSkipsClusterWithoutSelectedMachinesOrBindings(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	machine := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	keys.SyncBootstrapAuthorization.Delete(machine.Labels)
	controller, tenantClient := newBootstrapAuthorizationControllerForTest(t, []client.Object{cluster, machine})

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)

	err = tenantClient.Get(t.Context(), client.ObjectKey{Name: bootstrapNodeBootstrapperBindingName}, &rbacv1.ClusterRoleBinding{})
	require.True(t, apierrors.IsNotFound(err), "error: %v", err)
}

func TestUnit_BootstrapAuthorizationControllerReconcileCleansUpAfterLastMachineOptsOut(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	machine := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	keys.SyncBootstrapAuthorization.Delete(machine.Labels)
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapNodeBootstrapperBindingName},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "spiffe://example.org/bootstrap/a",
		}},
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", APIGroup: rbacv1.GroupName, Name: "system:node-bootstrapper"},
	}
	controller, tenantClient := newBootstrapAuthorizationControllerForTest(
		t,
		[]client.Object{cluster, machine},
		existing,
	)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)

	got := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: bootstrapNodeBootstrapperBindingName}, got))
	require.Empty(t, got.Subjects)
}

func TestUnit_BootstrapAuthorizationControllerReconcileWaitsForTenantAccess(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	machine := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	controller, _ := newBootstrapAuthorizationControllerForTest(t, []client.Object{cluster, machine})
	controller.clusterCache = staticBootstrapAuthorizationClusterCache{err: clustercache.ErrClusterNotConnected}

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)
	assertBootstrapAuthorizationCondition(
		t,
		controller.k8sClient,
		machine,
		metav1.ConditionFalse,
		BootstrapAuthorizationWaitingForTenantAccessReason,
	)
}

func TestUnit_BootstrapAuthorizationControllerReconcileSkipsTenantClientErrorWithoutSelectedMachines(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	controller, tenantClient := newBootstrapAuthorizationControllerForTest(t, []client.Object{cluster})
	controller.clusterCache = staticBootstrapAuthorizationClusterCache{err: errors.New("no tenant client")}

	result, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	err = tenantClient.Get(t.Context(), client.ObjectKey{Name: bootstrapNodeBootstrapperBindingName}, &rbacv1.ClusterRoleBinding{})
	require.True(t, apierrors.IsNotFound(err), "error: %v", err)
}

func TestUnit_BootstrapAuthorizationControllerReconcileLeavesBindingsWhenClusterIsDeleting(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	markClusterDeleting(cluster)
	machine := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapNodeBootstrapperBindingName},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "spiffe://example.org/bootstrap/a",
		}},
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", APIGroup: rbacv1.GroupName, Name: "system:node-bootstrapper"},
	}
	controller, tenantClient := newBootstrapAuthorizationControllerForTest(t, []client.Object{cluster, machine}, existing)

	result, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	got := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: bootstrapNodeBootstrapperBindingName}, got))
	require.Len(t, got.Subjects, 1)
}

func TestUnit_BootstrapAuthorizationControllerSetsWaitingConditionAtSubjectLimit(t *testing.T) {
	t.Parallel()

	activeB := newMachineForBootstrapAuthorizationTest("worker-b", "spiffe://example.org/bootstrap/b")
	activeA := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	controller, _ := newBootstrapAuthorizationControllerForTest(t, []client.Object{activeB, activeA})
	plan := planBootstrapAuthorization([]*clusterv1.Machine{activeB, activeA}, 1)

	require.NoError(t, controller.setSyncedConditions(t.Context(), plan))

	assertBootstrapAuthorizationCondition(t, controller.k8sClient, activeA, metav1.ConditionTrue, BootstrapAuthorizationSyncedReason)
	assertBootstrapAuthorizationCondition(
		t,
		controller.k8sClient,
		activeB,
		metav1.ConditionFalse,
		BootstrapAuthorizationSubjectLimitExceededReason,
	)
}

func TestUnit_BootstrapAuthorizationControllerRejectsDuplicateIdentity(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	activeA := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/shared")
	activeB := newMachineForBootstrapAuthorizationTest("worker-b", "spiffe://example.org/bootstrap/shared")
	controller, tenantClient := newBootstrapAuthorizationControllerForTest(t, []client.Object{cluster, activeA, activeB})

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.NoError(t, err)

	binding := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: bootstrapNodeBootstrapperBindingName}, binding))
	require.Empty(t, binding.Subjects)
	assertBootstrapAuthorizationCondition(
		t,
		controller.k8sClient,
		activeA,
		metav1.ConditionFalse,
		BootstrapAuthorizationDuplicateIdentityReason,
	)
	assertBootstrapAuthorizationCondition(
		t,
		controller.k8sClient,
		activeB,
		metav1.ConditionFalse,
		BootstrapAuthorizationDuplicateIdentityReason,
	)
}

func TestUnit_PlanBootstrapAuthorizationRetainsPreviouslyAdmittedMachine(t *testing.T) {
	t.Parallel()

	activeA := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	activeB := newMachineForBootstrapAuthorizationTest("worker-b", "spiffe://example.org/bootstrap/b")
	activeB.Status.Conditions = []metav1.Condition{{
		Type:   BootstrapAuthorizationSyncedCondition,
		Status: metav1.ConditionTrue,
		Reason: BootstrapAuthorizationSyncedReason,
	}}

	plan := planBootstrapAuthorization([]*clusterv1.Machine{activeA, activeB}, 1)

	require.Equal(t, []string{"spiffe://example.org/bootstrap/b"}, plan.subjectNames())
	require.Equal(t, []*clusterv1.Machine{activeB}, plan.toAuthorize)
	require.Equal(t, []*clusterv1.Machine{activeA}, plan.waitingForAuthorization)
}

func TestUnit_PlanBootstrapAuthorizationReleasesJoinedMachineCapacity(t *testing.T) {
	t.Parallel()

	authorized := newMachineForBootstrapAuthorizationTest("worker-authorized", "spiffe://example.org/bootstrap/authorized")
	authorized.Status.Conditions = []metav1.Condition{{
		Type:   BootstrapAuthorizationSyncedCondition,
		Status: metav1.ConditionTrue,
		Reason: BootstrapAuthorizationSyncedReason,
	}}
	waiting := newMachineForBootstrapAuthorizationTest("worker-waiting", "spiffe://example.org/bootstrap/waiting")

	fullPlan := planBootstrapAuthorization([]*clusterv1.Machine{waiting, authorized}, 1)
	require.Equal(t, []string{"spiffe://example.org/bootstrap/authorized"}, fullPlan.subjectNames())
	require.Equal(t, []*clusterv1.Machine{authorized}, fullPlan.toAuthorize)
	require.Equal(t, []*clusterv1.Machine{waiting}, fullPlan.waitingForAuthorization)

	authorized.Status.NodeRef = clusterv1.MachineNodeReference{Name: authorized.Name}
	nextPlan := planBootstrapAuthorization([]*clusterv1.Machine{waiting, authorized}, 1)
	require.Equal(t, []string{"spiffe://example.org/bootstrap/waiting"}, nextPlan.subjectNames())
	require.Equal(t, []*clusterv1.Machine{authorized}, nextPlan.joined)
	require.Equal(t, []*clusterv1.Machine{waiting}, nextPlan.toAuthorize)
	require.Empty(t, nextPlan.waitingForAuthorization)
}

func TestUnit_PlanBootstrapAuthorizationRejectsDuplicateIdentity(t *testing.T) {
	t.Parallel()

	activeA := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/shared")
	activeB := newMachineForBootstrapAuthorizationTest("worker-b", "spiffe://example.org/bootstrap/shared")

	plan := planBootstrapAuthorization([]*clusterv1.Machine{activeA, activeB}, 1)

	require.Empty(t, plan.subjectNames())
	require.Empty(t, plan.toAuthorize)
	require.Equal(t, []*clusterv1.Machine{activeA, activeB}, plan.duplicateIdentity)
}

func TestUnit_PlanBootstrapAuthorizationCompletesJoinedMachineWithDuplicateIdentity(t *testing.T) {
	t.Parallel()

	joined := newMachineForBootstrapAuthorizationTest("worker-joined", "spiffe://example.org/bootstrap/shared")
	joined.Status.NodeRef = clusterv1.MachineNodeReference{Name: joined.Name}
	active := newMachineForBootstrapAuthorizationTest("worker-active", "spiffe://example.org/bootstrap/shared")

	plan := planBootstrapAuthorization([]*clusterv1.Machine{joined, active}, 1)

	require.Equal(t, []*clusterv1.Machine{joined}, plan.joined)
	require.Equal(t, []*clusterv1.Machine{active}, plan.duplicateIdentity)
}

func TestUnit_BootstrapAuthorizationConditionErrorsBoundsReportedErrors(t *testing.T) {
	t.Parallel()

	var conditionErrors bootstrapAuthorizationConditionErrors
	for range maxReportedBootstrapAuthorizationConditionErrors + 2 {
		conditionErrors.add(errors.New("condition update failed"))
	}

	require.Len(t, conditionErrors.errs, maxReportedBootstrapAuthorizationConditionErrors)
	require.Equal(t, 2, conditionErrors.omitted)
	require.Contains(t, conditionErrors.err().Error(), "2 additional Machine condition update errors omitted")
}

func TestUnit_BootstrapAuthorizationMachinePredicate(t *testing.T) {
	t.Parallel()

	p := bootstrapAuthorizationMachinePredicate()
	selected := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")

	conditionOnly := selected.DeepCopy()
	conditionOnly.Status.Conditions = []metav1.Condition{{Type: BootstrapAuthorizationSyncedCondition}}
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: selected, ObjectNew: conditionOnly}))

	identityChanged := selected.DeepCopy()
	identityChanged.Annotations[keys.MachineBootstrapIdentity.String()] = "spiffe://example.org/bootstrap/changed"
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: selected, ObjectNew: identityChanged}))

	joined := selected.DeepCopy()
	joined.Status.NodeRef = clusterv1.MachineNodeReference{Name: joined.Name}
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: selected, ObjectNew: joined}))

	unselected := selected.DeepCopy()
	keys.SyncBootstrapAuthorization.Delete(unselected.Labels)
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: selected, ObjectNew: unselected}))
	require.True(t, p.Create(event.CreateEvent{Object: selected}))
	require.False(t, p.Create(event.CreateEvent{Object: unselected}))
}

func TestUnit_BootstrapAuthorizationControllerReconcileReportsTenantClientFailure(t *testing.T) {
	t.Parallel()

	cluster := newClusterForBootstrapAuthorizationTest()
	machine := newMachineForBootstrapAuthorizationTest("worker-a", "spiffe://example.org/bootstrap/a")
	controller, _ := newBootstrapAuthorizationControllerForTest(t, []client.Object{cluster, machine})
	tenantErr := errors.New("tenant client failed")
	controller.clusterCache = staticBootstrapAuthorizationClusterCache{err: tenantErr}

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
	require.ErrorIs(t, err, tenantErr)
	assertBootstrapAuthorizationCondition(
		t,
		controller.k8sClient,
		machine,
		metav1.ConditionFalse,
		BootstrapAuthorizationSyncFailedReason,
	)
}

func TestUnit_BootstrapAuthorizationControllerIgnoresMissingCluster(t *testing.T) {
	t.Parallel()

	controller, _ := newBootstrapAuthorizationControllerForTest(t, nil)
	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: testNamespace,
		Name:      "gone",
	}})
	require.NoError(t, err)
}

func TestUnit_BootstrapAuthorizationControllerMachineWatchMapping(t *testing.T) {
	t.Parallel()

	machine := newMachineForBootstrapAuthorizationTest("worker-a", "")
	request, ok := clusterRequestForMachine(machine)

	require.True(t, ok)
	require.Equal(t, reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: testNamespace,
		Name:      testBootstrapAuthorizationClusterName,
	}}, request)
}

type staticBootstrapAuthorizationClusterCache struct {
	client client.Client
	err    error
}

var _ clustercache.ClusterCache = staticBootstrapAuthorizationClusterCache{}

func (s staticBootstrapAuthorizationClusterCache) GetClient(context.Context, client.ObjectKey) (client.Client, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.client, nil
}

func (s staticBootstrapAuthorizationClusterCache) GetReader(ctx context.Context, cluster client.ObjectKey) (client.Reader, error) {
	return s.GetClient(ctx, cluster)
}

func (s staticBootstrapAuthorizationClusterCache) GetUncachedClient(ctx context.Context, cluster client.ObjectKey) (client.Client, error) {
	return s.GetClient(ctx, cluster)
}

func (s staticBootstrapAuthorizationClusterCache) GetRESTConfig(context.Context, client.ObjectKey) (*rest.Config, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &rest.Config{}, nil
}

func (s staticBootstrapAuthorizationClusterCache) GetHealthCheckingState(context.Context, client.ObjectKey) clustercache.HealthCheckingState {
	return clustercache.HealthCheckingState{}
}

func (s staticBootstrapAuthorizationClusterCache) Watch(context.Context, client.ObjectKey, clustercache.Watcher) error {
	return s.err
}

func (s staticBootstrapAuthorizationClusterCache) GetClusterSource(
	string,
	func(context.Context, client.Object) []ctrl.Request,
	...clustercache.GetClusterSourceOption,
) source.Source {
	return nil
}

func newClusterForBootstrapAuthorizationTest() *clusterv1.Cluster {
	return &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name:      testBootstrapAuthorizationClusterName,
		Namespace: testNamespace,
		UID:       types.UID("cluster-uid"),
	}}
}

func markClusterDeleting(cluster *clusterv1.Cluster) {
	now := metav1.Now()
	cluster.DeletionTimestamp = &now
	cluster.Finalizers = []string{"test.nvke.nvidia.com/bootstrap-authorization"}
}

func newMachineForBootstrapAuthorizationTest(name, identity string) *clusterv1.Machine {
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			UID:       types.UID(name + "-uid"),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:               testBootstrapAuthorizationClusterName,
				keys.SyncBootstrapAuthorization.String(): "true",
			},
			Annotations: map[string]string{},
		},
		Spec: clusterv1.MachineSpec{ClusterName: testBootstrapAuthorizationClusterName},
	}
	if identity != "" {
		machine.Annotations[keys.MachineBootstrapIdentity.String()] = identity
	}
	return machine
}

func newBootstrapAuthorizationControllerForTest(
	t *testing.T,
	managementObjects []client.Object,
	tenantObjects ...client.Object,
) (*AuthorizationController, client.Client) {
	t.Helper()

	managementScheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(managementScheme))
	managementClient := fake.NewClientBuilder().
		WithScheme(managementScheme).
		WithStatusSubresource(&clusterv1.Machine{}).
		WithObjects(managementObjects...).
		Build()

	tenantScheme := runtime.NewScheme()
	require.NoError(t, rbacv1.AddToScheme(tenantScheme))
	tenantClient := fake.NewClientBuilder().WithScheme(tenantScheme).WithObjects(tenantObjects...).Build()

	return &AuthorizationController{
		k8sClient:    managementClient,
		clusterCache: staticBootstrapAuthorizationClusterCache{client: tenantClient},
		logger:       slog.New(slog.DiscardHandler),
	}, tenantClient
}

func assertBootstrapClusterRoleBinding(
	t *testing.T,
	tenantClient client.Client,
	name string,
	roleName string,
	wantSubjects []rbacv1.Subject,
) {
	t.Helper()
	got := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Name: name}, got))
	require.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: roleName}, got.RoleRef)
	require.Equal(t, wantSubjects, got.Subjects)
}

func assertBootstrapRoleBinding(
	t *testing.T,
	tenantClient client.Client,
	binding bootstrapRoleBinding,
	wantSubjects []rbacv1.Subject,
) {
	t.Helper()
	got := &rbacv1.RoleBinding{}
	require.NoError(t, tenantClient.Get(t.Context(), client.ObjectKey{Namespace: binding.namespace, Name: binding.name}, got))
	require.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: binding.roleName}, got.RoleRef)
	require.Equal(t, wantSubjects, got.Subjects)
}

func assertBootstrapAuthorizationCondition(
	t *testing.T,
	k8sClient client.Client,
	machine *clusterv1.Machine,
	status metav1.ConditionStatus,
	reason string,
) {
	t.Helper()
	got := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), got))
	condition := meta.FindStatusCondition(got.Status.Conditions, BootstrapAuthorizationSyncedCondition)
	require.NotNil(t, condition)
	require.Equal(t, status, condition.Status)
	require.Equal(t, reason, condition.Reason)
}
