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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestMachineSpireEntryControllerReconcileCreatesClusterStaticEntry(t *testing.T) {
	t.Parallel()
	const nodeID = "45f59791-87df-4e85-9430-e565166ad020"

	tests := map[string]struct {
		tlsJWT       bool
		wantSpiffeID string
	}{
		"legacy SPIFFE node keeps host path": {
			wantSpiffeID: "spiffe://example.org/host/" + nodeID,
		},
		"TLS JWT node uses canonical DSX exchange path": {
			tlsJWT:       true,
			wantSpiffeID: "spiffe://example.org/tenant/nke/service/node-agent/bm/" + nodeID,
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
					},
				},
			}
			nodes := &machineSpireEntryNodeReaderStub{node: db.Node{
				ID:                     uuid.MustParse(nodeID),
				NodeAgentTLSJWTEnabled: test.tlsJWT,
			}}
			controller := &MachineSpireEntryController{
				cfg: config.Config{
					SpireClassName: "spire",
					NodeAgent: config.NodeAgentConfig{TLSJWT: config.TLSJWTConfig{
						SubjectTenant:  "nke",
						SubjectService: "node-agent",
					}},
				},
				k8sClient:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build(),
				nodes:       nodes,
				logger:      slog.New(slog.DiscardHandler),
				trustDomain: "example.org",
			}

			_, err := controller.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: "cluster-test",
				Name:      "worker-0",
			}})
			require.NoError(t, err)
			require.Equal(t, 1, nodes.calls)
			require.Equal(t, uuid.MustParse(nodeID), nodes.gotID)

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

type machineSpireEntryNodeReaderStub struct {
	node  db.Node
	err   error
	calls int
	gotID uuid.UUID
}

func (s *machineSpireEntryNodeReaderStub) GetNode(_ context.Context, id uuid.UUID) (db.Node, error) {
	s.calls++
	s.gotID = id
	return s.node, s.err
}
