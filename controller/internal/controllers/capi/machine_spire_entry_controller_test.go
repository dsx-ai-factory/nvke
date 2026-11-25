// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestMachineSpireEntryControllerReconcileCreatesClusterStaticEntry(t *testing.T) {
	t.Parallel()
	const nodeID = "45f59791-87df-4e85-9430-e565166ad020"

	tests := map[string]struct {
		agentConnection string
		wantSpiffeID    string
	}{
		"legacy SPIFFE node keeps host path": {
			agentConnection: "spiffe",
			wantSpiffeID:    "spiffe://example.org/host/" + nodeID,
		},
		"TLS JWT node uses canonical DSX exchange path": {
			agentConnection: "spiffe-jwt",
			wantSpiffeID:    "spiffe://example.org/tenant/nke/service/node-agent/bm/" + nodeID,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			require.NoError(t, clusterv1.AddToScheme(scheme))
			require.NoError(t, spirev1alpha1.AddToScheme(scheme))

			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "worker-0",
					Namespace: "cluster-test",
					Annotations: map[string]string{
						keys.MachineHostID.String():                 nodeID,
						keys.MachineSpireAgentSpiffeIDPath.String(): "/spire/agent/tpm/test-hash",
						keys.MachineAgentConnection.String():        test.agentConnection,
					},
				},
			}
			controller := &MachineSpireEntryController{
				cfg: config.Config{
					SpireClassName: "spire",
					NodeAgent: config.NodeAgentConfig{TLSJWT: config.TLSJWTConfig{
						SubjectTenant:  "nke",
						SubjectService: "node-agent",
					}},
				},
				k8sClient:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build(),
				logger:      slog.New(slog.DiscardHandler),
				trustDomain: "example.org",
			}

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: "cluster-test",
				Name:      "worker-0",
			}})
			require.NoError(t, err)

			cse := &spirev1alpha1.ClusterStaticEntry{}
			err = controller.k8sClient.Get(t.Context(), types.NamespacedName{
				Name: machineSpireEntryName(nodeID),
			}, cse)
			require.NoError(t, err)
			require.Equal(t, test.wantSpiffeID, cse.Spec.SPIFFEID)
			require.Equal(t, "spiffe://example.org/spire/agent/tpm/test-hash", cse.Spec.ParentID)
		})
	}
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
				keys.MachineHostID.String(): "host-123",
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

// TestMachineSpireEntryControllerReconcileWaitsForAgentConnection covers a
// Machine created before the controller wrote agent-connection: it gets no
// entry until the backfill sets the annotation, whose update reconciles it.
func TestMachineSpireEntryControllerReconcileWaitsForAgentConnection(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	const nodeID = "45f59791-87df-4e85-9430-e565166ad020"
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "cluster-test",
			Annotations: map[string]string{
				keys.MachineHostID.String():                 nodeID,
				keys.MachineSpireAgentSpiffeIDPath.String(): "/spire/agent/tpm/test-hash",
			},
		},
	}
	controller := &MachineSpireEntryController{
		k8sClient:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build(),
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	result, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test",
		Name:      "worker-0",
	}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Name: machineSpireEntryName(nodeID)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err))
}

const (
	testTPMParent      = "spiffe://example.org/spire/agent/tpm/test-hash"
	testOtherTPMParent = "spiffe://example.org/spire/agent/tpm/other-hash"
	testNewHostID      = "45f59791-87df-4e85-9430-e565166ad020"
	testOldHostID      = "0b9c1f0e-3a5d-4c55-9a51-0d6a3d2f8e11"
)

func testSpireEntry(parentID string, managed bool, created time.Time) *spirev1alpha1.ClusterStaticEntry {
	const hostID = testOldHostID
	entry := &spirev1alpha1.ClusterStaticEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:              machineSpireEntryName(hostID),
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ParentID:  parentID,
			Selectors: []string{"unix:uid:0"},
			SPIFFEID:  "spiffe://example.org/host/" + hostID,
		},
	}
	if managed {
		entry.Labels = map[string]string{keys.ManagedBy.String(): constants.FieldManagerMachineSpireEntry}
	}
	return entry
}

