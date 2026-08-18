// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
)

func TestNicoTpmSpiffeControllerReconcileAggregatesPubhashesAndAnnotatesMachine(t *testing.T) {
	currentMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-test",
		},
	}

	currentNicoMachine := newNicoMachineForTest("worker-0", "hash-current", false, currentMachine.Name)

	otherNicoMachine := newNicoMachineForTest("worker-1", "hash-other", false, "")
	noHashNicoMachine := newNicoMachineForTest("worker-2", "", false, "")
	deletingNicoMachine := newNicoMachineForTest("worker-3", "hash-deleting", true, "")

	controller := newNicoTpmSpiffeControllerForTest(
		t,
		currentMachine,
		currentNicoMachine,
		otherNicoMachine,
		noHashNicoMachine,
		deletingNicoMachine,
	)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: currentNicoMachine.Namespace,
			Name:      currentNicoMachine.Name,
		},
	})
	require.NoError(t, err)

	assertConfigMapEntries(t, controller, "hash-current", "hash-other")
	assertMachineAnnotated(t, controller, currentMachine, constants.TPMSpireAgentPath+"hash-current")
}

func TestNicoTpmSpiffeControllerReconcileCreatesConfigMapWhenRequestedMachineIsGone(t *testing.T) {
	survivingNicoMachine := newNicoMachineForTest("worker-1", "hash-surviving", false, "")

	controller := newNicoTpmSpiffeControllerForTest(t, survivingNicoMachine)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "cluster-test",
			Name:      "worker-stale",
		},
	})
	require.NoError(t, err)

	assertConfigMapEntries(t, controller, "hash-surviving")
}

func TestNicoTpmSpiffeControllerReconcilePreservesExistingNonTPMSpiffePath(t *testing.T) {
	joinTokenSpiffePath := constants.JoinTokenAlternateSpireAgentPath + "host-123"
	currentMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-test",
			Annotations: map[string]string{
				constants.MachineAnnotationKeySpireAgentSpiffeIDPath: joinTokenSpiffePath,
			},
		},
	}

	currentNicoMachine := newNicoMachineForTest("worker-0", "hash-current", false, currentMachine.Name)

	controller := newNicoTpmSpiffeControllerForTest(t, currentMachine, currentNicoMachine)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: currentNicoMachine.Namespace,
			Name:      currentNicoMachine.Name,
		},
	})
	require.NoError(t, err)

	assertConfigMapEntries(t, controller)
	assertMachineAnnotated(t, controller, currentMachine, joinTokenSpiffePath)
}

func TestNicoTpmSpiffeControllerReconcileSkipsMachineAnnotationWhenRequestedMachineHasNoHash(t *testing.T) {
	currentMachine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-test",
		},
	}

	currentNicoMachine := newNicoMachineForTest("worker-0", "", false, currentMachine.Name)
	otherNicoMachine := newNicoMachineForTest("worker-1", "hash-other", false, "")

	controller := newNicoTpmSpiffeControllerForTest(t, currentMachine, currentNicoMachine, otherNicoMachine)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: currentNicoMachine.Namespace,
			Name:      currentNicoMachine.Name,
		},
	})
	require.NoError(t, err)

	assertConfigMapEntries(t, controller, "hash-other")
	assertMachineNotAnnotated(t, controller, currentMachine)
}

func newNicoMachineForTest(name, tpmEkPubHash string, deleting bool, ownerMachineName string) *nicov1.NicoMachine {
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "cluster-test",
		},
		Status: nicov1.NicoMachineStatus{
			TpmEkPubHash: tpmEkPubHash,
		},
	}

	if deleting {
		now := metav1.Now()
		nicoMachine.DeletionTimestamp = &now
		nicoMachine.Finalizers = []string{"test.finalizer"}
	}

	if ownerMachineName != "" {
		isController := true
		nicoMachine.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
			Name:       ownerMachineName,
			Controller: &isController,
		}}
	}

	return nicoMachine
}

func newNicoTpmSpiffeControllerForTest(t *testing.T, objs ...client.Object) *NicoTpmSpiffeController {
	t.Helper()

	return &NicoTpmSpiffeController{
		cfg: config.Config{
			SpireServerNamespace:           "spire-server",
			SpireTPMPubhashesConfigMapName: "tpm-pubhashes",
		},
		k8sClient: fake.NewClientBuilder().
			WithScheme(newNicoTpmSpiffeControllerTestScheme(t)).
			WithObjects(objs...).
			Build(),
		logger: slog.New(slog.DiscardHandler),
	}
}

func newNicoTpmSpiffeControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))

	return scheme
}

func assertConfigMapEntries(t *testing.T, controller *NicoTpmSpiffeController, hashes ...string) {
	t.Helper()

	configMap := &corev1.ConfigMap{}
	err := controller.k8sClient.Get(context.Background(), types.NamespacedName{
		Namespace: "spire-server",
		Name:      "tpm-pubhashes",
	}, configMap)
	require.NoError(t, err)

	expectedData := make(map[string]string, len(hashes))
	for _, hash := range hashes {
		expectedData[hash] = constants.LabelKeyNkeManagedBy + "=" + constants.FieldManagerNicoTpmSpiffe
	}

	if len(expectedData) == 0 && len(configMap.Data) == 0 {
		return
	}
	require.Equal(t, expectedData, configMap.Data)
}

func assertMachineAnnotated(t *testing.T, controller *NicoTpmSpiffeController, machine *clusterv1.Machine, expectedPath string) {
	t.Helper()

	gotMachine := getMachineForTest(t, controller, machine)
	require.Equal(t, expectedPath, gotMachine.Annotations[constants.MachineAnnotationKeySpireAgentSpiffeIDPath])
}

func assertMachineNotAnnotated(t *testing.T, controller *NicoTpmSpiffeController, machine *clusterv1.Machine) {
	t.Helper()

	gotMachine := getMachineForTest(t, controller, machine)
	require.NotContains(t, gotMachine.Annotations, constants.MachineAnnotationKeySpireAgentSpiffeIDPath)
}

func getMachineForTest(t *testing.T, controller *NicoTpmSpiffeController, machine *clusterv1.Machine) *clusterv1.Machine {
	t.Helper()

	gotMachine := &clusterv1.Machine{}
	err := controller.k8sClient.Get(context.Background(), types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      machine.Name,
	}, gotMachine)
	require.NoError(t, err)

	return gotMachine
}
