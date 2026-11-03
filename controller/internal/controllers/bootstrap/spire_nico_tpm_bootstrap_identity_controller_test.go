// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	testNamespace   = "cluster-test"
	testMachineName = "worker-0"
	testMachineUID  = types.UID("machine-uid")
	testTrustDomain = "example.org"
	testSpireClass  = "spire"
)

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileCreatesClusterStaticEntryAndPublishesIdentity(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
		keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
	})
	nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("hash-current")
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, nicoMachine)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	wantSpiffeID := "spiffe://example.org/bootstrap/machine-uid"
	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.Equal(t, wantSpiffeID, keys.MachineBootstrapIdentity.Value(gotMachine.Annotations))
	require.False(t, keys.MachineSpireAgentSpiffeIDPath.Has(gotMachine.Annotations))
	require.Contains(t, gotMachine.Finalizers, spireNicoTPMBootstrapIdentityFinalizer)
	assertBootstrapIdentityCondition(t, gotMachine, metav1.ConditionTrue, BootstrapIdentityResolvedReason)

	cse := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, cse))
	require.Equal(t, testSpireClass, cse.Spec.ClassName)
	require.Equal(t, "spiffe://example.org/spire/agent/tpm/hash-current", cse.Spec.ParentID)
	require.Equal(t, wantSpiffeID, cse.Spec.SPIFFEID)
	require.Equal(t, []string{spireWorkloadEntryDefaultSelector}, cse.Spec.Selectors)
	require.Equal(t, spireWorkloadEntryDefaultHint, cse.Spec.Hint)
	require.Equal(t, constants.FieldManagerSpireNicoTPMBootstrapIdentity, keys.ManagedBy.Value(cse.Labels))
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileUsesWorkloadConfigAnnotation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		annotation string
		selectors  []string
		hint       string
	}{
		"replaces hint and selectors": {
			annotation: `{"hint":"custom-join","selectors":["unix:path:/opt/custom/exec-auth","k8s:sa:bootstrap"]}`,
			selectors:  []string{"unix:path:/opt/custom/exec-auth", "k8s:sa:bootstrap"},
			hint:       "custom-join",
		},
		"omitted hint stays empty": {
			annotation: `{"selectors":["unix:path:/opt/custom/exec-auth"]}`,
			selectors:  []string{"unix:path:/opt/custom/exec-auth"},
		},
		"empty hint stays empty": {
			annotation: `{"hint":"","selectors":["unix:path:/opt/custom/exec-auth"]}`,
			selectors:  []string{"unix:path:/opt/custom/exec-auth"},
		},
		"passes selector values through to SPIRE": {
			annotation: `{"selectors":[""]}`,
			selectors:  []string{""},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
			kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
				keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
			})
			kubeadmConfig.Annotations = map[string]string{
				keys.BootstrapIdentitySpireWorkloadEntryConfig.String(): test.annotation,
			}
			nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("hash-current")
			controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, nicoMachine)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			require.NoError(t, err)

			cse := &spirev1alpha1.ClusterStaticEntry{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, cse))
			require.Equal(t, test.selectors, cse.Spec.Selectors)
			require.Equal(t, test.hint, cse.Spec.Hint)
		})
	}
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileRejectsInvalidWorkloadConfigAnnotation(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"invalid JSON":    `{`,
		"hint only":       `{"hint":"custom-join"}`,
		"empty selectors": `{"hint":"custom-join","selectors":[]}`,
		"null selectors":  `{"hint":"custom-join","selectors":null}`,
	}

	for name, annotation := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
			kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
				keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
			})
			kubeadmConfig.Annotations = map[string]string{keys.BootstrapIdentitySpireWorkloadEntryConfig.String(): annotation}
			nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("hash-current")
			controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, nicoMachine)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			require.NoError(t, err)

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
			assertBootstrapIdentityCondition(t, gotMachine, metav1.ConditionFalse, BootstrapIdentityConfigurationInvalidReason)

			cse := &spirev1alpha1.ClusterStaticEntry{}
			err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, cse)
			require.True(t, apierrors.IsNotFound(err), "error: %v", err)
		})
	}
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileSkipsUnselectedMachines(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		labels  map[string]string
		tpmHash string
	}{
		"missing label": {
			tpmHash: "hash-current",
		},
		"different resolver": {
			labels:  map[string]string{keys.BootstrapIdentityResolver.String(): "launchlayer-dsx-imds"},
			tpmHash: "hash-current",
		},
		"waits for the TPM EK public hash": {
			labels: map[string]string{keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
			kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(test.labels)
			nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest(test.tpmHash)
			controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, nicoMachine)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			require.NoError(t, err)

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
			require.False(t, keys.MachineSpireAgentSpiffeIDPath.Has(gotMachine.Annotations))
			if name == "waits for the TPM EK public hash" {
				assertBootstrapIdentityCondition(t, gotMachine, metav1.ConditionFalse, BootstrapIdentityWaitingForIdentityDataReason)
			}

			cse := &spirev1alpha1.ClusterStaticEntry{}
			err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, cse)
			require.True(t, apierrors.IsNotFound(err), "error: %v", err)
		})
	}
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileDeletesClusterStaticEntryAndAnnotationWhenMachineDeleting(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "spiffe://example.org/bootstrap/machine-uid"
	machine.Finalizers = []string{"test.finalizer", spireNicoTPMBootstrapIdentityFinalizer}
	machine.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, cse)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err), "error: %v", err)

	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
	require.NotContains(t, gotMachine.Finalizers, spireNicoTPMBootstrapIdentityFinalizer)
	require.Contains(t, gotMachine.Finalizers, "test.finalizer")
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileDeletesClusterStaticEntryAndAnnotationWhenOptedOut(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "spiffe://example.org/bootstrap/machine-uid"
	machine.Finalizers = []string{spireNicoTPMBootstrapIdentityFinalizer}
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(nil)
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, cse)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err), "error: %v", err)

	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
	require.NotContains(t, gotMachine.Finalizers, spireNicoTPMBootstrapIdentityFinalizer)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileSkipsOptOutCleanupForAnotherResolver(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "dsx-imds://bootstrap/machine-uid"
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(nil)
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	cse.Labels[keys.ManagedBy.String()] = "other-bootstrap-identity-controller"
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, cse)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	gotCSE := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, gotCSE))

	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.Equal(t, "dsx-imds://bootstrap/machine-uid", keys.MachineBootstrapIdentity.Value(gotMachine.Annotations))
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileSkipsDeletionCleanupForAnotherResolver(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "dsx-imds://bootstrap/machine-uid"
	machine.Finalizers = []string{"other.example/bootstrap-identity"}
	machine.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	cse.Labels[keys.ManagedBy.String()] = "other-bootstrap-identity-controller"
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, cse)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{}))
	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.Equal(t, "dsx-imds://bootstrap/machine-uid", keys.MachineBootstrapIdentity.Value(gotMachine.Annotations))
	require.Equal(t, []string{"other.example/bootstrap-identity"}, gotMachine.Finalizers)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileSkipsNodeRefCleanupForAnotherResolver(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "dsx-imds://bootstrap/machine-uid"
	machine.Status.NodeRef = clusterv1.MachineNodeReference{Name: "node-a"}
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
		keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
	})
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	cse.Labels[keys.ManagedBy.String()] = "other-bootstrap-identity-controller"
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, cse)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{}))
	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.Equal(t, "dsx-imds://bootstrap/machine-uid", keys.MachineBootstrapIdentity.Value(gotMachine.Annotations))
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileCleansUpBootstrapIdentityWhenNodeRefIsSet(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "spiffe://example.org/bootstrap/machine-uid"
	machine.Finalizers = []string{spireNicoTPMBootstrapIdentityFinalizer}
	machine.Status.NodeRef = clusterv1.MachineNodeReference{Name: "node-a"}
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
		keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
	})
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, cse)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err), "error: %v", err)

	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
	require.NotContains(t, gotMachine.Finalizers, spireNicoTPMBootstrapIdentityFinalizer)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileReportsUnresolvedNicoMachine(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		includeNico bool
		mutate      func(*clusterv1.Machine, *nicov1.NicoMachine)
		reason      string
	}{
		"infrastructure ref is not a NicoMachine": {
			includeNico: true,
			mutate: func(machine *clusterv1.Machine, _ *nicov1.NicoMachine) {
				machine.Spec.InfrastructureRef.Kind = "OtherMachine"
			},
			reason: BootstrapIdentityInfrastructureInvalidReason,
		},
		"NicoMachine does not exist yet": {
			reason: BootstrapIdentityWaitingForInfrastructureReason,
		},
		"NicoMachine is not controlled by the Machine": {
			includeNico: true,
			mutate: func(_ *clusterv1.Machine, nicoMachine *nicov1.NicoMachine) {
				nicoMachine.OwnerReferences = nil
			},
			reason: BootstrapIdentityInfrastructureInvalidReason,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
			kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
				keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
			})
			nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("hash-current")
			if test.mutate != nil {
				test.mutate(machine, nicoMachine)
			}
			objects := []client.Object{machine, kubeadmConfig}
			if test.includeNico {
				objects = append(objects, nicoMachine)
			}
			controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, objects...)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			require.NoError(t, err)

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
			assertBootstrapIdentityCondition(t, gotMachine, metav1.ConditionFalse, test.reason)

			err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{})
			require.True(t, apierrors.IsNotFound(err), "error: %v", err)
		})
	}
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileReturnsPublishFailure(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
		keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
	})
	nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("hash-current")
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, nicoMachine)
	applyErr := errors.New("apply failed")
	controller.k8sClient = interceptor.NewClient(controller.k8sClient.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*spirev1alpha1.ClusterStaticEntry); ok {
				return applyErr
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.ErrorIs(t, err, applyErr)

	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
	require.Contains(t, gotMachine.Finalizers, spireNicoTPMBootstrapIdentityFinalizer)
	assertBootstrapIdentityCondition(t, gotMachine, metav1.ConditionFalse, BootstrapIdentityFailedReason)

	err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err), "error: %v", err)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileTreatsMissingKubeadmConfigAsUnselected(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		annotation  string
		finalizer   bool
		wantCleared bool
	}{
		"removes an identity this controller published": {
			annotation:  "spiffe://example.org/bootstrap/machine-uid",
			wantCleared: true,
		},
		"removes an entry protected before identity publication": {
			finalizer:   true,
			wantCleared: true,
		},
		"leaves another resolver's identity": {
			annotation: "dsx-imds://bootstrap/machine-uid",
		},
		"does nothing before an identity is published": {},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
			if test.annotation != "" {
				machine.Annotations[keys.MachineBootstrapIdentity.String()] = test.annotation
			}
			if test.finalizer {
				machine.Finalizers = []string{spireNicoTPMBootstrapIdentityFinalizer}
			}
			cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
			controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, cse)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			require.NoError(t, err)

			gotCSE := &spirev1alpha1.ClusterStaticEntry{}
			err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, gotCSE)
			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			if test.wantCleared {
				require.True(t, apierrors.IsNotFound(err), "error: %v", err)
				require.False(t, keys.MachineBootstrapIdentity.Has(gotMachine.Annotations))
				require.NotContains(t, gotMachine.Finalizers, spireNicoTPMBootstrapIdentityFinalizer)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.annotation, keys.MachineBootstrapIdentity.Value(gotMachine.Annotations))
		})
	}
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerReconcileKeepsPublishedIdentityWhenKubeadmConfigGetFails(t *testing.T) {
	t.Parallel()

	machine := newMachineForSpireNicoTPMBootstrapIdentityTest()
	machine.Annotations[keys.MachineBootstrapIdentity.String()] = "spiffe://example.org/bootstrap/machine-uid"
	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(map[string]string{
		keys.BootstrapIdentityResolver.String(): constants.LabelValueBootstrapIdentityResolverSpireNicoTPM,
	})
	cse := newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine)
	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t, machine, kubeadmConfig, cse)
	readErr := errors.New("apiserver timeout")
	controller.k8sClient = interceptor.NewClient(controller.k8sClient.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*kubeadmv1.KubeadmConfig); ok {
				return readErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.ErrorIs(t, err, readErr)

	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: clusterStaticEntryName(machine)}, &spirev1alpha1.ClusterStaticEntry{}))
	gotMachine := &clusterv1.Machine{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
	require.Equal(t, "spiffe://example.org/bootstrap/machine-uid", keys.MachineBootstrapIdentity.Value(gotMachine.Annotations))
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerIgnoresMissingMachine(t *testing.T) {
	t.Parallel()

	controller := newSpireNicoTPMBootstrapIdentityControllerForTest(t)

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: testNamespace,
		Name:      "worker-gone",
	}})

	require.NoError(t, err)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerKubeadmConfigWatchMapping(t *testing.T) {
	t.Parallel()

	kubeadmConfig := newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(nil)

	requests := machineRequestForKubeadmConfig(t.Context(), kubeadmConfig)

	require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: testNamespace, Name: testMachineName,
	}}}, requests)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerNicoMachineWatchMapping(t *testing.T) {
	t.Parallel()

	nicoMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("hash-current")

	requests := machineRequestForNicoMachine(t.Context(), nicoMachine)

	require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: testNamespace, Name: testMachineName,
	}}}, requests)
}

