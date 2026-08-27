// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

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

func newInfraMachine(name, providerID string) *infrav1.ProvisionedMachine {
	isController := true
	return &infrav1.ProvisionedMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: infrav1.GroupVersion.String(), Kind: "ProvisionedMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			Labels: map[string]string{clusterv1.ClusterNameLabel: testClusterName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: name, Controller: &isController,
			}},
		},
		Spec: infrav1.ProvisionedMachineSpec{ProviderID: providerID},
	}
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
