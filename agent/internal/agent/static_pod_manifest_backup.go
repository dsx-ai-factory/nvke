// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Timestamped snapshots of static-pod manifests taken before an RPC mutates
// them, shared by the kube-apiserver and etcd manifest patch paths.
//
// Snapshots live in defaultStaticPodBackupDir, which is deliberately outside
// /etc/kubernetes/manifests/ so kubelet does not try to start a second pod from
// a backup file. Each caller passes its own prefix, which must end in a dot so
// a glob on it never matches the sibling fixed-name manifests that the
// stop/restore static-pod flow parks in the same directory.

// snapshotStaticPodManifest copies manifestRaw into the backup directory under
// a timestamped name derived from backupPrefix and returns the absolute backup
// path. The newest maxBackups snapshots carrying that prefix are retained and
// older ones pruned, so operators keep a short forensic trail without repeated
// calls growing the directory without bound.
func (s *NodeAgentServer) snapshotStaticPodManifest(
	manifestPath, backupPrefix string,
	maxBackups int,
	manifestRaw []byte,
) (string, error) {
	backupDir := s.rootedPath(defaultStaticPodBackupDir)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup dir %s: %w", backupDir, err)
	}
	backupName := backupPrefix + time.Now().UTC().Format("20060102T150405.000000000Z")
	backupPath := filepath.Join(backupDir, backupName)

	spec := fileSpec{path: backupPath, content: manifestRaw, mode: patchManifestFileMode, uid: 0, gid: 0}
	if err := atomicWriteFileWithOwnership(spec, s.kubeadmFileOps); err != nil {
		return "", fmt.Errorf("writing snapshot %s: %w", backupPath, err)
	}
	slog.Info("snapshotted static pod manifest", "from", manifestPath, "to", backupPath)

	pruneStaticPodManifestBackups(backupDir, backupPrefix, maxBackups)
	return backupPath, nil
}

// pruneStaticPodManifestBackups keeps only the newest maxBackups snapshots
// matching backupPrefix, removing older ones. Best-effort: failures are logged,
// not returned, and the just-written snapshot is always among those kept. Only
// snapshots carrying the prefix are considered; other files sharing the backup
// directory are left untouched.
func pruneStaticPodManifestBackups(backupDir, backupPrefix string, maxBackups int) {
	matches, err := filepath.Glob(filepath.Join(backupDir, backupPrefix+"*"))
	if err != nil {
		slog.Warn("failed to list static pod manifest backups for pruning (best-effort)", "dir", backupDir, "err", err)
		return
	}
	if len(matches) <= maxBackups {
		return
	}
	// The fixed-width UTC timestamp suffix sorts lexicographically in
	// chronological order, so the newest snapshots are last.
	sort.Strings(matches)
	for _, old := range matches[:len(matches)-maxBackups] {
		if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to prune old static pod manifest backup (best-effort)", "path", old, "err", err)
		}
	}
}