func testLegacyCWESpireEntry(entry *spirev1alpha1.ClusterStaticEntry) *spirev1alpha1.ClusterStaticEntry {
	entry.Labels = map[string]string{keys.ManagedBy.Legacy(): legacyCWEManagedBy}
	return entry
}

func testSpireMachine(deleting bool) *clusterv1.Machine {
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-old",
			Namespace:   "cluster-old",
			Annotations: map[string]string{keys.MachineHostID.String(): testOldHostID},
		},
	}
	if deleting {
		machine.Finalizers = []string{"test.finalizer"}
		machine.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}
	return machine
}

// testLiveMachine is an unrelated live Machine, so the sweep sees a populated Machine list.
func testLiveMachine() *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-live",
			Namespace:   "cluster-live",
			Annotations: map[string]string{keys.MachineHostID.String(): testNewHostID},
		},
	}
}

func newTestSpireEntryController(t *testing.T, objs ...client.Object) *MachineSpireEntryController {
	t.Helper()
	return newTestSpireEntryControllerWithInterceptor(t, interceptor.Funcs{}, objs...)
}

func newTestSpireEntryControllerWithInterceptor(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *MachineSpireEntryController {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))
	return &MachineSpireEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		k8sClient:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build(),
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}
}

func entryExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(t.Context(), types.NamespacedName{Name: name}, &spirev1alpha1.ClusterStaticEntry{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestMachineSpireEntryControllerReconcileCleansUpSiblingEntries(t *testing.T) {
	t.Parallel()
	old := time.Now().Add(-48 * time.Hour)

	tests := map[string]struct {
		objs        []client.Object
		wantRemoved []string
		wantKept    []string
	}{
		"orphan sibling with same parent is deleted": {
			objs:        []client.Object{testSpireEntry(testTPMParent, true, old)},
			wantRemoved: []string{machineSpireEntryName(testOldHostID)},
		},
		"sibling whose Machine still exists is kept": {
			objs: []client.Object{
				testSpireEntry(testTPMParent, true, old),
				testSpireMachine(false),
			},
			wantKept: []string{machineSpireEntryName(testOldHostID)},
		},
		"sibling whose Machine is deleting is deleted": {
			objs: []client.Object{
				testSpireEntry(testTPMParent, true, old),
				testSpireMachine(true),
			},
			wantRemoved: []string{machineSpireEntryName(testOldHostID)},
		},
		"sibling whose Machine carries the legacy host-id annotation is kept": {
			objs: []client.Object{
				testSpireEntry(testTPMParent, true, old),
				&clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
					Name:        "worker-old",
					Namespace:   "cluster-old",
					Annotations: map[string]string{keys.MachineHostID.Legacy(): testOldHostID},
				}},
			},
			wantKept: []string{machineSpireEntryName(testOldHostID)},
		},
		"unmanaged entry with same parent is kept": {
			objs:     []client.Object{testSpireEntry(testTPMParent, false, old)},
			wantKept: []string{machineSpireEntryName(testOldHostID)},
		},
		"orphan sibling labeled by the pre-import CWE is deleted": {
			objs:        []client.Object{testLegacyCWESpireEntry(testSpireEntry(testTPMParent, false, old))},
			wantRemoved: []string{machineSpireEntryName(testOldHostID)},
		},
		"managed entry with a different parent is untouched": {
			objs:     []client.Object{testSpireEntry(testOtherTPMParent, true, old)},
			wantKept: []string{machineSpireEntryName(testOldHostID)},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "worker-0",
					Namespace: "cluster-test",
					Annotations: map[string]string{
						keys.MachineHostID.String():                 testNewHostID,
						keys.MachineSpireAgentSpiffeIDPath.String(): "/spire/agent/tpm/test-hash",
						keys.MachineAgentConnection.String():        "spiffe",
					},
				},
			}
			controller := newTestSpireEntryController(t, append(test.objs, machine)...)

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: "cluster-test",
				Name:      "worker-0",
			}})
			require.NoError(t, err)

			require.True(t, entryExists(t, controller.k8sClient, machineSpireEntryName(testNewHostID)))
			for _, name := range test.wantRemoved {
				require.False(t, entryExists(t, controller.k8sClient, name), "entry %s should be deleted", name)
			}
			for _, name := range test.wantKept {
				require.True(t, entryExists(t, controller.k8sClient, name), "entry %s should be kept", name)
			}
		})
	}
}

