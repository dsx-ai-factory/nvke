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

// parkManifest puts a manifest where an interrupted operation would have left
// it: in the backup directory, out of the one kubelet watches.
func parkManifest(t *testing.T, s *NodeAgentServer, manifestName string) {
	t.Helper()
	src := filepath.Join(s.RootPath, defaultStaticPodManifestDir, manifestName)
	dst := filepath.Join(s.RootPath, defaultStaticPodBackupDir, manifestName)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	require.NoError(t, os.Rename(src, dst))
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
// of service for recycling. A reconciler keyed on the file rather than on the
// operation would put the node straight back into the VIP underneath whoever
// was recycling it.
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
			parkManifest(t, s, defaultKubeVipManifestName)

			require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

			assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
				"a park this reconciler does not own must survive the restart")
		})
	}
}

// A defragmentation that reached a terminal phase made its own decision about
// what to leave parked - the member that never came back healthy is left out of
// service on purpose - and the reconciler must not overrule it.
func TestReconcileInterruptedStaticPodParksLeavesATerminalDefragAlone(t *testing.T) {
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

	assert.False(t, staticPodManifestIsActive(t, s, defaultKubeAPIServerManifestName))
	assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName))
}

// The bug this exists for: the reconciler left the interrupted record
// non-terminal, so it stayed a standing instruction to restore. Recycling a
// node reboots it, and a reboot after a deliberate graceful shutdown would find
// that stale record alongside a kube-vip manifest parked on purpose - and
// restore it, putting the node back into the VIP underneath the recycle. Keying
// on the operation only helps if the operation stops saying "restore me" once
// it has been honoured.
func TestReconcileInterruptedStaticPodParksSettlesTheRecordItActedOn(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.Persister = &MockPersister{}
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	parkManifest(t, s, defaultKubeVipManifestName)

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))
	require.True(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName), "precondition: the first pass restores")

	assert.Equal(t, NodeAgentOperationPhaseFailed, s.NodeStatus.NodeAgentOperations["defrag-1"].Phase,
		"the record must be terminal, and failed rather than deleted so a poll still gets an answer")

	// Now a deliberate park, as node recycling would leave it, and another
	// restart.
	parkManifest(t, s, defaultKubeVipManifestName)
	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName),
		"the honoured record must not restore a later, unrelated park")
}

// An operation with a live worker is still running, not interrupted. The
// reconciler only ever runs before the agent serves, so this cannot happen in
// production, but getting the condition wrong would make it possible for a
// future caller to race a restore against the operation performing it.
func TestReconcileInterruptedStaticPodParksIgnoresAnActiveOperation(t *testing.T) {
	s, _ := etcdRestartTestServer(t, &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1})
	s.NodeStatus.NodeAgentOperations = runningEtcdDefragRecord()
	s.activeOperations = map[string]struct{}{"defrag-1": {}}
	parkManifest(t, s, defaultKubeVipManifestName)

	require.NoError(t, s.ReconcileInterruptedStaticPodParks(context.Background()))

	assert.False(t, staticPodManifestIsActive(t, s, defaultKubeVipManifestName))
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

// A timestamped forensic snapshot shares the backup directory with the parked
// manifests, and restoring one would overwrite a live manifest with an old
// copy. Two independent things prevent it: parkedStaticPodManifests stats an
// exact fixed name, and the restore itself resolves source and destination from
// that same fixed name and stops when the destination is already active. This
// is a guard on the pair rather than a regression test for either - breaking
// one alone does not reach the live manifest.
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
