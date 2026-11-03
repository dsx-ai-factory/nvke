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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const nicoSpiffeTestHostID = "cd4aac60-00ca-4eae-b04e-e4c6c4afad14"

func TestUnit_NicoSpiffeControllerReconcile(t *testing.T) {
	t.Parallel()

	type parameters struct {
		method        config.SpireAttestationMethod
		status        nicov1.NicoMachineStatus
		existingPath  string
		ownerRef      bool
		deleting      bool
		missingHostID bool
		invalidHostID bool
		wantErr       string
		wantPath      string
	}

	tests := map[string]parameters{
		"annotates a TPM machine from its EK public hash": {
			method:   config.SpireAttestationMethodTPM,
			status:   nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current", PrimaryDPUMachineID: "dpu-123"},
			ownerRef: true,
			wantPath: constants.TPMSpireAgentPath + "hash-current",
		},
		"annotates a DSX IMDS machine from its primary DPU ID": {
			method:   config.SpireAttestationMethodDSXIMDS,
			status:   nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current", PrimaryDPUMachineID: "dpu-123"},
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
		"preserves an existing join-token path from a migrated node": {
			method:       config.SpireAttestationMethodJoinToken,
			status:       nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current", PrimaryDPUMachineID: "dpu-123"},
			existingPath: constants.JoinTokenAlternateSpireAgentPath + nicoSpiffeTestHostID,
			ownerRef:     true,
			wantPath:     constants.JoinTokenAlternateSpireAgentPath + nicoSpiffeTestHostID,
		},
		"waits for the configured identity source": {
			method:   config.SpireAttestationMethodDSXIMDS,
			ownerRef: true,
		},
		"waits for the attestation method annotation": {
			status:   nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current"},
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
		"skips a Machine without a host ID": {
			method:        config.SpireAttestationMethodTPM,
			status:        nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current"},
			ownerRef:      true,
			missingHostID: true,
		},
		"rejects an invalid host ID": {
			method:        config.SpireAttestationMethodTPM,
			status:        nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current"},
			ownerRef:      true,
			invalidHostID: true,
			wantErr:       "invalid host ID",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForNicoSpiffeTest(params.existingPath, params.method)
			if params.missingHostID {
				keys.MachineHostID.Delete(machine.Annotations)
			}
			if params.invalidHostID {
				machine.Annotations[keys.MachineHostID.String()] = "not-a-uuid"
			}
			nicoMachine := newNicoMachineForSpiffeTest(params.status, params.ownerRef, params.deleting)
			controller := newNicoSpiffeControllerForTest(t, machine, nicoMachine)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: nicoMachine.Namespace,
				Name:      nicoMachine.Name,
			}})
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
			} else {
				require.NoError(t, err)
			}

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			if params.wantPath == "" {
				require.NotContains(t, gotMachine.Annotations, keys.MachineSpireAgentSpiffeIDPath.String())
				return
			}
			require.Equal(t, params.wantPath, gotMachine.Annotations[keys.MachineSpireAgentSpiffeIDPath.String()])
		})
	}
}

func TestUnit_NicoSpiffeControllerReconcileIgnoresMissingNicoMachine(t *testing.T) {
	t.Parallel()
	controller := newNicoSpiffeControllerForTest(t)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test",
		Name:      "worker-gone",
	}})

	require.NoError(t, err)
}

func TestUnit_NicoSpiffeControllerMachineWatchPredicate(t *testing.T) {
	t.Parallel()
	pred := machineIdentityWatchPredicate()
	machine := newMachineForNicoSpiffeTest("", config.SpireAttestationMethodTPM)
	withoutHostID := machine.DeepCopy()
	keys.MachineHostID.Delete(withoutHostID.Annotations)
	withAgentPath := machine.DeepCopy()
	withAgentPath.Annotations[keys.MachineSpireAgentSpiffeIDPath.String()] = constants.TPMSpireAgentPath + "hash"
	withoutMethod := machine.DeepCopy()
	keys.MachineSpireAttestationMethod.Delete(withoutMethod.Annotations)
	spiffeToggled := machine.DeepCopy()
	spiffeToggled.Annotations[keys.MachineSpiffeEnabled.String()] = "false"

	require.False(t, pred.Create(event.CreateEvent{Object: withoutHostID}))
	require.True(t, pred.Create(event.CreateEvent{Object: machine}))
	require.True(t, pred.Update(event.UpdateEvent{ObjectOld: withoutHostID, ObjectNew: machine}))
	require.True(t, pred.Update(event.UpdateEvent{ObjectOld: withoutMethod, ObjectNew: machine}))
	require.True(t, pred.Update(event.UpdateEvent{ObjectOld: machine, ObjectNew: spiffeToggled}))
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: machine, ObjectNew: withAgentPath}))
	require.False(t, pred.Delete(event.DeleteEvent{Object: machine}))
}

