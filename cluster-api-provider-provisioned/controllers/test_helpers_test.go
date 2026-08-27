// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

const (
	testNamespace   = "cluster-test"
	testClusterName = "test"
	testMachineName = "test-cp-a1b2"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, infrav1.AddToScheme(scheme))
	return scheme
}

func newInfraCluster(hosts ...infrav1.ProvisionedHost) *infrav1.ProvisionedCluster {
	return &infrav1.ProvisionedCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: infrav1.GroupVersion.String(), Kind: "ProvisionedCluster"},
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       infrav1.ProvisionedClusterSpec{Hosts: hosts},
	}
}

func newCAPICluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: infrav1.GroupName, Kind: "ProvisionedCluster", Name: testClusterName,
		}},
	}
}

func newOwnerMachine() *clusterv1.Machine { return newOwnerMachineNamed(testMachineName) }

func newOwnerMachineNamed(name string) *clusterv1.Machine {
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			Labels: map[string]string{clusterv1.ClusterNameLabel: testClusterName},
		},
		Spec: clusterv1.MachineSpec{ClusterName: testClusterName},
	}
}

func newInfraMachine(name, providerID string) *infrav1.ProvisionedMachine {
	isController := true
	return &infrav1.ProvisionedMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: infrav1.GroupVersion.String(), Kind: "ProvisionedMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			UID:    types.UID("uid-" + name),
			Labels: map[string]string{clusterv1.ClusterNameLabel: testClusterName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: name, Controller: &isController,
			}},
		},
		Spec: infrav1.ProvisionedMachineSpec{ProviderID: providerID},
	}
}

func getInfraMachine(t *testing.T, k8sClient client.Client, name string) *infrav1.ProvisionedMachine {
	t.Helper()
	machine := &infrav1.ProvisionedMachine{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: name}, machine))
	return machine
}

func newMachineReconcilerForTest(t *testing.T, objects ...client.Object) (*MachineReconciler, client.Client) {
	t.Helper()
	k8sClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1.ProvisionedMachine{}, &infrav1.ProvisionedCluster{}).
		Build()
	return NewMachineReconciler(k8sClient, k8sClient, slog.New(slog.DiscardHandler)), k8sClient
}

func requireReadyCondition(t *testing.T, conditions []metav1.Condition, status metav1.ConditionStatus, reason string) {
	t.Helper()
	for _, condition := range conditions {
		if condition.Type == infrav1.ReadyCondition {
			require.Equal(t, status, condition.Status)
			require.Equal(t, reason, condition.Reason)
			return
		}
	}
	t.Fatalf("no %s condition found in %v", infrav1.ReadyCondition, conditions)
}
