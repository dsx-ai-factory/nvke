// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

const testProviderID = "provisioned://test/10.0.0.11"

func TestMachineReconcilerCopiesProviderIDToTenantNode(t *testing.T) {
	reconciler, tenantClient, _ := newMachineReconcilerWithTenant(t, "", nil)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, testProviderID, getTenantNode(t, tenantClient).Spec.ProviderID)
}

func TestMachineReconcilerKeepsMatchingTenantNodeProviderID(t *testing.T) {
	reconciler, tenantClient, _ := newMachineReconcilerWithTenant(t, testProviderID, nil)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, testProviderID, getTenantNode(t, tenantClient).Spec.ProviderID)
}

func TestMachineReconcilerRejectsDifferentTenantNodeProviderID(t *testing.T) {
	reconciler, tenantClient, _ := newMachineReconcilerWithTenant(t, "provisioned://other/10.0.0.12", nil)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.ErrorContains(t, err, "has provider ID")
	require.Equal(t, "provisioned://other/10.0.0.12", getTenantNode(t, tenantClient).Spec.ProviderID)
}

func TestMachineReconcilerDoesNotOverwriteAConcurrentTenantNodeProviderID(t *testing.T) {
	reconciler, tenantClient, factory := newMachineReconcilerWithTenant(t, "", nil)
	factory.client = &concurrentNodeUpdateClient{
		Client:     tenantClient,
		providerID: "provisioned://other/10.0.0.12",
	}

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{Requeue: true}, result)
	require.Equal(t, "provisioned://other/10.0.0.12", getTenantNode(t, tenantClient).Spec.ProviderID)
}

func TestMachineReconcilerWaitsForTenantNode(t *testing.T) {
	reconciler, tenantClient, _ := newMachineReconcilerWithTenant(t, "", nil)
	require.NoError(t, tenantClient.Delete(context.Background(), getTenantNode(t, tenantClient)))

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)
}

func TestMachineReconcilerWaitsForTenantKubeconfig(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "test-kubeconfig")
	reconciler, _, factory := newMachineReconcilerWithTenant(t, "", notFound)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)
	require.Equal(t, 1, factory.calls)
}

func TestMachineReconcilerReportsTenantClientError(t *testing.T) {
	reconciler, _, _ := newMachineReconcilerWithTenant(t, "", fmt.Errorf("invalid kubeconfig"))

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.ErrorContains(t, err, "failed to create tenant client")
}

type fakeTenantClientFactory struct {
	client client.Client
	err    error
	calls  int
}

type concurrentNodeUpdateClient struct {
	client.Client
	providerID string
	updated    bool
}

func (c *concurrentNodeUpdateClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if !c.updated {
		node := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), node); err != nil {
			return err
		}
		node.Spec.ProviderID = c.providerID
		if err := c.Update(ctx, node); err != nil {
			return err
		}
		c.updated = true
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (f *fakeTenantClientFactory) ClientForCluster(context.Context, *clusterv1.Cluster) (client.Client, error) {
	f.calls++
	return f.client, f.err
}

func newMachineReconcilerWithTenant(
	t *testing.T,
	nodeProviderID string,
	factoryErr error,
) (*MachineReconciler, client.Client, *fakeTenantClientFactory) {
	t.Helper()

	reconciler, _ := newMachineReconcilerForTest(t,
		newInfraCluster(infrav1.ProvisionedHost{Hostname: "host-01", IPAddress: "10.0.0.11"}),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, testProviderID),
	)
	tenantClient := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName},
		Spec:       corev1.NodeSpec{ProviderID: nodeProviderID},
	}).Build()
	factory := &fakeTenantClientFactory{client: tenantClient, err: factoryErr}
	reconciler.tenantClients = factory
	return reconciler, tenantClient, factory
}

func getTenantNode(t *testing.T, tenantClient client.Client) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: testMachineName}, node))
	return node
}

var _ tenantClientFactory = (*fakeTenantClientFactory)(nil)