func TestUnit_NicoSpiffeControllerMachineWatchMapping(t *testing.T) {
	t.Parallel()
	type parameters struct {
		apiGroup string
		kind     string
		name     string
		other    bool
		wantName string
	}
	tests := map[string]parameters{
		"routes a Machine to its NicoMachine infrastructure reference": {
			apiGroup: nicov1.GroupVersion.Group,
			kind:     "NicoMachine",
			name:     "nico-infra-1",
			wantName: "nico-infra-1",
		},
		"ignores another infrastructure kind": {
			apiGroup: nicov1.GroupVersion.Group,
			kind:     "DockerMachine",
			name:     "docker-infra-1",
		},
		"ignores another API group": {
			apiGroup: "example.org",
			kind:     "NicoMachine",
			name:     "nico-infra-1",
		},
		"ignores a missing infrastructure name": {
			apiGroup: nicov1.GroupVersion.Group,
			kind:     "NicoMachine",
		},
		"ignores a non-Machine object": {other: true},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var obj client.Object = newMachineForNicoSpiffeTest("", config.SpireAttestationMethodTPM)
			if params.other {
				obj = newNicoMachineForSpiffeTest(nicov1.NicoMachineStatus{}, false, false)
			} else {
				machine := obj.(*clusterv1.Machine)
				machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
					APIGroup: params.apiGroup,
					Kind:     params.kind,
					Name:     params.name,
				}
			}
			requests := nicoMachineRequestsForMachine(t.Context(), obj)
			if params.wantName == "" {
				require.Empty(t, requests)
				return
			}
			require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{
				Namespace: "cluster-test", Name: params.wantName,
			}}}, requests)
		})
	}
}

func TestUnit_NicoSpiffeControllerReconcileAfterMachineHostIDAdded(t *testing.T) {
	t.Parallel()
	machine := newMachineForNicoSpiffeTest("", config.SpireAttestationMethodTPM)
	keys.MachineHostID.Delete(machine.Annotations)
	machine.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: nicov1.GroupVersion.Group,
		Kind:     "NicoMachine",
		Name:     "worker-0",
	}
	nicoMachine := newNicoMachineForSpiffeTest(nicov1.NicoMachineStatus{TpmEkPubHash: "hash-current"}, true, false)
	controller := newNicoSpiffeControllerForTest(t, machine, nicoMachine)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: nicoMachine.Namespace, Name: nicoMachine.Name}}

	_, err := controller.Reconcile(t.Context(), req)
	require.NoError(t, err)

	updated := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), updated))
	updated.Annotations[keys.MachineHostID.String()] = nicoSpiffeTestHostID
	require.NoError(t, controller.k8sClient.Update(t.Context(), updated))
	require.True(t, machineIdentityWatchPredicate().Update(event.UpdateEvent{ObjectOld: machine, ObjectNew: updated}))
	require.Equal(t, []reconcile.Request{req}, nicoMachineRequestsForMachine(t.Context(), updated))

	_, err = controller.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), updated))
	require.Equal(t, constants.TPMSpireAgentPath+"hash-current",
		updated.Annotations[keys.MachineSpireAgentSpiffeIDPath.String()])
}

func newMachineForNicoSpiffeTest(existingPath string, method config.SpireAttestationMethod) *clusterv1.Machine {
	annotations := map[string]string{
		keys.MachineHostID.String():        nicoSpiffeTestHostID,
		keys.MachineSpiffeEnabled.String(): "true",
	}
	if method != "" {
		annotations[keys.MachineSpireAttestationMethod.String()] = string(method)
	}
	if existingPath != "" {
		annotations[keys.MachineSpireAgentSpiffeIDPath.String()] = existingPath
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

func newNicoSpiffeControllerForTest(t *testing.T, objects ...client.Object) *NicoSpiffeController {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))

	return &NicoSpiffeController{
		cfg: config.Config{
			NodeAgent: config.NodeAgentConfig{
				SpireAttestationMethod: config.SpireAttestationMethodTPM,
				DSXImds: config.DSXImdsConfig{
					OperatorTenant: "operator-1",
				},
			},
		},
		k8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		logger:    slog.New(slog.DiscardHandler),
	}
}
