// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"
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

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
)

const (
	providerIDTestNodeName            = "worker-0"
	providerIDTestManagedByLabelValue = "nvke-controller"
)

func TestNicoProviderIDControllerReconcilePatchesTenantNodeProviderID(t *testing.T) {
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
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

func TestNicoProviderIDControllerReconcileNoOpsWhenTenantNodeProviderIDMatches(t *testing.T) {
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
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

func TestNicoProviderIDControllerReconcilePatchesTenantNodeTopologyLabels(t *testing.T) {
	nicoMachine := newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName)
	nicoMachine.Status = nicov1.NicoMachineStatus{
		MachineID: "machine-123",
		SiteID:    "site-123",
		SiteName:  "New York / A",
		VPCID:     "vpc-123",
		VPCName:   "Production VPC",
	}
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
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
		"example.com/keep":         "value",
		keys.MachineID.String():    "machine-123",
		keys.NodeSiteID.String():   "site-123",
		keys.NodeSiteName.String(): "new-york-a",
		keys.NodeVPCID.String():    "vpc-123",
		keys.NodeVPCName.String():  "production-vpc",
	}, node.Labels)
}

// A Node labeled by a build that wrote nke.nvidia.com/* is moved onto
// nvke.nvidia.com/* rather than ending up carrying both.
func TestNicoProviderIDControllerReconcileMigratesLegacyTopologyLabels(t *testing.T) {
	nicoMachine := newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName)
	nicoMachine.Status = nicov1.NicoMachineStatus{
		MachineID: "machine-123",
		SiteID:    "site-123",
		SiteName:  "New York / A",
		VPCID:     "vpc-123",
		VPCName:   "Production VPC",
	}
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			nicoMachine,
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://instance-1", map[string]string{
				"example.com/keep":         "value",
				keys.MachineID.Legacy():    "machine-123",
				keys.NodeSiteID.Legacy():   "site-123",
				keys.NodeSiteName.Legacy(): "new-york-a",
				keys.NodeVPCID.Legacy():    "vpc-123",
				keys.NodeVPCName.Legacy():  "production-vpc",
			}),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, map[string]string{
		"example.com/keep":         "value",
		keys.MachineID.String():    "machine-123",
		keys.NodeSiteID.String():   "site-123",
		keys.NodeSiteName.String(): "new-york-a",
		keys.NodeVPCID.String():    "vpc-123",
		keys.NodeVPCName.String():  "production-vpc",
	}, node.Labels)
}

// Clearing a topology label has to clear it in both domains; removing only the
// current one would leave a stale legacy copy that nothing else sweeps up.
func TestNicoProviderIDControllerReconcileRemovesLegacyTopologyLabels(t *testing.T) {
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://instance-1", map[string]string{
				"example.com/keep":         "value",
				keys.MachineID.Legacy():    "stale-machine",
				keys.NodeSiteID.Legacy():   "stale-site",
				keys.NodeSiteName.Legacy(): "stale-site-name",
				keys.NodeVPCID.Legacy():    "stale-vpc",
				keys.NodeVPCName.Legacy():  "stale-vpc-name",
			}),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, map[string]string{"example.com/keep": "value"}, node.Labels)
}

func TestNicoProviderIDControllerReconcileRemovesMissingTopologyLabels(t *testing.T) {
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
		},
		[]client.Object{
			newTenantNodeForProviderIDTest("nico://instance-1", map[string]string{
				"example.com/keep":         "value",
				keys.MachineID.String():    "stale-machine",
				keys.NodeSiteID.String():   "stale-site",
				keys.NodeSiteName.String(): "stale-site-name",
				keys.NodeVPCID.String():    "stale-vpc",
				keys.NodeVPCName.String():  "stale-vpc-name",
			}),
		},
	)

	_, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)

	node := getTenantNodeForProviderIDTest(t, tenantClient)
	require.Equal(t, map[string]string{"example.com/keep": "value"}, node.Labels)
}

