// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/kube"
)

const providerIDTestNodeName = "worker-0"

func TestMachineProviderIDControllerReconcilePatchesTenantNodeProviderID(t *testing.T) {
	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest(""),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, "nico://instance-1", node.Spec.ProviderID)
}

func TestMachineProviderIDControllerReconcileNoOpsWhenTenantNodeProviderIDMatches(t *testing.T) {
	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://instance-1"),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, "nico://instance-1", node.Spec.ProviderID)
}

// A provider other than NICo still gets its providerID copied. This is the case
// that the preprovisioned provider and CAPD take.
func TestMachineProviderIDControllerReconcilePatchesNonNicoProviderID(t *testing.T) {
	machine := newMachineForProviderIDTest("preprovisioned://test-cluster/host-01")
	machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "PreprovisionedMachine",
		Name:     providerIDTestNodeName,
	}

	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			machine,
		},
		[]client.Object{
			newTenantNodeForProviderIDTest(""),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, "preprovisioned://test-cluster/host-01", node.Spec.ProviderID)
}

// Forge topology labels belong to the NICo provider. Another provider reports
// no topology, so the controller must leave those labels alone rather than
// delete them.
func TestMachineProviderIDControllerReconcileLeavesTopologyLabelsForNonNicoProvider(t *testing.T) {
	machine := newMachineForProviderIDTest("preprovisioned://test-cluster/host-01")
	machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "PreprovisionedMachine",
		Name:     providerIDTestNodeName,
	}

	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			machine,
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("", map[string]string{
				"example.com/keep":           "value",
				constants.LabelKeyNodeSiteID: "set-by-somebody-else",
			}),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, map[string]string{
		"example.com/keep":           "value",
		constants.LabelKeyNodeSiteID: "set-by-somebody-else",
	}, node.Labels)
}

func TestMachineProviderIDControllerReconcilePatchesTenantNodeTopologyLabels(t *testing.T) {
	nicoMachine := newNicoMachineForProviderIDTest()
	nicoMachine.Status = nicov1.NicoMachineStatus{
		MachineID: "machine-123",
		SiteID:    "site-123",
		SiteName:  "New York / A",
		VPCID:     "vpc-123",
		VPCName:   "Production VPC",
	}
	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			nicoMachine,
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://instance-1", map[string]string{"example.com/keep": "value"}),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, map[string]string{
		"example.com/keep":             "value",
		constants.LabelKeyMachineID:    "machine-123",
		constants.LabelKeyNodeSiteID:   "site-123",
		constants.LabelKeyNodeSiteName: "new-york-a",
		constants.LabelKeyNodeVPCID:    "vpc-123",
		constants.LabelKeyNodeVPCName:  "production-vpc",
	}, node.Labels)
}

func TestMachineProviderIDControllerReconcileRemovesMissingTopologyLabels(t *testing.T) {
	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://instance-1", map[string]string{
				"example.com/keep":             "value",
				constants.LabelKeyMachineID:    "stale-machine",
				constants.LabelKeyNodeSiteID:   "stale-site",
				constants.LabelKeyNodeSiteName: "stale-site-name",
				constants.LabelKeyNodeVPCID:    "stale-vpc",
				constants.LabelKeyNodeVPCName:  "stale-vpc-name",
			}),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, map[string]string{"example.com/keep": "value"}, node.Labels)
}

func TestMachineProviderIDControllerReconcileErrorsWhenTenantNodeProviderIDDiffers(t *testing.T) {
	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://other-instance"),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.Error(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, "nico://other-instance", node.Spec.ProviderID)
}

func TestMachineProviderIDControllerReconcileWaitsForMachineProviderID(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newMachineProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(""),
			newNicoMachineForProviderIDTest(),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

func TestMachineProviderIDControllerReconcileSkipsWhenMachineIsGone(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newMachineProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

func TestMachineProviderIDControllerReconcileWaitsForKubeconfigSecret(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{
		err: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "test-kubeconfig"),
	}
	controller := newMachineProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Equal(t, tenantClusterReadinessRetryAfter, result.RequeueAfter)
	require.Equal(t, 1, tenantProvider.calls)
}

func TestMachineProviderIDControllerReconcileSkipsWhenCAPIClusterIsGone(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newMachineProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

func TestMachineProviderIDControllerReconcileWaitsForTenantNode(t *testing.T) {
	controller, _ := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
			newNicoMachineForProviderIDTest(),
		},
		nil,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Equal(t, tenantClusterReadinessRetryAfter, result.RequeueAfter)
}

func TestMachineProviderIDControllerReconcileSkipsDeletingMachine(t *testing.T) {
	deletingMachine := newMachineForProviderIDTest("nico://instance-1")
	now := metav1.Now()
	deletingMachine.DeletionTimestamp = &now
	deletingMachine.Finalizers = []string{"test.finalizer"}

	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newMachineProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			deletingMachine,
			newNicoMachineForProviderIDTest(),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

// A NicoMachine that Cluster API deleted before this reconcile leaves the
// Machine with a providerID. The node still gets it, without topology labels.
func TestMachineProviderIDControllerReconcileToleratesMissingNicoMachine(t *testing.T) {
	controller, tenantClient := newMachineProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest("nico://instance-1"),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest(""),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, "nico://instance-1", node.Spec.ProviderID)
}

func TestInfraMachineToOwnerMachine(t *testing.T) {
	isController := true
	nicoMachine := newNicoMachineForProviderIDTest()
	nicoMachine.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       machineConst,
		Name:       providerIDTestNodeName,
		Controller: &isController,
	}}

	requests := infraMachineToOwnerMachine(context.Background(), nicoMachine)
	require.Equal(t, []ctrl.Request{{
		NamespacedName: types.NamespacedName{Namespace: "cluster-test", Name: providerIDTestNodeName},
	}}, requests)
}

