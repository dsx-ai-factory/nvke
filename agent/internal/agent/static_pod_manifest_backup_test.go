// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticPodBackupTestServer returns a server rooted at a temporary directory.
// The snapshot write resolves root's uid and gid, which a test environment's
// user database may not carry.
func staticPodBackupTestServer(t *testing.T) *NodeAgentServer {
	t.Helper()
	return &NodeAgentServer{
		RootPath: t.TempDir(),
		kubeadmFileOps: fileOps{
			lookupUser: func(string) (*user.User, error) {
				return &user.User{Username: "root", Uid: "0"}, nil
			},
			lookupGroup: func(string) (*user.Group, error) {
				return &user.Group{Name: "root", Gid: "0"}, nil
			},
			chown: func(string, int, int) error { return nil },
		},
	}
}

// The hazard this guards: snapshots and parked manifests share one directory,
// and the trailing dot is what keeps the pruner's glob off the parked ones. A
// prefix of "kube-vip.yaml" rather than "kube-vip.yaml." makes that glob match
// the parked manifest and os.Remove it - destroying the only copy of a
// control-plane pod that exists while an operation holds it aside, with nothing
// but a best-effort log to say so. The reconciler's safety argument cites this
// requirement, so it has to be enforced rather than assumed.
//
// The retention of one is what makes the hazard bite: without the dot the
// parked manifest is a second match, and pruning to one deletes it.
func TestSnapshotStaticPodManifestRequiresADotTerminatedPrefix(t *testing.T) {
	s := staticPodBackupTestServer(t)
	parked := filepath.Join(s.RootPath, defaultStaticPodBackupDir, defaultKubeVipManifestName)
	require.NoError(t, os.MkdirAll(filepath.Dir(parked), 0o700))
	require.NoError(t, os.WriteFile(parked, []byte("parked: true\n"), 0o600))

	_, err := s.snapshotStaticPodManifest("/etc/kubernetes/manifests/kube-vip.yaml",
		defaultKubeVipManifestName, 1, []byte("snapshot: true\n"))

	// Asserted rather than required, so a regression reports both the missing
	// rejection and the destroyed manifest rather than only the first.
	assert.ErrorIs(t, err, errBackupPrefixNotDotTerminated)
	assert.FileExists(t, parked, "the parked manifest must survive a rejected prefix")
}

// maxBackups below one is not a retention policy: zero would delete the
// snapshot written moments earlier, and a negative value would slice past the
// end of the match list. The pruner cannot report an error, so it declines.
func TestPruneStaticPodManifestBackupsDeclinesRetentionBelowOne(t *testing.T) {
	for _, maxBackups := range []int{0, -1} {
		dir := t.TempDir()
		snapshot := filepath.Join(dir, apiServerManifestBackupPrefix+"20260101T000000.000000000Z")
		require.NoError(t, os.WriteFile(snapshot, []byte("snapshot: true\n"), 0o600))

		pruneStaticPodManifestBackups(dir, apiServerManifestBackupPrefix, maxBackups)

		assert.FileExists(t, snapshot, "a retention of %d must not delete anything", maxBackups)
	}
}

func TestSnapshotStaticPodManifestPrunesOldestFirst(t *testing.T) {
	s := staticPodBackupTestServer(t)
	backupDir := filepath.Join(s.RootPath, defaultStaticPodBackupDir)
	require.NoError(t, os.MkdirAll(backupDir, 0o700))

	// Older than anything snapshotStaticPodManifest will write now, so these are
	// the ones that must go.
	stale := []string{
		filepath.Join(backupDir, apiServerManifestBackupPrefix+"20200101T000000.000000000Z"),
		filepath.Join(backupDir, apiServerManifestBackupPrefix+"20200102T000000.000000000Z"),
	}
	for _, path := range stale {
		require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o600))
	}
	// A parked manifest in the same directory, which the prefix must not reach.
	parked := filepath.Join(backupDir, defaultKubeAPIServerManifestName)
	require.NoError(t, os.WriteFile(parked, []byte("parked: true\n"), 0o600))

	written, err := s.snapshotStaticPodManifest("/etc/kubernetes/manifests/kube-apiserver.yaml",
		apiServerManifestBackupPrefix, 2, []byte("snapshot: true\n"))
	require.NoError(t, err)

	assert.FileExists(t, written, "the snapshot just written is always kept")
	assert.NoFileExists(t, stale[0], "the oldest snapshot is pruned first")
	assert.FileExists(t, stale[1], "retention of 2 keeps the newer stale snapshot alongside the new one")
	assert.FileExists(t, parked, "pruning must never reach a parked manifest")
}