func TestNicoProviderIDControllerReconcileErrorsWhenTenantNodeProviderIDDiffers(t *testing.T) {
	controller, tenantClient := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
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

func TestNicoProviderIDControllerReconcileWaitsForNicoMachineProviderID(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newNicoProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("", providerIDTestNodeName),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

func TestNicoProviderIDControllerReconcileWaitsForOwnerMachine(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newNicoProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", ""),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

func TestNicoProviderIDControllerReconcileWaitsForKubeconfigSecret(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{
		err: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "test-kubeconfig"),
	}
	controller := newNicoProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Equal(t, tenantClusterReadinessRetryAfter, result.RequeueAfter)
	require.Equal(t, 1, tenantProvider.calls)
}

func TestNicoProviderIDControllerReconcileWaitsWhenTenantClientNotReady(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{
		err: fmt.Errorf("%w: published kubeconfig not ready", kube.ErrTenantClientNotReady),
	}
	controller := newNicoProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Equal(t, tenantClusterReadinessRetryAfter, result.RequeueAfter)
	require.Equal(t, 1, tenantProvider.calls)
}

func TestNicoProviderIDControllerReconcileSkipsWhenCAPIClusterIsGone(t *testing.T) {
	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newNicoProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

func TestNicoProviderIDControllerReconcileChecksNVKEOwnershipOfCAPICluster(t *testing.T) {
	tests := []struct {
		name               string
		clusterLabels      map[string]string
		providerErr        error
		wantErr            bool
		wantProviderCalls  int
		wantNodeProviderID string
	}{
		{
			name:               "cluster without managed-by label is skipped",
			clusterLabels:      nil,
			wantProviderCalls:  0,
			wantNodeProviderID: "",
		},
		{
			name:               "cluster managed by another value is skipped",
			clusterLabels:      map[string]string{keys.ManagedBy.String(): "someone-else"},
			wantProviderCalls:  0,
			wantNodeProviderID: "",
		},
		{
			name:               "cluster with legacy-domain managed-by label is reconciled",
			clusterLabels:      map[string]string{keys.ManagedBy.Legacy(): providerIDTestManagedByLabelValue},
			wantProviderCalls:  1,
			wantNodeProviderID: "nico://instance-1",
		},
		{
			name:          "NVKE-managed cluster without a cluster row keeps the missing-record error",
			clusterLabels: map[string]string{keys.ManagedBy.String(): providerIDTestManagedByLabelValue},
			providerErr: fmt.Errorf("no cluster row found for CAPI cluster %s/%s: %w",
				"cluster-test", "test-cluster", db.ErrNotFound),
			wantErr:            true,
			wantProviderCalls:  1,
			wantNodeProviderID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newCAPIClusterForProviderIDTest()
			cluster.Labels = tt.clusterLabels

			tenantClient := fake.NewClientBuilder().
				WithScheme(newTenantClientProviderIDTestScheme(t)).
				WithObjects(newTenantNodeForProviderIDTest("")).
				Build()
			tenantProvider := &fakeTenantClusterClientProvider{
				tenantClient: tenantClient,
				err:          tt.providerErr,
			}
			controller := newNicoProviderIDControllerForTestWithProvider(
				t,
				[]client.Object{
					cluster,
					newMachineForProviderIDTest(),
					newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
				},
				tenantProvider,
			)

			result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
			if tt.wantErr {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, ctrl.Result{}, result)
			require.Equal(t, tt.wantProviderCalls, tenantProvider.calls)

			node := getTenantNodeForProviderIDTest(t, tenantClient)
			require.Equal(t, tt.wantNodeProviderID, node.Spec.ProviderID)
		})
	}
}

func TestIsNVKEManagedCluster(t *testing.T) {
	tests := []struct {
		name            string
		clusterLabels   map[string]string
		configuredValue string
		want            bool
	}{
		{
			name:            "matching label is managed",
			clusterLabels:   map[string]string{keys.ManagedBy.String(): providerIDTestManagedByLabelValue},
			configuredValue: providerIDTestManagedByLabelValue,
			want:            true,
		},
		{
			name:            "empty configured value does not match an unlabeled cluster",
			clusterLabels:   nil,
			configuredValue: "",
			want:            false,
		},
		{
			name:            "empty configured value does not match an empty label",
			clusterLabels:   map[string]string{keys.ManagedBy.String(): ""},
			configuredValue: "",
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newCAPIClusterForProviderIDTest()
			cluster.Labels = tt.clusterLabels
			require.Equal(t, tt.want, isNVKEManagedCluster(cluster, tt.configuredValue))
		})
	}
}

func TestNicoProviderIDControllerReconcileWaitsForTenantNode(t *testing.T) {
	controller, _ := newNicoProviderIDControllerForTest(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName),
		},
		nil,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Equal(t, tenantClusterReadinessRetryAfter, result.RequeueAfter)
}

