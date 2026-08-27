// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestActivitiesApplyCAPIMachineAssignsLeastUsedFailureDomain(t *testing.T) {
	t.Parallel()

	cluster := failureDomainTestDBCluster()
	namespace := capi.ClusterNamespace(cluster.Name)
	controlPlane := true
	existingA := failureDomainTestMachine("cp-existing-a", cluster.Name, namespace, true, "fd-a")
	existingA.Labels[clusterv1.MachineControlPlaneLabel] = "true"
	k8sClient := failureDomainTestClient(t,
		failureDomainTestCAPICluster(cluster.Name, namespace,
			clusterv1.FailureDomain{Name: "fd-c", ControlPlane: &controlPlane},
			clusterv1.FailureDomain{Name: "fd-a", ControlPlane: &controlPlane},
			clusterv1.FailureDomain{Name: "fd-b", ControlPlane: &controlPlane},
		),
		existingA,
		failureDomainTestMachine("cp-existing-b", cluster.Name, namespace, true, "fd-b"),
		failureDomainTestMachine("worker-existing", cluster.Name, namespace, false, "fd-c"),
	)
	activity, tx, database := failureDomainTestActivity(cluster, k8sClient)
	machine := failureDomainTestMachine("cp-new", cluster.Name, namespace, true, "")

	require.NoError(t, activity.applyCAPIMachine(context.Background(), &capiMachineCreateContext{
		node:      db.Node{ID: uuid.New(), ControlPlane: true},
		cluster:   cluster,
		namespace: namespace,
	}, machine))

	applied := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), applied))
	assert.Equal(t, "fd-c", applied.Spec.FailureDomain)
	tx.AssertExpectations(t)
	database.AssertExpectations(t)
}

func TestActivitiesApplyCAPIMachineFiltersIneligibleFailureDomains(t *testing.T) {
	t.Parallel()

	cluster := failureDomainTestDBCluster()
	namespace := capi.ClusterNamespace(cluster.Name)
	eligible := true
	ineligible := false
	k8sClient := failureDomainTestClient(t,
		failureDomainTestCAPICluster(cluster.Name, namespace,
			clusterv1.FailureDomain{Name: "fd-a", ControlPlane: &ineligible},
			clusterv1.FailureDomain{Name: "fd-b"},
			clusterv1.FailureDomain{Name: "fd-c", ControlPlane: &eligible},
		),
	)
	activity, tx, database := failureDomainTestActivity(cluster, k8sClient)
	machine := failureDomainTestMachine("cp-new", cluster.Name, namespace, true, "")

	require.NoError(t, activity.applyCAPIMachine(context.Background(), &capiMachineCreateContext{
		node:      db.Node{ID: uuid.New(), ControlPlane: true},
		cluster:   cluster,
		namespace: namespace,
	}, machine))

	applied := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), applied))
	assert.Equal(t, "fd-c", applied.Spec.FailureDomain)
	tx.AssertExpectations(t)
	database.AssertExpectations(t)
}

func TestActivitiesApplyCAPIMachineLeavesPlacementEmptyWithoutEligibleDomains(t *testing.T) {
	t.Parallel()

	cluster := failureDomainTestDBCluster()
	namespace := capi.ClusterNamespace(cluster.Name)
	ineligible := false
	k8sClient := failureDomainTestClient(t,
		failureDomainTestCAPICluster(cluster.Name, namespace,
			clusterv1.FailureDomain{Name: "fd-a", ControlPlane: &ineligible},
		),
	)
	activity, tx, database := failureDomainTestActivity(cluster, k8sClient)
	machine := failureDomainTestMachine("cp-new", cluster.Name, namespace, true, "")

	require.NoError(t, activity.applyCAPIMachine(context.Background(), &capiMachineCreateContext{
		node:      db.Node{ID: uuid.New(), ControlPlane: true},
		cluster:   cluster,
		namespace: namespace,
	}, machine))

	applied := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), applied))
	assert.Empty(t, applied.Spec.FailureDomain)
	tx.AssertExpectations(t)
	database.AssertExpectations(t)
}

func TestActivitiesApplyCAPIMachinePreservesExistingPlacement(t *testing.T) {
	t.Parallel()

	cluster := failureDomainTestDBCluster()
	namespace := capi.ClusterNamespace(cluster.Name)
	controlPlane := true
	existing := failureDomainTestMachine("cp-existing", cluster.Name, namespace, true, "fd-b")
	k8sClient := failureDomainTestClient(t,
		failureDomainTestCAPICluster(cluster.Name, namespace,
			clusterv1.FailureDomain{Name: "fd-a", ControlPlane: &controlPlane},
			clusterv1.FailureDomain{Name: "fd-b", ControlPlane: &controlPlane},
		),
		existing,
	)
	activity, tx, database := failureDomainTestActivity(cluster, k8sClient)
	retryMachine := existing.DeepCopy()
	retryMachine.Spec.FailureDomain = ""

	require.NoError(t, activity.applyCAPIMachine(context.Background(), &capiMachineCreateContext{
		node:      db.Node{ID: uuid.New(), ControlPlane: true},
		cluster:   cluster,
		namespace: namespace,
	}, retryMachine))

	applied := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(existing), applied))
	assert.Equal(t, "fd-b", applied.Spec.FailureDomain)
	tx.AssertExpectations(t)
	database.AssertExpectations(t)
}