func TestUnit_SpireNicoTPMBootstrapIdentityControllerNicoMachinePredicate(t *testing.T) {
	t.Parallel()

	pred := nicoMachineTPMHashChanged()
	oldMachine := newNicoMachineForSpireNicoTPMBootstrapIdentityTest("")
	newMachine := oldMachine.DeepCopy()
	newMachine.Status.TpmEkPubHash = "hash-current"
	sameHashMachine := oldMachine.DeepCopy()

	require.True(t, pred.Create(event.CreateEvent{Object: oldMachine}))
	require.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldMachine, ObjectNew: newMachine}))
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: oldMachine, ObjectNew: sameHashMachine}))
	require.False(t, pred.Delete(event.DeleteEvent{Object: oldMachine}))
}

func newMachineForSpireNicoTPMBootstrapIdentityTest() *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testMachineName,
			Namespace:   testNamespace,
			UID:         testMachineUID,
			Annotations: map[string]string{},
		},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: kubeadmv1.GroupVersion.Group,
				Kind:     "KubeadmConfig",
				Name:     "worker-bootstrap",
			}},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: nicov1.GroupVersion.Group,
				Kind:     "NicoMachine",
				Name:     "worker-infra",
			},
		},
	}
}

func newKubeadmConfigForSpireNicoTPMBootstrapIdentityTest(labels map[string]string) *kubeadmv1.KubeadmConfig {
	return &kubeadmv1.KubeadmConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "worker-bootstrap",
			Namespace:       testNamespace,
			UID:             types.UID("kubeadm-config-uid"),
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{machineOwnerRefForSpireNicoTPMBootstrapIdentityTest()},
		},
	}
}

