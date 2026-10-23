// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	// kubeAPIServerManifestPath is the static-pod manifest read by kubelet.
	// Mutating the file in place causes kubelet to restart the apiserver pod.
	kubeAPIServerManifestPath = "/etc/kubernetes/manifests/kube-apiserver.yaml"

	// apiServerManifestBackupDir holds timestamped snapshots of the manifest
	// taken before PatchAPIServerManifest mutates it. The directory lives
	// outside /etc/kubernetes/manifests/ so kubelet does not try to start a
	// second apiserver pod from the backup file.
	apiServerManifestBackupDir = "/etc/kubernetes/manifests-backup"

	// kubeAPIServerContainerName is the name kubeadm gives to the apiserver
	// container in the static-pod manifest. Used as a sanity check when
	// locating the container to mutate.
	kubeAPIServerContainerName = "kube-apiserver"

	// kubeVipContainerName is the name of the kube-vip container in its
	// static-pod manifest.
	kubeVipContainerName = "kube-vip"

	// patchManifestFileMode is the on-disk mode of the static-pod manifest and
	// its snapshots.
	patchManifestFileMode os.FileMode = 0o600

	// apiServerManifestBackupPrefix is the filename prefix of the timestamped
	// snapshots this RPC writes into apiServerManifestBackupDir. It ends in a dot
	// so a glob on it never matches the sibling fixed-name backups other flows
	// (e.g. control-plane graceful shutdown) keep in the same directory.
	apiServerManifestBackupPrefix = "kube-apiserver.yaml."

	// maxAPIServerManifestBackups bounds how many of this RPC's snapshots are
	// retained; older ones are pruned so repeated calls do not grow the backup
	// directory without bound.
	maxAPIServerManifestBackups = 5
)