func TestMachineSpireEntryControllerSweepOrphanedEntries(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-2 * orphanEntryGracePeriod)
	fresh := now.Add(-orphanEntryGracePeriod / 2)

	tests := map[string]struct {
		objs           []client.Object
		noLiveMachines bool
		wantRemoved    bool
	}{
		"old entry is kept when no Machine has a host ID": {
			objs:           []client.Object{testSpireEntry(testTPMParent, true, old)},
			noLiveMachines: true,
		},
		"old managed entry without a Machine is deleted": {
			objs:        []client.Object{testSpireEntry(testTPMParent, true, old)},
			wantRemoved: true,
		},
		"entry inside the grace period is kept": {
			objs: []client.Object{testSpireEntry(testTPMParent, true, fresh)},
		},
		"entry with an existing Machine is kept": {
			objs: []client.Object{
				testSpireEntry(testTPMParent, true, old),
				testSpireMachine(false),
			},
		},
		"entry whose Machine is deleting is left to Reconcile": {
			objs: []client.Object{
				testSpireEntry(testTPMParent, true, old),
				testSpireMachine(true),
			},
		},
		"unmanaged entry is kept": {
			objs: []client.Object{testSpireEntry(testTPMParent, false, old)},
		},
		"old entry labeled by the pre-import CWE without a Machine is deleted": {
			objs:        []client.Object{testLegacyCWESpireEntry(testSpireEntry(testTPMParent, false, old))},
			wantRemoved: true,
		},
		"entry labeled by the pre-import CWE with an existing Machine is kept": {
			objs: []client.Object{
				testLegacyCWESpireEntry(testSpireEntry(testTPMParent, false, old)),
				testSpireMachine(false),
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			objs := test.objs
			if !test.noLiveMachines {
				objs = append(objs, testLiveMachine())
			}
			controller := newTestSpireEntryController(t, objs...)

			require.NoError(t, controller.sweepOrphanedEntries(t.Context(), now))

			require.Equal(t, !test.wantRemoved, entryExists(t, controller.k8sClient, machineSpireEntryName(testOldHostID)))
		})
	}
}

func TestMachineSpireEntryControllerSweepContinuesPastDeleteErrors(t *testing.T) {
	t.Parallel()
	old := time.Now().Add(-2 * orphanEntryGracePeriod)
	failing := testSpireEntry(testTPMParent, true, old)
	failing.Name = "host-failing"
	orphan := testSpireEntry(testTPMParent, true, old)
	errDenied := errors.New("denied")
	controller := newTestSpireEntryControllerWithInterceptor(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == failing.Name {
				return errDenied
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, failing, orphan, testLiveMachine())

	err := controller.sweepOrphanedEntries(t.Context(), time.Now())

	require.ErrorIs(t, err, errDenied)
	require.True(t, entryExists(t, controller.k8sClient, failing.Name))
	require.False(t, entryExists(t, controller.k8sClient, orphan.Name))
}

func TestIsManagedHostEntry(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		name   string
		labels map[string]string
		want   bool
	}{
		"current label":                          {name: "host-a", labels: map[string]string{keys.ManagedBy.String(): constants.FieldManagerMachineSpireEntry}, want: true},
		"current value under legacy key":         {name: "host-a", labels: map[string]string{keys.ManagedBy.Legacy(): constants.FieldManagerMachineSpireEntry}, want: true},
		"pre-import CWE host entry":              {name: "host-a", labels: map[string]string{keys.ManagedBy.Legacy(): legacyCWEManagedBy}, want: true},
		"pre-import CWE label on non-host name":  {name: "bootstrap-a", labels: map[string]string{keys.ManagedBy.Legacy(): legacyCWEManagedBy}},
		"pre-import CWE value under current key": {name: "host-a", labels: map[string]string{keys.ManagedBy.String(): legacyCWEManagedBy}},
		"other manager":                          {name: "host-a", labels: map[string]string{keys.ManagedBy.String(): "someone-else"}},
		"no labels":                              {name: "host-a"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := &spirev1alpha1.ClusterStaticEntry{ObjectMeta: metav1.ObjectMeta{Name: test.name, Labels: test.labels}}
			require.Equal(t, test.want, isManagedHostEntry(entry))
		})
	}
}