func newNicoMachineForSpireNicoTPMBootstrapIdentityTest(tpmHash string) *nicov1.NicoMachine {
	return &nicov1.NicoMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "worker-infra",
			Namespace:       testNamespace,
			UID:             types.UID("nico-machine-uid"),
			OwnerReferences: []metav1.OwnerReference{machineOwnerRefForSpireNicoTPMBootstrapIdentityTest()},
		},
		Status: nicov1.NicoMachineStatus{TpmEkPubHash: tpmHash},
	}
}

func newClusterStaticEntryForSpireNicoTPMBootstrapIdentityTest(machine *clusterv1.Machine) *spirev1alpha1.ClusterStaticEntry {
	return &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       "ClusterStaticEntry",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterStaticEntryName(machine),
			Labels: map[string]string{
				keys.ManagedBy.String(): constants.FieldManagerSpireNicoTPMBootstrapIdentity,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: testSpireClass,
			ParentID:  "spiffe://example.org/spire/agent/tpm/hash-current",
			SPIFFEID:  "spiffe://example.org/bootstrap/machine-uid",
		},
	}
}

func machineOwnerRefForSpireNicoTPMBootstrapIdentityTest() metav1.OwnerReference {
	isController := true
	return metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Machine",
		Name:       testMachineName,
		UID:        testMachineUID,
		Controller: &isController,
	}
}

func newSpireNicoTPMBootstrapIdentityControllerForTest(
	t *testing.T,
	objects ...client.Object,
) *SpireNicoTPMBootstrapIdentityController {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, kubeadmv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	return &SpireNicoTPMBootstrapIdentityController{
		cfg:         config.Config{SpireClassName: testSpireClass},
		k8sClient:   fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&clusterv1.Machine{}).WithObjects(objects...).Build(),
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: testTrustDomain,
	}
}

func assertBootstrapIdentityCondition(t *testing.T, machine *clusterv1.Machine, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(machine.Status.Conditions, BootstrapIdentityResolvedCondition)
	require.NotNil(t, condition)
	require.Equal(t, status, condition.Status)
	require.Equal(t, reason, condition.Reason)
}