// PatchAPIServerManifest applies an allowlisted, convergent change to the local
// kube-apiserver static-pod manifest: a strategic-merge patch (keyed sections
// such as volumes/volumeMounts/env), convergent flag operations (the keyless
// command/args string list strategic-merge cannot reconcile), plus staging and
// removal of on-disk files that back volume mounts. It is the general form of
// the Enable/DisableAPIServerAuthenticationConfig RPCs.
//
// The manifest is manipulated as JSON throughout so unknown fields the vendored
// Kubernetes types do not model survive the round-trip; corev1.Pod{} is passed
// to strategic-merge only as merge-key metadata.
//
// Per the two-lane error-boundary contract the APPLIED / UNCHANGED /
// FAILED_RESTORED / FAILED_NOT_RESTORED business outcomes ride on the response
// with a NIL gRPC error, so a rollout can tell a clean halt (FAILED_RESTORED)
// from a node that needs intervention (FAILED_NOT_RESTORED). Only role,
// request-validation, and manifest-read/parse failures use real gRPC error
// codes.
func (s *NodeAgentServer) PatchAPIServerManifest(
	ctx context.Context,
	req *pb.PatchAPIServerManifestRequest,
) (*pb.PatchAPIServerManifestResponse, error) {
	if !s.isControlPlaneRole() {
		role := ""
		if s.NodeStatus != nil {
			role = s.NodeStatus.Role
		}
		slog.Info("unable to fulfill PatchAPIServerManifest from non-controlplane node", "NodeRole", role)
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"patch apiserver manifest: control-plane node required, current role %q",
			role,
		)
	}

	if err := validatePatchAPIServerManifestRequest(req); err != nil {
		slog.Error("rejecting PatchAPIServerManifest request", "err", err)
		return nil, status.Errorf(codes.InvalidArgument, "patch apiserver manifest: %v", err)
	}

	// Phase 1: compute the target manifest entirely in memory. No disk writes
	// happen here, so any failure leaves the node untouched and needs no
	// rollback.
	manifestPath := s.resolveFilePath(kubeAPIServerManifestPath)
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		slog.Error("failed to read kube-apiserver static-pod manifest", "path", manifestPath, "err", err)
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"patch apiserver manifest: read apiserver manifest %s: %v",
			manifestPath, err,
		)
	}
	origJSON, err := yaml.YAMLToJSON(manifestRaw)
	if err != nil {
		slog.Error("failed to parse kube-apiserver static-pod manifest", "path", manifestPath, "err", err)
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"patch apiserver manifest: parse apiserver manifest %s: %v",
			manifestPath, err,
		)
	}

	mergedJSON := origJSON
	for i, smp := range req.GetStrategicMergePatches() {
		if len(smp) == 0 {
			continue
		}
		patchJSON, perr := yaml.YAMLToJSON(smp)
		if perr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "patch apiserver manifest: parse strategic merge patch %d: %v", i, perr)
		}
		mergedJSON, err = strategicpatch.StrategicMergePatch(mergedJSON, patchJSON, corev1.Pod{})
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "patch apiserver manifest: apply strategic merge patch %d: %v", i, err)
		}
	}

	if len(req.GetFlagOps()) > 0 {
		mergedJSON, err = applyFlagOps(mergedJSON, kubeAPIServerContainerName, req.GetFlagOps())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "patch apiserver manifest: apply flag ops: %v", err)
		}
	}

	manifestChanged := !jsonSemanticEqual(origJSON, mergedJSON)

	// Phase 2: plan file staging read-only so we know whether files would change
	// — a write or a removal — for the UNCHANGED short-circuit, without writing
	// anything yet.
	staging, filesChanged, err := s.planFileStaging(req.GetFiles(), req.GetRemoveFiles())
	if err != nil {
		slog.Error("failed to plan file staging for PatchAPIServerManifest", "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "patch apiserver manifest: %v", err)
	}

	if !manifestChanged && !filesChanged {
		slog.Info("PatchAPIServerManifest: manifest and staged files already match request - UNCHANGED", "manifest", manifestPath)
		return &pb.PatchAPIServerManifestResponse{
			Outcome: pb.PatchAPIServerManifestResponse_OUTCOME_UNCHANGED,
		}, nil
	}

	// Phase 3: commit. Files are staged just before the manifest write so
	// volume-mount targets exist for the pod swap; any failure rolls back the
	// staged files (restoring overwritten ones to their prior state) and, when
	// the manifest was already rewritten, the manifest too, then re-verifies
	// readiness. The running apiserver auto-reloads files it watches
	// (authentication-config, encryption-config, serving/client certs), so a
	// files change is never assumed harmless — it is rolled back and verified.
	backupPath := ""
	if manifestChanged {
		mergedYAML, merr := yaml.JSONToYAML(mergedJSON)
		if merr != nil {
			return s.buildPatchCleanFailureResponse("", fmt.Sprintf("marshal merged apiserver manifest: %v", merr)), nil
		}

		backupPath, err = s.snapshotAPIServerManifest(manifestPath, manifestRaw)
		if err != nil {
			slog.Error("failed to snapshot kube-apiserver static-pod manifest", "err", err)
			return s.buildPatchCleanFailureResponse("", fmt.Sprintf("snapshot apiserver manifest: %v", err)), nil
		}

		// Capture the kubelet mirror-pod UID immediately before we touch the
		// static-pod manifest so the post-mutation wait gates on the pod swap,
		// not on the OLD apiserver still serving /readyz during kubelet's
		// --file-check-frequency window. Empty UID degrades to a bare wait.
		preMutationUID, uidErr := s.readAPIServerMirrorPodUID(ctx)
		if uidErr != nil {
			slog.Warn("PatchAPIServerManifest: failed to read pre-mutation mirror-pod UID; degrading to bare /readyz wait", "err", uidErr)
		}

		// Stage files just before the manifest write. A staging failure here has
		// not rewritten the manifest, so roll the files back and re-verify (no
		// manifest restore needed).
		if err := s.commitFileStaging(staging); err != nil {
			slog.Error("failed to stage files for PatchAPIServerManifest", "err", err)
			return s.buildPatchRestoreResponse(ctx, false, backupPath, manifestPath, manifestRaw, staging, fmt.Sprintf("stage files: %v", err)), nil
		}

		manifestSpec := fileSpec{path: manifestPath, content: mergedYAML, mode: patchManifestFileMode, uid: 0, gid: 0}
		if err := atomicWriteFileWithOwnership(manifestSpec, s.kubeadmFileOps); err != nil {
			slog.Error("failed to atomic-write merged kube-apiserver manifest", "err", err)
			// The atomic write failed, so the original manifest is intact; the
			// staged files may already have been auto-reloaded, so roll them back
			// and re-verify without restoring the (unchanged) manifest.
			return s.buildPatchRestoreResponse(ctx, false, backupPath, manifestPath, manifestRaw, staging, fmt.Sprintf("write merged apiserver manifest %s: %v", manifestPath, err)), nil
		}

		// Past this point the live manifest is mutated. Any failure must restore
		// the manifest and the files, then re-verify readiness.
		if err := s.waitForAPIServerReadyAfterMutation(ctx, preMutationUID); err != nil {
			slog.Error("kube-apiserver did not become ready after manifest patch", "err", err)
			return s.buildPatchRestoreResponse(ctx, true, backupPath, manifestPath, manifestRaw, staging, fmt.Sprintf("apiserver readiness wait failed: %v", err)), nil
		}
	} else if len(staging.toWrite) > 0 {
		// Files-write-only change: no static-pod swap, so no manifest snapshot.
		// Stage the files; the running apiserver auto-reloads any it watches. On a
		// staging or readiness failure, roll the files back to their prior state
		// (the apiserver reloads back) and re-verify. A removes-only change falls
		// through to the removal below with no write, wait, or rollback: deleting
		// files the manifest no longer references cannot affect the apiserver.
		if err := s.commitFileStaging(staging); err != nil {
			slog.Error("failed to stage files for PatchAPIServerManifest", "err", err)
			return s.buildPatchRestoreResponse(ctx, false, "", manifestPath, manifestRaw, staging, fmt.Sprintf("stage files: %v", err)), nil
		}
		if err := s.waitForAPIServerReady(ctx); err != nil {
			slog.Error("kube-apiserver not Ready after files-only patch", "err", err)
			return s.buildPatchRestoreResponse(ctx, false, "", manifestPath, manifestRaw, staging, fmt.Sprintf("apiserver readiness wait failed after files-only change: %v", err)), nil
		}
	}

	// Success: unlink the planned remove_files. After a manifest or file change
	// the apiserver is confirmed Ready first; a removes-only change reaches here
	// directly, since the files are no longer referenced. Best-effort and
	// self-healing — a failed removal leaves the file, which the next run re-plans
	// as pending. Absence is not an error.
	for _, resolved := range staging.toRemove {
		if err := os.Remove(resolved); err != nil && !os.IsNotExist(err) {
			slog.Warn("PatchAPIServerManifest: failed to remove requested file (best-effort)", "path", resolved, "err", err)
		}
	}

	slog.Info(
		"PatchAPIServerManifest: applied convergent change to local kube-apiserver",
		"manifest", manifestPath,
		"manifestChanged", manifestChanged,
		"filesChanged", filesChanged,
		"backup", backupPath,
	)
	return &pb.PatchAPIServerManifestResponse{
		Outcome:            pb.PatchAPIServerManifestResponse_OUTCOME_APPLIED,
		BackupManifestPath: backupPath,
	}, nil
}

