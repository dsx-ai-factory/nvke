// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

// loggingOperationProgress reports step changes to the log instead of to a
// persisted operation record. The startup reconciler runs outside any
// operation, so there is nothing to record progress against, but the
// static-pod helpers still take a progress sink.
type loggingOperationProgress struct {
	context string
}

func (p loggingOperationProgress) SetStep(step string) {
	slog.Info("static pod park reconcile step", "context", p.context, "step", step)
}

func (p loggingOperationProgress) SetResult(string) {}

// parkedStaticPodRestoreOrder is the order manifests go back in, and it is a
// dependency order rather than a preference. etcd first because kube-apiserver
// is pointed at the local member only and cannot serve without it. kube-vip
// last because advertising the control-plane VIP from a node whose apiserver is
// not up yet sends traffic to an endpoint that will fail it.
var parkedStaticPodRestoreOrder = []struct {
	manifestName  string
	containerName string
	restoringStep string
	waitingStep   string
}{
	{etcdManifestName, etcdContainerName, stepEtcdDefragReturningToService, stepEtcdDefragWaitingRestart},
	{defaultKubeAPIServerManifestName, kubeAPIServerContainerName, stepRestoringKubeAPIServer, stepWaitingKubeAPIServerStart},
	{defaultKubeVipManifestName, kubeVipContainerName, stepRestoringKubeVip, stepWaitingKubeVipStart},
}

// ReconcileInterruptedStaticPodParks puts back static-pod manifests that an
// operation moved aside and then died before restoring, and is the crash-safety
// half of the bundled etcd defragmentation sequence. Run it at startup, before
// the agent serves RPCs.
//
// The durable record is the backup directory itself. A fixed-name manifest
// there means the agent renamed it out of the manifest directory and has not
// renamed it back, and that survives the agent dying, systemd restarting it and
// the node rebooting, so no separate state file is needed. Only the three
// manifests the stop paths touch can ever appear: etcd, kube-apiserver and
// kube-vip. Timestamped forensic snapshots live in the same directory but
// cannot be confused with them, because snapshotStaticPodManifest requires a
// prefix ending in a dot.
//
// A parked manifest is not on its own evidence that a restore is owed, which is
// the trap this function exists to avoid. The graceful-shutdown RPC parks
// kube-vip, and optionally kube-apiserver, deliberately, and expects them to
// stay parked until a caller explicitly restores them - that is how a node is
// taken out of service for recycling. Restoring on sight would re-advertise the
// VIP on a node someone is deliberately draining.
//
// So the discriminator is the operation record, not the file. An operation left
// non-terminal with no active in-process worker was interrupted mid-flight,
// which is exactly the condition inactiveNodeAgentOperationError already names,
// and only those operations are owed a restore here. A graceful shutdown that
// reached a terminal phase is a completed, intentional park and is left alone.
func (s *NodeAgentServer) ReconcileInterruptedStaticPodParks(ctx context.Context) error {
	interrupted := s.interruptedStaticPodParkingOperations()
	if len(interrupted) == 0 {
		return nil
	}

	parked, err := s.parkedStaticPodManifests()
	if err != nil {
		return err
	}
	if len(parked) == 0 {
		slog.InfoContext(ctx, "found interrupted static-pod operations but no parked manifests; nothing to restore",
			"operationIDs", interrupted)
		return nil
	}

	slog.WarnContext(ctx, "restoring static-pod manifests parked by an interrupted operation",
		"operationIDs", interrupted, "manifests", parked)

	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}
	progress := loggingOperationProgress{context: "interrupted-operation-recovery"}

	var failures []error
	for _, candidate := range parkedStaticPodRestoreOrder {
		if !parked[candidate.manifestName] {
			continue
		}
		if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
			containerName: candidate.containerName,
			manifestName:  candidate.manifestName,
			restoringStep: candidate.restoringStep,
			waitingStep:   candidate.waitingStep,
			timeout:       s.staticPodStartTimeoutOrDefault(),
		}); err != nil {
			// Carry on to the remaining manifests rather than returning. They
			// are independent pods, and leaving kube-vip parked because
			// kube-apiserver was slow to come back would compound one problem
			// into two.
			failures = append(failures, fmt.Errorf("restore %s: %w", candidate.manifestName, err))
			continue
		}
		slog.InfoContext(ctx, "restored a parked static-pod manifest", "manifest", candidate.manifestName)
	}
	return errors.Join(failures...)
}

// interruptedStaticPodParkingOperations returns the IDs of operations that park
// static-pod manifests and were left non-terminal with no active worker.
//
// Only etcd defragmentation qualifies. It is the one operation that parks a
// manifest it is itself responsible for putting back within the same call, so
// an interrupted one leaves a debt nothing else will settle. The
// graceful-shutdown family is excluded by design: its park is the point of the
// operation, and its restore is a separate RPC the caller owns.
func (s *NodeAgentServer) interruptedStaticPodParkingOperations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureNodeAgentOperationStateLocked()
	var interrupted []string
	for id, op := range s.NodeStatus.NodeAgentOperations {
		if op.Type != operationTypeEtcdDefrag || nodeAgentOperationTerminal(op.Phase) {
			continue
		}
		if _, active := s.activeOperations[id]; active {
			continue
		}
		interrupted = append(interrupted, id)
	}
	return interrupted
}

// parkedStaticPodManifests reports which of the restorable fixed-name manifests
// are currently sitting in the backup directory. Missing directory means none,
// which is the normal case and not an error.
func (s *NodeAgentServer) parkedStaticPodManifests() (map[string]bool, error) {
	parked := make(map[string]bool, len(parkedStaticPodRestoreOrder))
	for _, candidate := range parkedStaticPodRestoreOrder {
		path := s.rootedPath(filepath.Join(defaultStaticPodBackupDir, candidate.manifestName))
		switch _, err := os.Stat(path); {
		case err == nil:
			parked[candidate.manifestName] = true
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, fmt.Errorf("stat parked manifest %s: %w", path, err)
		}
	}
	return parked, nil
}
