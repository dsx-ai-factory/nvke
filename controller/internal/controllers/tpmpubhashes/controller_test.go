// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tpmpubhashes

import (
	"context"
	"log/slog"
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	testSpireClassName          = "spire"
	testSpireServerNamespace    = "spire-server"
	testTPMPubhashesCMName      = "tpm-pubhashes"
	testTPMPubhashesTrustDomain = "example.org"
)

func TestController_ReconcileRendersManagedClusterStaticEntries(t *testing.T) {
	controller := newControllerForTest(t,
		newTPMClusterStaticEntryForTest("host-a", constants.FieldManagerMachineSpireEntry, "hash-a"),
		newTPMClusterStaticEntryForTest("host-b", constants.FieldManagerMachineSpireEntry, "hash-b"),
	)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	assertTPMPubhashesConfigMapData(t, controller, map[string]string{
		"hash-a": keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
		"hash-b": keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
	})
}

// An entry applied before the nvke.nvidia.com rename carries the legacy
// managed-by label until its Machine reconciles again. A label selector
// matches one key, so missing it would drop that node's pubhash and break its
// TPM attestation.
func TestController_ReconcileRendersLegacyLabelledClusterStaticEntries(t *testing.T) {
	legacy := newTPMClusterStaticEntryForTest("host-legacy", constants.FieldManagerMachineSpireEntry, "hash-legacy")
	legacy.Labels = map[string]string{keys.ManagedBy.Legacy(): constants.FieldManagerMachineSpireEntry}

	controller := newControllerForTest(t,
		newTPMClusterStaticEntryForTest("host-a", constants.FieldManagerMachineSpireEntry, "hash-a"),
		legacy,
	)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	assertTPMPubhashesConfigMapData(t, controller, map[string]string{
		"hash-a":      keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
		"hash-legacy": keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
	})
}

func TestController_ReconcileFiltersClusterStaticEntries(t *testing.T) {
	controller := newControllerForTest(t,
		newTPMClusterStaticEntryForTest("host-valid", constants.FieldManagerMachineSpireEntry, "hash-valid"),
		newTPMClusterStaticEntryForTest("host-unmanaged", "cloud-workflow-engine", "hash-unmanaged"),
		newClusterStaticEntryForTest("host-wrong-class", "cloud-workflow-engine", "other-class", spiffeIDForTPMHash("hash-wrong-class")),
		newClusterStaticEntryForTest("host-non-tpm", constants.FieldManagerMachineSpireEntry, testSpireClassName, "spiffe://example.org/spire/agent/join-token/hash-non-tpm"),
	)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	assertTPMPubhashesConfigMapData(t, controller, map[string]string{
		"hash-valid": keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
	})
}

func TestController_ReconcileUpdatesConfigMapOutput(t *testing.T) {
	cse := newTPMClusterStaticEntryForTest("host-a", constants.FieldManagerMachineSpireEntry, "hash-old")
	controller := newControllerForTest(t, cse)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	cse.Spec.ParentID = spiffeIDForTPMHash("hash-new")
	require.NoError(t, controller.k8sClient.Update(context.Background(), cse))

	_, err = controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	assertTPMPubhashesConfigMapData(t, controller, map[string]string{
		"hash-new": keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
	})
}

func TestController_ReconcileDeletesConfigMapOutput(t *testing.T) {
	cse := newTPMClusterStaticEntryForTest("host-a", constants.FieldManagerMachineSpireEntry, "hash-a")
	controller := newControllerForTest(t, cse)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	require.NoError(t, controller.k8sClient.Delete(context.Background(), cse))

	_, err = controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	assertTPMPubhashesConfigMapData(t, controller, map[string]string{})
}

func TestController_ReconcilePreservesExternallyManagedConfigMapData(t *testing.T) {
	controller := newControllerForTest(t,
		newTPMClusterStaticEntryForTest("host-a", constants.FieldManagerMachineSpireEntry, "hash-a"),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testTPMPubhashesCMName,
				Namespace: testSpireServerNamespace,
			},
			Data: map[string]string{
				"bogus": "bad",
			},
		},
	)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	assertTPMPubhashesConfigMapData(t, controller, map[string]string{
		"hash-a": keys.ManagedBy.String() + "=" + constants.FieldManagerMachineSpireEntry,
		"bogus":  "bad",
	})
}

func TestController_ReconcileIsIdempotent(t *testing.T) {
	controller := newControllerForTest(t,
		newTPMClusterStaticEntryForTest("host-a", constants.FieldManagerMachineSpireEntry, "hash-a"),
	)

	_, err := controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	first := getTPMPubhashesConfigMapForTest(t, controller).DeepCopy()

	_, err = controller.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	second := getTPMPubhashesConfigMapForTest(t, controller)

	require.Equal(t, first.Data, second.Data)
}

func newControllerForTest(t *testing.T, objs ...client.Object) *Controller {
	t.Helper()

	return &Controller{
		cfg: config.Config{
			SpireClassName:                 testSpireClassName,
			SpireServerNamespace:           testSpireServerNamespace,
			SpireTPMPubhashesConfigMapName: testTPMPubhashesCMName,
			FieldManagerName:               "cloud-workflow-engine",
		},
		k8sClient: fake.NewClientBuilder().
			WithScheme(newControllerTestScheme(t)).
			WithObjects(objs...).
			Build(),
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: testTPMPubhashesTrustDomain,
	}
}

func newControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	return scheme
}

func newTPMClusterStaticEntryForTest(name, manager, hash string) *spirev1alpha1.ClusterStaticEntry {
	return newClusterStaticEntryForTest(name, manager, testSpireClassName, spiffeIDForTPMHash(hash))
}

func newClusterStaticEntryForTest(name, manager, className, parentID string) *spirev1alpha1.ClusterStaticEntry {
	return &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       "ClusterStaticEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				keys.ManagedBy.String(): manager,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: className,
			ParentID:  parentID,
		},
	}
}

func getTPMPubhashesConfigMapForTest(t *testing.T, controller *Controller) *corev1.ConfigMap {
	t.Helper()

	configMap := &corev1.ConfigMap{}
	err := controller.k8sClient.Get(context.Background(), types.NamespacedName{
		Namespace: testSpireServerNamespace,
		Name:      testTPMPubhashesCMName,
	}, configMap)
	require.NoError(t, err)

	return configMap
}

func assertTPMPubhashesConfigMapData(t *testing.T, controller *Controller, expectedData map[string]string) {
	t.Helper()

	configMap := getTPMPubhashesConfigMapForTest(t, controller)
	if len(expectedData) == 0 {
		require.Empty(t, configMap.Data)
		return
	}
	require.Equal(t, expectedData, configMap.Data)
}

func spiffeIDForTPMHash(hash string) string {
	return "spiffe://" + testTPMPubhashesTrustDomain + constants.TPMSpireAgentPath + hash
}