// priorFile captures an existing file's pre-call state — content, mode, and
// ownership — so a rollback can restore it byte-for-byte with the same
// permissions.
type priorFile struct {
	content []byte
	mode    os.FileMode
	uid     int
	gid     int
}

// fileStaging is the plan/undo record for a PatchAPIServerManifest file stage:
// the files to write, the resolved paths of those that did not previously exist
// (removed on rollback), and the prior state of those overwritten in place
// (restored on rollback).
type fileStaging struct {
	toWrite      []*pb.File
	newlyCreated []string
	overwritten  map[string]priorFile
	toRemove     []string // resolved remove_files paths that currently exist
}

// buildPatchCleanFailureResponse handles a failure where nothing that affects
// the running apiserver was mutated — an in-memory computation failure, or a
// pre-write failure such as snapshotting. No files were committed and the
// manifest was not touched, so there is nothing to roll back and no readiness
// wait to run. The outcome is FAILED_RESTORED: the node is clean.
func (s *NodeAgentServer) buildPatchCleanFailureResponse(
	backupPath, failureMessage string,
) *pb.PatchAPIServerManifestResponse {
	slog.Warn("PatchAPIServerManifest: failed before any mutation; node left in original state", "reason", failureMessage)
	return &pb.PatchAPIServerManifestResponse{
		Outcome:            pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED,
		ErrorMessage:       failureMessage,
		BackupManifestPath: backupPath,
	}
}