func TestInfraMachineToOwnerMachineIgnoresOtherOwners(t *testing.T) {
	nicoMachine := newNicoMachineForProviderIDTest()
	nicoMachine.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "something-else",
	}}

	require.Nil(t, infraMachineToOwnerMachine(context.Background(), nicoMachine))
}

type fakeTenantClusterClientProvider struct {
	tenantClient client.Client
	err          error
	calls        int
}

func (p *fakeTenantClusterClientProvider) ClientForCluster(
	context.Context,
	*clusterv1.Cluster,
	...kube.AddToSchemeFunc,
) (client.Client, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return p.tenantClient, nil
}

func newMachineProviderIDControllerForTest(
	t *testing.T,
	managementObjects []client.Object,
	tenantObjects []client.Object,
) (*MachineProviderIDController, client.Client) {
	t.Helper()

	tenantClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderIDTestScheme(t)).
		WithObjects(tenantObjects...).
		Build()
	tenantProvider := &fakeTenantClusterClientProvider{
		tenantClient: tenantClient,
	}
	controller := newMachineProviderIDControllerForTestWithProvider(t, managementObjects, tenantProvider)
	return controller, tenantClient
}

func newMachineProviderIDControllerForTestWithProvider(
	t *testing.T,
	managementObjects []client.Object,
	tenantProvider kube.TenantClientProvider,
) *MachineProviderIDController {
	t.Helper()

	managementClient := fake.NewClientBuilder().
		WithScheme(newMachineProviderIDControllerTestScheme(t)).
		WithObjects(managementObjects...).
		Build()
	return &MachineProviderIDController{
		k8sClient:            managementClient,
		tenantClientProvider: tenantProvider,
		logger:               slog.New(slog.DiscardHandler),
		watchNicoMachines:    true,
	}
}

func newMachineProviderIDControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	return scheme
}

func newTenantClientProviderIDTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func newCAPIClusterForProviderIDTest() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "cluster-test",
			UID:       types.UID("test-cluster-uid"),
		},
	}
}

// newMachineForProviderIDTest builds the Machine that the Cluster API machine
// controller would have reconciled: it carries the providerID copied from the
// infrastructure machine, and the reference to that object.
func newMachineForProviderIDTest(providerID string) *clusterv1.Machine {
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerIDTestNodeName,
			Namespace: "cluster-test",
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "test-cluster",
			ProviderID:  providerID,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     nicoMachineKind,
				Name:     providerIDTestNodeName,
			},
		},
	}
}

func newNicoMachineForProviderIDTest() *nicov1.NicoMachine {
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       nicoMachineKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerIDTestNodeName,
			Namespace: "cluster-test",
		},
	}
}

func newTenantNodeForProviderIDTest(providerID string, labels ...map[string]string) *corev1.Node {
	var nodeLabels map[string]string
	if len(labels) > 0 {
		nodeLabels = labels[0]
	}

	return &corev1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   providerIDTestNodeName,
			Labels: nodeLabels,
		},
		Spec: corev1.NodeSpec{
			ProviderID: providerID,
		},
	}
}

func providerIDReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "cluster-test",
			Name:      providerIDTestNodeName,
		},
	}
}

func getTenantNodeForProviderIDTest(t *testing.T, tenantClient client.Client) *corev1.Node {
	t.Helper()

	node := &corev1.Node{}
	err := tenantClient.Get(context.Background(), client.ObjectKey{Name: providerIDTestNodeName}, node)
	require.NoError(t, err)
	return node
}

var _ kube.TenantClientProvider = (*fakeTenantClusterClientProvider)(nil)
