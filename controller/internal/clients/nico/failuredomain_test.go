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

func TestFailureDomainUnavailable(t *testing.T) {
	t.Parallel()

	provisioned := func(status metav1.ConditionStatus, reason string, observedGeneration int64) metav1.Condition {
		return metav1.Condition{
			Type:               nicov1.MachineProvisionedCondition,
			Status:             status,
			Reason:             reason,
			ObservedGeneration: observedGeneration,
		}
	}
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{"domain out of capacity", []metav1.Condition{provisioned(metav1.ConditionFalse, nicov1.FailureDomainUnavailableReason, 2)}, true},
		{"no observed generation", []metav1.Condition{provisioned(metav1.ConditionFalse, nicov1.FailureDomainUnavailableReason, 0)}, true},
		{"placement refused for a non-capacity reason", []metav1.Condition{provisioned(metav1.ConditionFalse, nicov1.FailureDomainPlacementFailedReason, 2)}, false},
		{"NICo create failed", []metav1.Condition{provisioned(metav1.ConditionFalse, nicov1.InstanceCreateFailedReason, 2)}, false},
		{"provisioned", []metav1.Condition{provisioned(metav1.ConditionTrue, nicov1.InstanceReadyReason, 2)}, false},
		{"condition predates the current spec", []metav1.Condition{provisioned(metav1.ConditionFalse, nicov1.FailureDomainUnavailableReason, 1)}, false},
		{"no condition yet", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			nicoMachine := &nicov1.NicoMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "capi-cluster-a", Generation: 2},
				Status:     nicov1.NicoMachineStatus{Conditions: tt.conditions},
			}
			deps := shared.MachineAdvanceDeps{K8sClient: newNicoMachineRotationClient(t, nicoMachine)}
			got, err := New().FailureDomainUnavailable(context.Background(), deps, testCAPINicoMachine())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("NicoMachine not created yet", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		require.NoError(t, nicov1.AddToScheme(scheme))
		deps := shared.MachineAdvanceDeps{K8sClient: fake.NewClientBuilder().WithScheme(scheme).Build()}
		got, err := New().FailureDomainUnavailable(context.Background(), deps, testCAPINicoMachine())
		require.NoError(t, err)
		assert.False(t, got)
	})
}