// buildPatchRestoreResponse rolls back a failure that may have affected the
// running apiserver: it restores the manifest from the in-memory snapshot when
// the manifest had already been rewritten (restoreManifest), rolls the staged
// files back to their pre-call state, and re-verifies readiness. It reports a
// business Outcome with a NIL gRPC error: FAILED_RESTORED when the node is
// confirmed back to its pre-call state, FAILED_NOT_RESTORED when recovery could
// not be confirmed.
func (s *NodeAgentServer) buildPatchRestoreResponse(
	ctx context.Context,
	restoreManifest bool,
	backupPath, manifestPath string,
	manifestRaw []byte,
	staging fileStaging,
	failureMessage string,
) *pb.PatchAPIServerManifestResponse {
	restored, restoreErr := s.restoreNode(ctx, restoreManifest, manifestPath, manifestRaw, staging)
	if !restored {
		return &pb.PatchAPIServerManifestResponse{
			Outcome:            pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_NOT_RESTORED,
			ErrorMessage:       fmt.Sprintf("%s; node not confirmed restored: %v", failureMessage, restoreErr),
			BackupManifestPath: backupPath,
		}
	}
	return &pb.PatchAPIServerManifestResponse{
		Outcome:            pb.PatchAPIServerManifestResponse_OUTCOME_FAILED_RESTORED,
		ErrorMessage:       failureMessage,
		BackupManifestPath: backupPath,
	}
}

// planFileStaging inspects the requested files and remove_files against their
// on-disk state WITHOUT changing anything, returning the write/remove plan and
// the rollback record. A file that already matches is skipped; a file that does
// not exist is recorded as newly-created; a file that exists with different
// content is recorded with its prior state so a rollback can restore it. A
// remove_files path that currently exists is recorded for deletion. The bool
// reports whether any file would change — a write OR a removal — so a re-run that
// still has files to delete is not mistaken for a no-op.
func (s *NodeAgentServer) planFileStaging(files []*pb.File, removeFiles []string) (fileStaging, bool, error) {
	staging := fileStaging{overwritten: map[string]priorFile{}}
	for _, f := range files {
		resolved := s.resolveFilePath(f.GetPath())
		existing, readErr := os.ReadFile(resolved)
		switch {
		case readErr == nil:
			if bytes.Equal(existing, f.GetContent()) {
				continue // already matches; leave it untouched
			}
			prior, perr := capturePriorFile(resolved, existing)
			if perr != nil {
				return staging, false, fmt.Errorf("capturing prior state of %s: %w", resolved, perr)
			}
			staging.overwritten[resolved] = prior
		case os.IsNotExist(readErr):
			staging.newlyCreated = append(staging.newlyCreated, resolved)
		default:
			return staging, false, fmt.Errorf("reading %s: %w", resolved, readErr)
		}
		staging.toWrite = append(staging.toWrite, f)
	}

	for _, p := range removeFiles {
		resolved := s.resolveFilePath(p)
		// Lstat (not Stat) so a symlink counts as present — os.Remove deletes the
		// link itself. An absent (or unreadable) path needs no removal.
		if _, err := os.Lstat(resolved); err == nil {
			staging.toRemove = append(staging.toRemove, resolved)
		}
	}

	changed := len(staging.toWrite) > 0 || len(staging.toRemove) > 0
	return staging, changed, nil
}

