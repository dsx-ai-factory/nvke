// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const fdTestNamespace = "nke-system"

type failureDomainStore struct {
	db.DB
	nodes map[uuid.UUID]db.Node
}

func (s *failureDomainStore) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return fn(ctx, s)
}

func (s *failureDomainStore) GetNodeForUpdate(_ context.Context, id uuid.UUID) (db.Node, error) {
	return s.nodes[id], nil
}

func (s *failureDomainStore) UpdateNode(_ context.Context, node *db.Node) (db.Node, error) {
	s.nodes[node.ID] = *node
	return *node, nil
}

func (s *failureDomainStore) ListNodes(_ context.Context, filters db.NodeFilters) ([]db.Node, error) {
	var out []db.Node
	for _, node := range s.nodes {
		if node.ClusterID == *filters.ClusterID && node.ControlPlane == *filters.ControlPlane {
			out = append(out, node)
		}
	}
	return out, nil
}

func TestRotateFailureDomain(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	pastGrace := time.Now().Add(-time.Hour)
	withinGrace := time.Now().Add(-time.Minute)

	tests := []struct {
		name        string
		disabled    bool
		worker      bool
		domains     []string
		node        db.Node
		machineFD   string
		others      map[string]string
		unavailable bool
		wantMoved   bool
		wantDomain  string
		wantTried   []string
		wantSince   bool
		wantMachine string
	}{
		{
			name:        "disabled leaves the node alone",
			disabled:    true,
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace},
			unavailable: true,
			wantDomain:  "fd-a",
			wantSince:   true,
		},
		{
			name:       "a cleared reason clears the timer",
			node:       db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace},
			wantDomain: "fd-a",
		},
		{
			name:        "the first report starts the timer",
			node:        db.Node{FailureDomain: "fd-a"},
			unavailable: true,
			wantDomain:  "fd-a",
			wantSince:   true,
		},
		{
			name:        "a report within the grace period waits",
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &withinGrace},
			unavailable: true,
			wantDomain:  "fd-a",
			wantSince:   true,
		},
		{
			name:        "past the grace period the node moves to the least-used domain",
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace},
			others:      map[string]string{"cp-1": "fd-b"},
			unavailable: true,
			wantMoved:   true,
			wantDomain:  "fd-c",
			wantTried:   []string{"fd-a"},
		},
		{
			name:        "a tried domain is skipped even when least used",
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace, FailureDomainsTried: []string{"fd-c"}},
			others:      map[string]string{"cp-1": "fd-b"},
			unavailable: true,
			wantMoved:   true,
			wantDomain:  "fd-b",
			wantTried:   []string{"fd-c", "fd-a"},
		},
		{
			name:        "the walk starts over once every domain was tried",
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace, FailureDomainsTried: []string{"fd-b", "fd-c"}},
			others:      map[string]string{"cp-1": "fd-b"},
			unavailable: true,
			wantMoved:   true,
			wantDomain:  "fd-c",
			wantTried:   []string{"fd-a"},
		},
		{
			name:        "a single domain leaves the node waiting",
			domains:     []string{"fd-a"},
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace},
			unavailable: true,
			wantDomain:  "fd-a",
			wantSince:   true,
		},
		{
			name:        "a Machine moved before the row update finishes the row",
			node:        db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &pastGrace},
			machineFD:   "fd-b",
			unavailable: true,
			wantDomain:  "fd-b",
			wantTried:   []string{"fd-a"},
			wantMachine: "fd-b",
		},
		{
			name:       "a worker node is ignored",
			worker:     true,
			node:       db.Node{FailureDomain: "fd-a"},
			wantDomain: "fd-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			node := tt.node
			node.ID, node.ClusterID, node.Name = uuid.New(), clusterID, "cp-0"
			node.ControlPlane = !tt.worker
			store := &failureDomainStore{nodes: map[uuid.UUID]db.Node{node.ID: node}}
			for name, domain := range tt.others {
				other := db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, Name: name, FailureDomain: domain}
				store.nodes[other.ID] = other
			}

			machineFD := tt.machineFD
			if machineFD == "" {
				machineFD = node.FailureDomain
			}
			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: fdTestNamespace, Labels: map[string]string{
					clusterv1.ClusterNameLabel: "cluster-a", clusterv1.MachineControlPlaneLabel: "",
				}},
				Spec: clusterv1.MachineSpec{ClusterName: "cluster-a", FailureDomain: machineFD},
			}
			domains := tt.domains
			if domains == nil {
				domains = []string{"fd-a", "fd-b", "fd-c"}
			}
			k8sClient := failureDomainTestClient(t, domains, machine)

			a := &Activities{db: store, k8sClient: k8sClient, cfg: config.Config{
				ManagementNamespace:                          fdTestNamespace,
				ControlPlaneFailureDomainFallbackEnabled:     !tt.disabled,
				ControlPlaneFailureDomainFallbackGracePeriod: 10 * time.Minute,
			}}
			logger := log.NewStructuredLogger(slog.New(slog.DiscardHandler))

			moved, err := a.rotateFailureDomain(context.Background(), logger, db.Cluster{ID: clusterID, Name: "cluster-a"}, node, machine, tt.unavailable)
			require.NoError(t, err)
			assert.Equal(t, tt.wantMoved, moved)

			got := store.nodes[node.ID]
			assert.Equal(t, tt.wantDomain, got.FailureDomain)
			assert.Equal(t, tt.wantTried, got.FailureDomainsTried)
			assert.Equal(t, tt.wantSince, got.FailureDomainUnavailableSince != nil)

			wantMachine := tt.wantMachine
			if wantMachine == "" {
				wantMachine = tt.wantDomain
			}
			stored := &clusterv1.Machine{}
			require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), stored))
			assert.Equal(t, wantMachine, stored.Spec.FailureDomain)
		})
	}
}

func failureDomainTestClient(t *testing.T, domains []string, machine *clusterv1.Machine) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	capiCluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: fdTestNamespace}}
	capiCluster.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
	for _, domain := range domains {
		capiCluster.Status.FailureDomains = append(capiCluster.Status.FailureDomains, clusterv1.FailureDomain{Name: domain, ControlPlane: ptr.To(true)})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(capiCluster, machine).WithStatusSubresource(capiCluster).Build()
}

func TestFailureDomainWaitNote(t *testing.T) {
	t.Parallel()
	since := time.Now()
	tests := []struct {
		name string
		node db.Node
		want string
	}{
		{name: "not waiting", node: db.Node{FailureDomain: "fd-a"}, want: ""},
		{name: "waiting", node: db.Node{FailureDomain: "fd-a", FailureDomainUnavailableSince: &since}, want: `waiting on failure domain "fd-a"`},
		{name: "moved", node: db.Node{FailureDomain: "fd-b", FailureDomainsTried: []string{"fd-a", "fd-c"}}, want: `waiting on failure domain "fd-b"; tried fd-a, fd-c`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, failureDomainWaitNote(tt.node))
		})
	}
}
