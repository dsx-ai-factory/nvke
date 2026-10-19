// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	launchLayerSpiffeTestHostID = "7b0e3c7e-5a43-4f43-9b0a-0d7b1c6f2a11"
	launchLayerSpiffeTestPath   = "/spire/agent/dsx_imds/tenant/org-1/bm/instance-123"
)

// The path itself is the LaunchLayer client's (see its SpireAgentPath tests);
// these cases cover the controller's wiring around it.
func TestUnit_LaunchLayerSpiffeControllerReconcile(t *testing.T) {
	t.Parallel()

	type parameters struct {
		method        db.SpireAttestationMethod
		instanceID    string
		existingPath  string
		ownerRef      bool
		deleting      bool
		missingHostID bool
		invalidHostID bool
		dbErr         error
		wantErr       string
		wantRequeue   bool
		wantPath      string
	}

	tests := map[string]parameters{
		"writes the path the provider client resolves": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123", ownerRef: true,
			wantPath: launchLayerSpiffeTestPath,
		},
		"writes nothing when the provider client supplies no path": {
			method: db.SpireAttestationMethodTPM, instanceID: "instance-123", ownerRef: true,
		},
		"requeues while the path is pending": {
			method: db.SpireAttestationMethodDSXIMDS, ownerRef: true, wantRequeue: true,
		},
		"keeps a path already on the Machine": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123", ownerRef: true,
			existingPath: constants.JoinTokenAlternateSpireAgentPath + launchLayerSpiffeTestHostID,
			wantPath:     constants.JoinTokenAlternateSpireAgentPath + launchLayerSpiffeTestHostID,
		},
		"waits for a Machine owner reference": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123",
		},
		"skips a deleting LaunchLayerMachine": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123", ownerRef: true, deleting: true,
		},
		"skips a Machine without a host ID": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123", ownerRef: true, missingHostID: true,
		},
		"rejects an invalid host ID": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123", ownerRef: true, invalidHostID: true,
			wantErr: "invalid host ID",
		},
		"retries when the node is absent from the database": {
			method: db.SpireAttestationMethodDSXIMDS, instanceID: "instance-123", ownerRef: true, dbErr: db.ErrNotFound,
			wantErr: "get node " + launchLayerSpiffeTestHostID,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machine := newMachineForLaunchLayerSpiffeTest(params.existingPath)
			if params.missingHostID {
				keys.MachineHostID.Delete(machine.Annotations)
			}
			if params.invalidHostID {
				machine.Annotations[keys.MachineHostID.String()] = "not-a-uuid"
			}
			launchLayerMachine := newLaunchLayerMachineForSpiffeTest(params.instanceID, params.ownerRef, params.deleting)
			reader := &launchLayerSpiffeReaderStub{node: db.Node{SpiffeEnabled: true, SpireAttestationMethod: params.method}, err: params.dbErr}
			controller := newLaunchLayerSpiffeControllerForTest(t, reader, machine, launchLayerMachine)

			result, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(launchLayerMachine)})
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
			} else {
				require.NoError(t, err)
			}
			if params.wantRequeue {
				require.Equal(t, launchLayerSpireAgentPathRequeue, result.RequeueAfter)
			} else {
				require.Zero(t, result.RequeueAfter)
			}

			gotMachine := &clusterv1.Machine{}
			require.NoError(t, controller.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(machine), gotMachine))
			if params.wantPath == "" {
				require.False(t, keys.MachineSpireAgentSpiffeIDPath.Has(gotMachine.Annotations))
				return
			}
			require.Equal(t, params.wantPath, keys.MachineSpireAgentSpiffeIDPath.Value(gotMachine.Annotations))
		})
	}
}

func TestUnit_LaunchLayerSpiffeControllerReconcileIgnoresMissingLaunchLayerMachine(t *testing.T) {
	t.Parallel()
	controller := newLaunchLayerSpiffeControllerForTest(t, &launchLayerSpiffeReaderStub{})

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "cluster-test",
		Name:      "worker-gone",
	}})

	require.NoError(t, err)
}