// commitFileStaging writes the planned files to disk. On error some files may
// already have been written; the caller rolls them back via rollbackFileStaging.
func (s *NodeAgentServer) commitFileStaging(staging fileStaging) error {
	if len(staging.toWrite) == 0 {
		return nil
	}
	return s.writeFilesToDisk(staging.toWrite)
}

// rollbackFileStaging undoes commitFileStaging best-effort: newly-created files
// are removed and overwritten files are restored to their captured prior state
// (content, mode, ownership).
func (s *NodeAgentServer) rollbackFileStaging(staging fileStaging) {
	removeFilesBestEffort(staging.newlyCreated)
	for path, prior := range staging.overwritten {
		spec := fileSpec{path: path, content: prior.content, mode: prior.mode, uid: prior.uid, gid: prior.gid}
		if err := atomicWriteFileWithOwnership(spec, s.kubeadmFileOps); err != nil {
			slog.Warn("PatchAPIServerManifest: failed to restore overwritten file during rollback (best-effort)", "path", path, "err", err)
		}
	}
}

// capturePriorFile records an existing file's content, mode, and ownership so a
// rollback can restore it faithfully.
func capturePriorFile(path string, content []byte) (priorFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return priorFile{}, err
	}
	pf := priorFile{content: content, mode: info.Mode().Perm()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		pf.uid = int(st.Uid)
		pf.gid = int(st.Gid)
	}
	return pf, nil
}

// snapshotAPIServerManifest copies the current manifest into
// apiServerManifestBackupDir under a timestamped name and returns the absolute
// backup path. A bounded number of the most recent snapshots is retained (older
// ones are pruned) so operators keep a short forensic trail via
// backup_manifest_path without repeated calls growing the directory without
// bound.
func (s *NodeAgentServer) snapshotAPIServerManifest(manifestPath string, manifestRaw []byte) (string, error) {
	return s.snapshotStaticPodManifest(
		manifestPath,
		apiServerManifestBackupPrefix,
		maxAPIServerManifestBackups,
		manifestRaw,
	)
}

// restoreNode rolls the node back to its pre-call state and re-verifies
// readiness. When restoreManifest is set (the manifest had already been
// rewritten) it atomic-writes the in-memory pre-call manifest bytes back and
// gives the apiserver a mutation-aware readiness wait, since a pod swap is
// expected; otherwise only files changed, so a bare readiness wait after the
// apiserver's file auto-reload is correct. It always rolls the staged files
// back to their prior state. Returns (true, nil) only when the rollback
// succeeded AND the apiserver returned to Ready — rewriting the original bytes
// is not by itself proof of recovery, so the wait is load-bearing. Returns
// (false, err) otherwise, which the caller reports as FAILED_NOT_RESTORED.
func (s *NodeAgentServer) restoreNode(
	ctx context.Context,
	restoreManifest bool,
	manifestPath string,
	manifestRaw []byte,
	staging fileStaging,
) (bool, error) {
	waitReady := func() error { return s.waitForAPIServerReady(ctx) }
	if restoreManifest {
		preMutationUID, uidErr := s.readAPIServerMirrorPodUID(ctx)
		if uidErr != nil {
			slog.Warn("restoreNode: failed to read pre-mutation mirror-pod UID; degrading to bare /readyz wait", "err", uidErr)
		}
		spec := fileSpec{path: manifestPath, content: manifestRaw, mode: patchManifestFileMode, uid: 0, gid: 0}
		if err := atomicWriteFileWithOwnership(spec, s.kubeadmFileOps); err != nil {
			slog.Error("restoreNode: failed to rewrite manifest from in-memory snapshot", "manifest", manifestPath, "err", err)
			return false, err
		}
		slog.Info("restoreNode: rewrote kube-apiserver manifest from in-memory snapshot", "manifest", manifestPath)
		waitReady = func() error { return s.waitForAPIServerReadyAfterMutation(ctx, preMutationUID) }
	}

	s.rollbackFileStaging(staging)

	if err := waitReady(); err != nil {
		slog.Error("restoreNode: apiserver did not return to Ready after rollback; reporting unconfirmed recovery", "manifest", manifestPath, "err", err)
		return false, fmt.Errorf("node rolled back but apiserver did not return to Ready: %w", err)
	}
	return true, nil
}

