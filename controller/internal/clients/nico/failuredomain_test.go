// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestIsFailureDomainUnavailable(t *testing.T) {
	t.Parallel()

	const message = `failure domain "fd-a": No Machines are available for specified Instance Type`
	tests := []struct {
		name       string
		status     metav1.ConditionStatus
		reason     string
		message    string
		generation int64
		observed   int64
		instanceID string
		want       bool
	}{
		{name: "out of capacity in the requested domain", status: metav1.ConditionFalse, reason: nicov1.FailureDomainUnavailableReason, message: message, want: true},
		{name: "current generation", status: metav1.ConditionFalse, reason: nicov1.FailureDomainUnavailableReason, message: message, generation: 2, observed: 2, want: true},
		{name: "older generation", status: metav1.ConditionFalse, reason: nicov1.FailureDomainUnavailableReason, message: message, generation: 3, observed: 2},
		{name: "report about the previous domain", status: metav1.ConditionFalse, reason: nicov1.FailureDomainUnavailableReason, message: `failure domain "fd-b": no machines`},
		{name: "placement failed for another reason", status: metav1.ConditionFalse, reason: nicov1.FailureDomainPlacementFailedReason, message: message},
		{name: "instance type out everywhere", status: metav1.ConditionFalse, reason: nicov1.InstanceTypeUnavailableReason, message: message},
		{name: "instance allocated", status: metav1.ConditionFalse, reason: nicov1.FailureDomainUnavailableReason, message: message, instanceID: "i-1"},
		{name: "provisioned", status: metav1.ConditionTrue, reason: nicov1.FailureDomainUnavailableReason, message: message},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			nicoMachine := &nicov1.NicoMachine{}
			nicoMachine.Generation = tt.generation
			nicoMachine.Status.InstanceID = tt.instanceID
			nicoMachine.Status.Conditions = []metav1.Condition{{
				Type:               nicov1.MachineProvisionedCondition,
				Status:             tt.status,
				Reason:             tt.reason,
				Message:            tt.message,
				ObservedGeneration: tt.observed,
			}}
			assert.Equal(t, tt.want, isFailureDomainUnavailable(nicoMachine, "fd-a"))
		})
	}
}

func TestRotateFailureDomainPassesUnavailable(t *testing.T) {
	t.Parallel()

	machine := testCAPINicoMachine()
	machine.Spec.FailureDomain = "fd-a"

	t.Run("reads the NicoMachine of the requested domain", func(t *testing.T) {
		t.Parallel()
		nicoMachine := &nicov1.NicoMachine{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "capi-cluster-a"}}
		nicoMachine.Status.Conditions = []metav1.Condition{{
			Type:    nicov1.MachineProvisionedCondition,
			Status:  metav1.ConditionFalse,
			Reason:  nicov1.FailureDomainUnavailableReason,
			Message: `failure domain "fd-a": No Machines are available for specified Instance Type`,
		}}
		assert.True(t, rotateUnavailable(t, newNicoMachineRotationClient(t, nicoMachine), machine))
	})

	t.Run("NicoMachine not created yet", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		require.NoError(t, nicov1.AddToScheme(scheme))
		assert.False(t, rotateUnavailable(t, fake.NewClientBuilder().WithScheme(scheme).Build(), machine))
	})
}

func rotateUnavailable(t *testing.T, k8sClient client.Client, machine *clusterv1.Machine) bool {
	t.Helper()
	var got bool
	deps := shared.MachineAdvanceDeps{
		K8sClient: k8sClient,
		RotateFailureDomain: func(_ context.Context, _ db.Node, _ *clusterv1.Machine, unavailable bool) (bool, error) {
			got = unavailable
			return false, nil
		},
	}
	_, err := RotateFailureDomain(context.Background(), deps, db.Node{}, machine)
	require.NoError(t, err)
	return got
}