func TestNicoProviderIDControllerReconcileSkipsDeletingNicoMachine(t *testing.T) {
	deletingNicoMachine := newNicoMachineForProviderIDTest("nico://instance-1", providerIDTestNodeName)
	now := metav1.Now()
	deletingNicoMachine.DeletionTimestamp = &now
	deletingNicoMachine.Finalizers = []string{"test.finalizer"}

	tenantProvider := &fakeTenantClusterClientProvider{}
	controller := newNicoProviderIDControllerForTestWithProvider(
		t,
		[]client.Object{
			newCAPIClusterForProviderIDTest(),
			newMachineForProviderIDTest(),
			deletingNicoMachine,
		},
		tenantProvider,
	)

	result, err := controller.Reconcile(context.Background(), providerIDReconcileRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 0, tenantProvider.calls)
}

type fakeTenantClusterClientProvider struct {
	tenantClient client.Client
	err          error
	calls        int
}

func (p *fakeTenantClusterClientProvider) ClientForCluster(
	context.Context,
	*clusterv1.Cluster,
) (client.Client, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return p.tenantClient, nil
}

func newNicoProviderIDControllerForTest(
	t *testing.T,
	managementObjects []client.Object,
	tenantObjects []client.Object,
) (*NicoProviderIDController, client.Client) {
	t.Helper()

	tenantClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderIDTestScheme(t)).
		WithObjects(tenantObjects...).
		Build()
	tenantProvider := &fakeTenantClusterClientProvider{
		tenantClient: tenantClient,
	}
	controller := newNicoProviderIDControllerForTestWithProvider(t, managementObjects, tenantProvider)
	return controller, tenantClient
}

func newNicoProviderIDControllerForTestWithProvider(
	t *testing.T,
	managementObjects []client.Object,
	tenantProvider kube.TenantClientProvider,
) *NicoProviderIDController {
	t.Helper()

	managementClient := fake.NewClientBuilder().
		WithScheme(newNicoProviderIDControllerTestScheme(t)).
		WithObjects(managementObjects...).
		Build()
	return &NicoProviderIDController{
		k8sClient:            managementClient,
		tenantClientProvider: tenantProvider,
		logger:               slog.New(slog.DiscardHandler),
		managedByLabelValue:  providerIDTestManagedByLabelValue,
	}
}

func newNicoProviderIDControllerTestScheme(t *testing.T) *runtime.Scheme {
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
			Labels:    map[string]string{keys.ManagedBy.String(): providerIDTestManagedByLabelValue},
		},
	}
}

func newMachineForProviderIDTest() *clusterv1.Machine {
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerIDTestNodeName,
			Namespace: "cluster-test",
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "test-cluster",
		},
	}
}

func newNicoMachineForProviderIDTest(providerID, ownerMachineName string) *nicov1.NicoMachine {
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerIDTestNodeName,
			Namespace: "cluster-test",
		},
		Spec: nicov1.NicoMachineSpec{
			ProviderID: providerID,
		},
	}

	if ownerMachineName != "" {
		isController := true
		nicoMachine.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       ownerMachineName,
			Controller: &isController,
		}}
	}

	return nicoMachine
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