// removeFilesBestEffort unlinks each path, ignoring not-exist. Used to clean up
// files a failed call created.
func removeFilesBestEffort(paths []string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("PatchAPIServerManifest: failed to remove staged file (best-effort)", "path", p, "err", err)
		}
	}
}

// validatePatchAPIServerManifestRequest enforces the request contract: at least
// one of the four mutation inputs is present; every staged/removed file path is
// validated by the same validateKubeadmFilePath the init/join file-write RPCs
// use (absolute, deduplicated, not root-restricted); each staged file is
// <= 1 MiB; and every flag op carries a well-formed Set or Remove whose name is
// a single bare token. Staged files are additionally checked by
// validateKubeadmFileDestination (rejecting symlinked destinations) at write
// time via writeFilesToDisk, so file handling matches init/join end to end.
func validatePatchAPIServerManifestRequest(req *pb.PatchAPIServerManifestRequest) error {
	if req == nil {
		return errors.New("request is required")
	}
	if len(req.GetStrategicMergePatches()) == 0 &&
		len(req.GetFlagOps()) == 0 &&
		len(req.GetFiles()) == 0 &&
		len(req.GetRemoveFiles()) == 0 {
		return errors.New("request must set at least one of strategic_merge_patches, flag_ops, files, remove_files")
	}

	seen := make(map[string]struct{})
	for i, f := range req.GetFiles() {
		if f == nil {
			return fmt.Errorf("file entry %d is nil", i)
		}
		if len(f.GetContent()) > maxKubeadmFileBytes {
			return fmt.Errorf(
				"file entry %d (%s) content exceeds max size (%d bytes): got %d bytes",
				i, f.GetPath(), maxKubeadmFileBytes, len(f.GetContent()),
			)
		}
		cleaned, err := validateKubeadmFilePath(f.GetPath(), seen)
		if err != nil {
			return fmt.Errorf("file entry %d: %w", i, err)
		}
		seen[cleaned] = struct{}{}
	}

	removeSeen := make(map[string]struct{})
	for i, p := range req.GetRemoveFiles() {
		cleaned, err := validateKubeadmFilePath(p, removeSeen)
		if err != nil {
			return fmt.Errorf("remove_files entry %d: %w", i, err)
		}
		removeSeen[cleaned] = struct{}{}
	}

	for i, op := range req.GetFlagOps() {
		if op == nil {
			return fmt.Errorf("flag op %d is nil", i)
		}
		switch {
		case op.GetSet() != nil:
			if err := validateFlagName(op.GetSet().GetName()); err != nil {
				return fmt.Errorf("flag op %d (set): %w", i, err)
			}
		case op.GetRemove() != nil:
			if err := validateFlagName(op.GetRemove().GetName()); err != nil {
				return fmt.Errorf("flag op %d (remove): %w", i, err)
			}
		default:
			return fmt.Errorf("flag op %d has neither set nor remove", i)
		}
	}

	return nil
}

// The convergent flag helpers this RPC relies on (applyFlagOps, validateFlagName,
// jsonSemanticEqual and friends) live in static_pod_flags.go, shared with the
// etcd manifest RPCs.
