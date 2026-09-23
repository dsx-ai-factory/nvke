// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"log/slog"
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
)

func TestMachineSpireEntryControllerReconcileCreatesClusterStaticEntry(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-test",
			Annotations: map[string]string{
				constants.MachineAnnotationKeyHostID:                 "host-123",
				constants.MachineAnnotationKeySpireAgentSpiffeIDPath: "/spire/agent/tpm/test-hash",
			},
		},
	}

	controller := &MachineSpireEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		k8sClient:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build(),
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	_, err := controller.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test",
		Name:      "worker-0",
	}})
	require.NoError(t, err)

	cse := &spirev1alpha1.ClusterStaticEntry{}
	err = controller.k8sClient.Get(context.Background(), types.NamespacedName{
		Name: machineSpireEntryName("host-123"),
	}, cse)
	require.NoError(t, err)
	require.Equal(t, "spiffe://example.org/host/host-123", cse.Spec.SPIFFEID)
	require.Equal(t, "spiffe://example.org/spire/agent/tpm/test-hash", cse.Spec.ParentID)
}

func TestMachineSpireEntryControllerReconcileDeletesClusterStaticEntryWhenMachineDeleting(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-test",
			Annotations: map[string]string{
				constants.MachineAnnotationKeyHostID: "host-123",
			},
			Finalizers:        []string{"test.finalizer"},
			DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
		},
	}

	cse := &spirev1alpha1.ClusterStaticEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name: machineSpireEntryName("host-123"),
		},
	}

	controller := &MachineSpireEntryController{
		k8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine, cse).Build(),
		logger:    slog.New(slog.DiscardHandler),
	}

	_, err := controller.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test",
		Name:      "worker-0",
	}})
	require.NoError(t, err)

	err = controller.k8sClient.Get(context.Background(), types.NamespacedName{Name: cse.Name}, &spirev1alpha1.ClusterStaticEntry{})
	require.Error(t, err)
	require.True(t, apierrors.IsNotFound(err))
}