func TestActivitiesApplyCAPIMachineDoesNotSpreadWorkers(t *testing.T) {
	t.Parallel()

	cluster := failureDomainTestDBCluster()
	namespace := capi.ClusterNamespace(cluster.Name)
	controlPlane := true
	k8sClient := failureDomainTestClient(t,
		failureDomainTestCAPICluster(cluster.Name, namespace,
			clusterv1.FailureDomain{Name: "fd-a", ControlPlane: &controlPlane},
		),
	)
	activity := &Activities{k8sClient: k8sClient}
	machine := failureDomainTestMachine("worker-new", cluster.Name, namespace, false, "")

	require.NoError(t, activity.applyCAPIMachine(context.Background(), &capiMachineCreateContext{
		node:      db.Node{ID: uuid.New()},
		cluster:   cluster,
		namespace: namespace,
	}, machine))

	applied := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), applied))
	assert.Empty(t, applied.Spec.FailureDomain)
}

func TestActivitiesApplyCAPIMachineSerializesConcurrentAssignments(t *testing.T) {
	t.Parallel()

	cluster := failureDomainTestDBCluster()
	namespace := capi.ClusterNamespace(cluster.Name)
	controlPlane := true
	k8sClient := failureDomainTestClient(t,
		failureDomainTestCAPICluster(cluster.Name, namespace,
			clusterv1.FailureDomain{Name: "fd-a", ControlPlane: &controlPlane},
			clusterv1.FailureDomain{Name: "fd-b", ControlPlane: &controlPlane},
			clusterv1.FailureDomain{Name: "fd-c", ControlPlane: &controlPlane},
		),
	)

	const machineCount = 6
	tx := &testutil.MockTx{}
	tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Times(machineCount)
	database := &serializingFailureDomainDB{tx: tx}
	selection := &concurrencyProbeSelection{}
	activity := &Activities{
		k8sClient:              k8sClient,
		db:                     database,
		failureDomainSelection: selection,
	}
	createCtx := &capiMachineCreateContext{
		cluster:   cluster,
		namespace: namespace,
	}

	start := make(chan struct{})
	errs := make(chan error, machineCount)
	var workers sync.WaitGroup
	for i := range machineCount {
		machine := failureDomainTestMachine(
			fmt.Sprintf("cp-concurrent-%d", i),
			cluster.Name,
			namespace,
			true,
			"",
		)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errs <- activity.applyCAPIMachine(context.Background(), createCtx, machine)
		}()
	}

	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	machines := &clusterv1.MachineList{}
	require.NoError(t, k8sClient.List(
		context.Background(),
		machines,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	))
	require.Len(t, machines.Items, machineCount)

	counts := map[string]int{}
	for _, machine := range machines.Items {
		counts[machine.Spec.FailureDomain]++
	}
	assert.Equal(t, map[string]int{"fd-a": 2, "fd-b": 2, "fd-c": 2}, counts)
	assert.Equal(t, 1, selection.maxConcurrent())
	tx.AssertExpectations(t)
}

type serializingFailureDomainDB struct {
	db.DB

	tx db.Tx
	mu sync.Mutex
}

func (d *serializingFailureDomainDB) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(ctx, d.tx)
}

type concurrencyProbeSelection struct {
	mu            sync.Mutex
	active        int
	maxActiveSeen int
}

func (s *concurrencyProbeSelection) Select(eligibleDomains, assignments []string) (string, error) {
	s.mu.Lock()
	s.active++
	s.maxActiveSeen = max(s.maxActiveSeen, s.active)
	s.mu.Unlock()

	time.Sleep(time.Millisecond)
	selected, err := (capi.LeastUsedFailureDomainSelection{}).Select(eligibleDomains, assignments)

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return selected, err
}

func (s *concurrencyProbeSelection) maxConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxActiveSeen
}

func failureDomainTestActivity(cluster db.Cluster, k8sClient client.Client) (*Activities, *testutil.MockTx, *testutil.MockDB) {
	tx := &testutil.MockTx{}
	tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	database := &testutil.MockDB{GetTx: func() db.Tx { return tx }}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	return &Activities{k8sClient: k8sClient, db: database}, tx, database
}

func failureDomainTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

func failureDomainTestDBCluster() db.Cluster {
	return db.Cluster{
		ID:   uuid.New(),
		Name: "cluster-a",
	}
}

func failureDomainTestCAPICluster(name, namespace string, domains ...clusterv1.FailureDomain) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: clusterv1.ClusterStatus{
			FailureDomains: domains,
		},
	}
}

func failureDomainTestMachine(name, clusterName, namespace string, controlPlane bool, failureDomain string) *clusterv1.Machine {
	labels := map[string]string{clusterv1.ClusterNameLabel: clusterName}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       machineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:   clusterName,
			FailureDomain: failureDomain,
		},
	}
}
