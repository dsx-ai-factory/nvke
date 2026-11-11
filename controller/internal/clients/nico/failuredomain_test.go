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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
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

func TestFailureDomainUnavailable(t *testing.T) {
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
		deps := shared.MachineAdvanceDeps{K8sClient: newNicoMachineRotationClient(t, nicoMachine)}
		got, err := New().FailureDomainUnavailable(context.Background(), deps, machine)
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("NicoMachine not created yet", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		require.NoError(t, nicov1.AddToScheme(scheme))
		deps := shared.MachineAdvanceDeps{K8sClient: fake.NewClientBuilder().WithScheme(scheme).Build()}
		got, err := New().FailureDomainUnavailable(context.Background(), deps, machine)
		require.NoError(t, err)
		assert.False(t, got)
	})
}
