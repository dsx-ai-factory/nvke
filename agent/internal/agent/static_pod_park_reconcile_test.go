// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parkManifestIn moves a manifest out of the directory kubelet watches and into
// parkDir, which is what decides who owes the restore.
func parkManifestIn(t *testing.T, s *NodeAgentServer, parkDir, manifestName string) {
	t.Helper()
	src := filepath.Join(s.RootPath, defaultStaticPodManifestDir, manifestName)
	dst := filepath.Join(s.RootPath, parkDir, manifestName)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	require.NoError(t, os.Rename(src, dst))
}

// parkManifest puts a manifest where an interrupted defragmentation would have
// left it, which is the park the reconciler owns.
func parkManifest(t *testing.T, s *NodeAgentServer, manifestName string) {
	t.Helper()
	parkManifestIn(t, s, defaultEtcdDefragParkDir, manifestName)
}

// manifestIsParkedIn reports whether a manifest is sitting in parkDir.
func manifestIsParkedIn(t *testing.T, s *NodeAgentServer, parkDir, manifestName string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(s.RootPath, parkDir, manifestName))
	return err == nil
}

func runningEtcdDefragRecord() map[string]NodeAgentOperation {
	return map[string]NodeAgentOperation{
		"defrag-1": {
			ID:    "defrag-1",
			Type:  operationTypeEtcdDefrag,
			Phase: NodeAgentOperationPhaseRunning,
			Step:  stepEtcdDefragWaitingRestart,
		},
	}
}

// The case this exists for: the agent dies between taking the node out of
// service and putting it back. Nothing else will restore those manifests -
// kubelet cannot see them, and the caller that asked for the defragmentation
// has no RPC that undoes a half-finished one - so the agent has to do it before
// it starts serving again.
func TestReconcileInterruptedStaticPodParksRestoresAnInterruptedDefrag(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	parkManifest(t, s, defaultKubeAPIServerManifestName)
	parkManifest(t, s, defaultKubeVipManifestName)

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName))
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName))
}

// The trap this exists for: a parked manifest is not on its own evidence that a
// restore is owed. Graceful shutdown parks kube-vip deliberately and expects it
// to stay parked until the caller restores it, which is how a node is held out
// of service for recycling. A reconciler that restored whatever it found parked
// would put the node straight back into the VIP underneath whoever was
// recycling it.
//
// The park directory is what keeps them apart, so the operation phase is
// irrelevant here - the reconciler never reads it, and never looks in the
// shared directory at all.
func TestReconcileInterruptedStaticPodParksLeavesADeliberateParkAlone(t *testing.T) {
	tests := []struct {
		name  string
		phase NodeAgentOperationPhase
	}{
		{name: "a completed graceful shutdown", phase: NodeAgentOperationPhaseSucceeded},
		// Non-terminal too: the restore is a separate RPC the caller owns, so
		// even an interrupted shutdown is not this reconciler's to undo.
		{name: "an interrupted graceful shutdown", phase: NodeAgentOperationPhaseRunning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
			s.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{
				"shutdown-1": {
					ID:    "shutdown-1",
					Type:  operationTypeK8sControlPlaneNodeGracefulShutdown,
					Phase: tt.phase,
				},
			}
			parkManifestIn(t, s, defaultStaticPodBackupDir, defaultKubeVipManifestName)

			require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

			assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
				"a park this reconciler does not own must survive the restart")
			assert.True(t, manifestIsParkedIn(t, s, defaultStaticPodBackupDir, defaultKubeVipManifestName),
				"and must be left exactly where the caller will look for it")
		})
	}
}

// A defragmentation that gave up on the member deliberately leaves this node out
// of service, and says so by handing its manifests to the shared directory. The
// reconciler must not overrule that - which it cannot, because it only ever
// looks in its own.
func TestReconcileInterruptedStaticPodParksLeavesAHandedOffParkAlone(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{
		"defrag-1": {
			ID:    "defrag-1",
			Type:  operationTypeEtcdDefrag,
			Phase: NodeAgentOperationPhaseFailed,
		},
	}
	parkManifest(t, s, defaultKubeAPIServerManifestName)
	parkManifest(t, s, defaultKubeVipManifestName)
	require.NoError(t, s.handOffParkedManifestsToCaller())

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.False(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName))
	assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName))
	assert.True(t, manifestIsParkedIn(t, s, defaultStaticPodBackupDir, defaultKubeVipManifestName))
}

// The counterpart: a defragmentation that failed trying to put the pods back
// leaves them in its own directory on purpose, because retrying is exactly the
// right thing. An apiserver that was slow to start once should come back on the
// next boot rather than needing a human.
func TestReconcileInterruptedStaticPodParksRetriesAFailedReturnToService(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{
		"defrag-1": {
			ID:    "defrag-1",
			Type:  operationTypeEtcdDefrag,
			Phase: NodeAgentOperationPhaseFailed,
		},
	}
	parkManifest(t, s, defaultKubeAPIServerManifestName)
	parkManifest(t, s, defaultKubeVipManifestName)

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName))
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName))
}