func TestUnit_LaunchLayerSpiffeControllerMachineWatchMapping(t *testing.T) {
	t.Parallel()
	type parameters struct {
		apiGroup string
		kind     string
		name     string
		other    bool
		wantName string
	}
	tests := map[string]parameters{
		"routes a Machine to its LaunchLayerMachine infrastructure reference": {
			apiGroup: launchLayerAPIGroup, kind: launchLayerMachineKind, name: "ll-infra-1", wantName: "ll-infra-1",
		},
		"ignores another infrastructure kind":   {apiGroup: launchLayerAPIGroup, kind: "NicoMachine", name: "nico-infra-1"},
		"ignores another API group":             {apiGroup: "example.org", kind: launchLayerMachineKind, name: "ll-infra-1"},
		"ignores a missing infrastructure name": {apiGroup: launchLayerAPIGroup, kind: launchLayerMachineKind},
		"ignores a non-Machine object":          {other: true},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var obj client.Object = newMachineForLaunchLayerSpiffeTest("")
			if params.other {
				obj = newLaunchLayerMachineForSpiffeTest("", false, false)
			} else {
				obj.(*clusterv1.Machine).Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
					APIGroup: params.apiGroup,
					Kind:     params.kind,
					Name:     params.name,
				}
			}
			requests := launchLayerMachineRequestsForMachine(t.Context(), obj)
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

// A SPIFFE-enabled LaunchLayer Machine gets the ClusterStaticEntry its agent
// attests against: this controller writes the agent ID path, and
// MachineSpireEntryController turns it into the entry's parent ID.
func TestUnit_LaunchLayerSpiffeControllerMachineGetsClusterStaticEntry(t *testing.T) {
	t.Parallel()
	machine := newMachineForLaunchLayerSpiffeTest("")
	launchLayerMachine := newLaunchLayerMachineForSpiffeTest("instance-123", true, false)
	reader := &launchLayerSpiffeReaderStub{node: db.Node{
		ID:                     uuid.MustParse(launchLayerSpiffeTestHostID),
		SpiffeEnabled:          true,
		SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS,
	}}
	controller := newLaunchLayerSpiffeControllerForTest(t, reader, machine, launchLayerMachine)
	entryController := &MachineSpireEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		k8sClient:   controller.k8sClient,
		nodes:       reader,
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(launchLayerMachine)})
	require.NoError(t, err)
	_, err = entryController.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	require.NoError(t, err)

	cse := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, controller.k8sClient.Get(t.Context(), types.NamespacedName{
		Name: machineSpireEntryName(launchLayerSpiffeTestHostID),
	}, cse))
	require.Equal(t, "spiffe://example.org"+launchLayerSpiffeTestPath, cse.Spec.ParentID)
	require.Equal(t, "spiffe://example.org/host/"+launchLayerSpiffeTestHostID, cse.Spec.SPIFFEID)
}

func newMachineForLaunchLayerSpiffeTest(existingPath string) *clusterv1.Machine {
	annotations := map[string]string{keys.MachineHostID.String(): launchLayerSpiffeTestHostID}
	if existingPath != "" {
		annotations[keys.MachineSpireAgentSpiffeIDPath.String()] = existingPath
	}
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-0",
			Namespace:   "cluster-test",
			Labels:      map[string]string{clusterv1.ClusterNameLabel: "tenant"},
			Annotations: annotations,
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: "tenant",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: launchLayerAPIGroup, Kind: launchLayerMachineKind, Name: "worker-0",
			},
		},
	}
}

func newLaunchLayerMachineForSpiffeTest(instanceID string, ownerRef, deleting bool) *unstructured.Unstructured {
	launchLayerMachine := newLaunchLayerMachine()
	launchLayerMachine.SetNamespace("cluster-test")
	launchLayerMachine.SetName("worker-0")
	if instanceID != "" {
		launchLayerMachine.Object["status"] = map[string]any{"instanceID": instanceID}
	}
	if ownerRef {
		isController := true
		launchLayerMachine.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       "worker-0",
			Controller: &isController,
		}})
	}
	if deleting {
		now := metav1.Now()
		launchLayerMachine.SetDeletionTimestamp(&now)
		launchLayerMachine.SetFinalizers([]string{"test.finalizer"})
	}
	return launchLayerMachine
}

// newLaunchLayerSpiffeControllerForTest seeds the CAPI Cluster and the
// LaunchLayerCluster (organization org-1) the provider client resolves the
// organization through.
func newLaunchLayerSpiffeControllerForTest(t *testing.T, reader launchLayerSpiffeReader, objects ...client.Object) *LaunchLayerSpiffeController {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	launchLayerCluster := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"vpc": map[string]any{"orgID": "org-1"}}}}
	launchLayerCluster.SetAPIVersion(launchLayerAPIGroup + "/" + launchLayerAPIVersion)
	launchLayerCluster.SetKind("LaunchLayerCluster")
	launchLayerCluster.SetNamespace("cluster-test")
	launchLayerCluster.SetName("tenant-infra")
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "cluster-test"},
		Spec: clusterv1.ClusterSpec{InfrastructureRef: clusterv1.ContractVersionedObjectReference{
			APIGroup: launchLayerAPIGroup, Kind: "LaunchLayerCluster", Name: "tenant-infra",
		}},
	}

	return &LaunchLayerSpiffeController{
		db:        reader,
		k8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, cluster, launchLayerCluster)...).Build(),
		logger:    slog.New(slog.DiscardHandler),
	}
}

type launchLayerSpiffeReaderStub struct {
	node db.Node
	err  error
}

func (s *launchLayerSpiffeReaderStub) GetNode(context.Context, uuid.UUID) (db.Node, error) {
	return s.node, s.err
}

func (s *launchLayerSpiffeReaderStub) GetCluster(_ context.Context, id uuid.UUID) (db.Cluster, error) {
	return db.Cluster{ID: id, ProviderType: db.ProviderTypeLaunchLayer}, nil
}
