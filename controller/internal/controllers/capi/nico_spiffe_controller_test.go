// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
)

func TestUnit_NicoSpiffeControllerReconcile(t *testing.T) {
	t.Parallel()

	type parameters struct {
		method       config.SpireAttestationMethod
		status       nicov1.NicoMachineStatus
		existingPath string
		ownerRef     bool
		deleting     bool
		wantPath     string
	}

	tests := map[string]parameters{
		"annotates a TPM machine from its EK public hash": {
			method:   config.SpireAttestationMethodTPM,
			status:   nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current"},
			ownerRef: true,
			wantPath: constants.TPMSpireAgentPath + "hash-current",
		},
		"annotates a DSX IMDS machine from its primary DPU ID": {
			method:   config.SpireAttestationMethodDSXIMDS,
			status:   nicov1.NicoMachineStatus{PrimaryDPUMachineID: "dpu-123"},
			ownerRef: true,
			wantPath: constants.DSXIMDSSpireAgentPath + "operator-1/bm/dpu-123",
		},
		"preserves an annotation for another attestation method": {
			method:       config.SpireAttestationMethodDSXIMDS,
			status:       nicov1.NicoMachineStatus{PrimaryDPUMachineID: "dpu-123"},
			existingPath: constants.JoinTokenAlternateSpireAgentPath + "host-123",
			ownerRef:     true,
			wantPath:     constants.JoinTokenAlternateSpireAgentPath + "host-123",
		},
		"waits for the configured identity source": {
			method:   config.SpireAttestationMethodDSXIMDS,
			ownerRef: true,
		},
		"waits for a Machine owner reference": {
			method: config.SpireAttestationMethodDSXIMDS,
			status: nicov1.NicoMachineStatus{PrimaryDPUMachineID: "dpu-123"},
		},
		"skips a deleting NicoMachine": {
			method:   config.SpireAttestationMethodDSXIMDS,
			status:   nicov1.NicoMachineStatus{PrimaryDPUMachineID: "dpu-123"},
			ownerRef: true,
			deleting: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForNicoSpiffeTest(params.existingPath)
			nicoMachine := newNicoMachineForSpiffeTest(params.status, params.ownerRef, params.deleting)
			controller := newNicoSpiffeControllerForTest(t, params.method, machine, nicoMachine)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: nicoMachine.Namespace,
				Name:      nicoMachine.Name,
			}})
			require.NoError(t, err)

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			if params.wantPath == "" {
				require.NotContains(t, gotMachine.Annotations, constants.MachineAnnotationKeySpireAgentSpiffeIDPath)
				return
			}
			require.Equal(t, params.wantPath, gotMachine.Annotations[constants.MachineAnnotationKeySpireAgentSpiffeIDPath])
		})
	}
}

func TestUnit_NicoSpiffeControllerReconcileIgnoresMissingNicoMachine(t *testing.T) {
	t.Parallel()
	controller := newNicoSpiffeControllerForTest(t, config.SpireAttestationMethodDSXIMDS)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test",
		Name:      "worker-gone",
	}})

	require.NoError(t, err)
}

func newMachineForNicoSpiffeTest(existingPath string) *clusterv1.Machine {
	annotations := map[string]string{}
	if existingPath != "" {
		annotations[constants.MachineAnnotationKeySpireAgentSpiffeIDPath] = existingPath
	}
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-0",
			Namespace:   "cluster-test",
			Annotations: annotations,
		},
	}
}

func newNicoMachineForSpiffeTest(status nicov1.NicoMachineStatus, ownerRef, deleting bool) *nicov1.NicoMachine {
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: nicov1.GroupVersion.String(), Kind: "NicoMachine"},
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "cluster-test"},
		Status:     status,
	}
	if ownerRef {
		isController := true
		nicoMachine.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       "worker-0",
			Controller: &isController,
		}}
	}
	if deleting {
		now := metav1.Now()
		nicoMachine.DeletionTimestamp = &now
		nicoMachine.Finalizers = []string{"test.finalizer"}
	}
	return nicoMachine
}

func newNicoSpiffeControllerForTest(t *testing.T, method config.SpireAttestationMethod, objects ...client.Object) *NicoSpiffeController {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))

	return &NicoSpiffeController{
		cfg: config.Config{
			NodeAgent: config.NodeAgentConfig{
				SpireAttestationMethod: method,
				DSXImds: config.DSXImdsConfig{
					OperatorTenant: "operator-1",
				},
			},
		},
		k8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		logger:    slog.New(slog.DiscardHandler),
	}
}