// A caller polling GetEtcdDefragStatus for an attempt that died with the agent
// would otherwise wait on a record that never moves, so the record is settled -
// failed rather than deleted, so the poll gets an answer rather than a
// not-found.
//
// Settling is only ever bookkeeping now. An earlier version let the record
// decide what was restored, which made a stale one dangerous: a reboot during
// node recycling would find it next to a deliberately parked kube-vip and put
// the node back into the VIP underneath the recycle. The park directory answers
// that question now, so the record cannot reach a manifest at all.
func TestReconcileInterruptedStaticPodParksSettlesTheRecordItActedOn(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.Persister = &MockPersister{}
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	parkManifest(t, s, defaultKubeVipManifestName)

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))
	require.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName), "precondition: the first pass restores")

	assert.Equal(t, NodeAgentOperationPhaseFailed, s.NodeStatus.NodeAgentOperations["defrag-1"].Phase,
		"the record must be terminal, and failed rather than deleted so a poll still gets an answer")
}

// The hole chaochn47 found: the records are settled before anything is
// restored, and settling is durable. An agent that dies in the window between
// the two - which spans the per-pod start waits, so up to minutes, and is
// reached only by an agent that has already crashed once - comes back to a
// terminal record, no interrupted operation, and an early return. The parked
// manifests are then stranded for good: kubelet cannot see them, no RPC undoes
// a half-finished defragmentation, and the one mechanism that was supposed to
// catch it has already spent its own evidence.
func TestReconcileInterruptedStaticPodParksSurvivesACrashBetweenSettlingAndRestoring(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.Persister = &MockPersister{}
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	parkManifest(t, s, defaultKubeAPIServerManifestName)
	parkManifest(t, s, defaultKubeVipManifestName)

	// The crash: the records have been settled, nothing has been put back.
	interrupted := s.interruptedStaticPodParkingOperations()
	require.Len(t, interrupted, 1, "precondition: the defrag record is interrupted")
	require.NoError(t, s.failInterruptedStaticPodParkingOperations(context.Background(), interrupted))
	require.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
		"precondition: nothing restored yet")

	// The next startup has to finish the job.
	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName),
		"kube-apiserver must still be restored after a crash mid-recovery")
	assert.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
		"kube-vip must still be restored after a crash mid-recovery")
}

// An operation with a live worker is still running, not interrupted, and
// marking it failed would report a defragmentation still in progress as over.
// The reconciler only ever runs before the agent serves, so this cannot happen
// in production; the condition is pinned because nothing else would catch it
// if a later caller ran the reconcile with a worker up.
//
// The restore is not gated on this. Whether the manifest goes back is decided
// by the park directory, and a worker still holding staticPodMutationMu is what
// keeps the two from interleaving.
func TestReconcileInterruptedStaticPodParksLeavesAnActiveOperationRunning(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.Persister = &MockPersister{}
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	s.activeOperations = map[string]struct{}{"defrag-1": {}}
	parkManifest(t, s, defaultKubeVipManifestName)

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.Equal(t, NodeAgentOperationPhaseRunning, s.NodeStatus.NodeAgentOperations["defrag-1"].Phase,
		"an operation with a live worker must not be settled out from under it")
}

// Nothing parked is the overwhelmingly common case: a clean agent restart. It
// must not fail just because the backup directory was never created.
func TestReconcileInterruptedStaticPodParksIsANoOpWithNothingParked(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	require.NoError(t, os.RemoveAll(filepath.Join(s.RootPath, defaultStaticPodBackupDir)))

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assertNodeInService(t, s)
}

// Restoring a timestamped forensic snapshot would overwrite a live manifest
// with a stale copy. Three independent things prevent it, and this is a guard
// on all three rather than a regression test for any one of them - breaking a
// single one does not reach the live manifest. The snapshots are written to the
// shared backup directory, which this reconciler never reads;
// parkedStaticPodManifests stats an exact fixed name; and the restore resolves
// source and destination from that same fixed name and stops when the
// destination is already active.
func TestReconcileInterruptedStaticPodParksIgnoresTimestampedSnapshots(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()

	snapshot := filepath.Join(s.RootPath, defaultStaticPodBackupDir, defaultKubeVipManifestName+".20260101T000000Z")
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshot), 0o700))
	require.NoError(t, os.WriteFile(snapshot, []byte("stale: true\n"), 0o600))

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	active, err := os.ReadFile(filepath.Join(s.RootPath, defaultStaticPodManifestDir, defaultKubeVipManifestName))
	require.NoError(t, err)
	assert.Equal(t, testKubeVipManifest, string(active), "the live manifest must be untouched")
}
