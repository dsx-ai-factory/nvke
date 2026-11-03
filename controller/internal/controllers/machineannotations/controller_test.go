// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package machineannotations

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

const (
	testNamespace = "nvke-system"
	testNodeID    = "45f59791-87df-4e85-9430-e565166ad020"
)

func TestReconcileSetsControllerOwnedAnnotations(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		node        db.Node
		annotations map[string]string
		want        map[keys.Key]string
	}{
		"missing on a SPIFFE node": {
			node:        db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			annotations: map[string]string{keys.MachineHostID.String(): testNodeID},
			want: map[keys.Key]string{
				keys.MachineAgentConnection:        "spiffe",
				keys.MachineSpireAttestationMethod: "tpm",
				keys.MachineSpiffeEnabled:          "true",
			},
		},
		"missing on a TLS-JWT dsxImds node": {
			node:        db.Node{SpiffeEnabled: true, NodeAgentTLSJWTEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS},
			annotations: map[string]string{keys.MachineHostID.String(): testNodeID},
			want: map[keys.Key]string{
				keys.MachineAgentConnection:        "spiffe-jwt",
				keys.MachineSpireAttestationMethod: "dsxImds",
				keys.MachineSpiffeEnabled:          "true",
			},
		},
		"only the attestation annotations are missing": {
			node: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			annotations: map[string]string{
				keys.MachineHostID.String():          testNodeID,
				keys.MachineAgentConnection.String(): "spiffe",
			},
			want: map[keys.Key]string{
				keys.MachineAgentConnection:        "spiffe",
				keys.MachineSpireAttestationMethod: "tpm",
				keys.MachineSpiffeEnabled:          "true",
			},
		},
		"stale values are corrected": {
			node: db.Node{NodeAgentExternalMTLSEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			annotations: map[string]string{
				keys.MachineHostID.String():                 testNodeID,
				keys.MachineAgentConnection.String():        "plaintext",
				keys.MachineSpireAttestationMethod.String(): "dsxImds",
				keys.MachineSpiffeEnabled.String():          "true",
			},
			want: map[keys.Key]string{
				keys.MachineAgentConnection:        "external-mtls",
				keys.MachineSpireAttestationMethod: "tpm",
				keys.MachineSpiffeEnabled:          "false",
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test.node.ID = uuid.MustParse(testNodeID)
			test.node.Name = "worker-0"
			test.annotations["unrelated.example.com/keep"] = "yes"
			controller := newTestController(t, test.node, newMachine(test.annotations))

			result, err := controller.Reconcile(t.Context(), dbreconcile.Key{ID: test.node.ID})
			require.NoError(t, err)
			require.Equal(t, reconcile.Result{}, result)

			machine := getMachine(t, controller.k8sClient)
			for key, want := range test.want {
				require.Equal(t, want, key.Value(machine.Annotations), key.String())
			}
			require.Equal(t, testNodeID, keys.MachineHostID.Value(machine.Annotations))
			require.Equal(t, "yes", machine.Annotations["unrelated.example.com/keep"])
		})
	}
}

func TestReconcileLeavesMachineUnchanged(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		node        db.Node
		annotations map[string]string
	}{
		"values already current": {
			node: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			annotations: map[string]string{
				keys.MachineHostID.String():                 testNodeID,
				keys.MachineAgentConnection.String():        "spiffe",
				keys.MachineSpireAttestationMethod.String(): "tpm",
				keys.MachineSpiffeEnabled.String():          "true",
			},
		},
		"Machine without host-id predates the controller's Machine identity": {
			node:        db.Node{SpiffeEnabled: true},
			annotations: map[string]string{},
		},
		"Machine host-id names a different node": {
			node:        db.Node{SpiffeEnabled: true},
			annotations: map[string]string{keys.MachineHostID.String(): uuid.NewString()},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test.node.ID = uuid.MustParse(testNodeID)
			test.node.Name = "worker-0"
			controller := newTestController(t, test.node, newMachine(test.annotations))
			before := getMachine(t, controller.k8sClient)

			_, err := controller.Reconcile(t.Context(), dbreconcile.Key{ID: test.node.ID})
			require.NoError(t, err)

			after := getMachine(t, controller.k8sClient)
			require.Equal(t, before.ResourceVersion, after.ResourceVersion)
			require.Equal(t, before.Annotations, after.Annotations)
		})
	}
}

func TestReconcileIgnoresMissingObjects(t *testing.T) {
	t.Parallel()

	t.Run("node not found", func(t *testing.T) {
		t.Parallel()
		controller := newTestController(t, db.Node{})
		controller.nodes = nodeReaderStub{err: db.ErrNotFound}

		_, err := controller.Reconcile(t.Context(), dbreconcile.Key{ID: uuid.MustParse(testNodeID)})
		require.NoError(t, err)
	})

	t.Run("Machine not found", func(t *testing.T) {
		t.Parallel()
		controller := newTestController(t, db.Node{ID: uuid.MustParse(testNodeID), Name: "worker-0"})

		_, err := controller.Reconcile(t.Context(), dbreconcile.Key{ID: uuid.MustParse(testNodeID)})
		require.NoError(t, err)

		err = controller.k8sClient.Get(t.Context(), types.NamespacedName{Namespace: testNamespace, Name: "worker-0"}, &clusterv1.Machine{})
		require.True(t, apierrors.IsNotFound(err))
	})
}

func newTestController(t *testing.T, node db.Node, objects ...client.Object) *Controller {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	return &Controller{
		cfg:       config.Config{ManagementNamespace: testNamespace},
		nodes:     nodeReaderStub{node: node},
		k8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		logger:    slog.New(slog.DiscardHandler),
	}
}

func newMachine(annotations map[string]string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-0",
			Namespace:   testNamespace,
			Annotations: annotations,
		},
	}
}

func getMachine(t *testing.T, k8sClient client.Client) *clusterv1.Machine {
	t.Helper()
	machine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(t.Context(), types.NamespacedName{Namespace: testNamespace, Name: "worker-0"}, machine))
	return machine
}

type nodeReaderStub struct {
	node db.Node
	err  error
}

func (s nodeReaderStub) GetNode(context.Context, uuid.UUID) (db.Node, error) {
	return s.node, s.err
}
