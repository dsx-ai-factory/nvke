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
	reason string
}

func (p loggingOperationProgress) SetStep(step string) {
	slog.Info("static pod park reconcile step", "reason", p.reason, "step", step)
}

func (p loggingOperationProgress) SetResult(string) {}

// staticPodParkingOperationTypes are the operation types that park a
// static-pod manifest and owe its restore within the same call, so an
// interrupted one leaves a debt nothing else will settle.
//
// Only etcd defragmentation qualifies. The graceful-shutdown family is excluded
// by design, not by omission: its park is the point of the operation and its
// restore is a separate RPC the caller owns, so an interrupted one is still a
// node someone means to have out of service.
var staticPodParkingOperationTypes = map[string]bool{
	operationTypeEtcdDefrag: true,
}

type staticPodParkRestore struct {
	manifestName string
	restore      func(context.Context, nvkeutils.CommandRunner, nodeAgentOperationProgress) error
}

// staticPodParkRestores is every manifest this reconciler can put back, in the
// order it puts them back, paired with the same helper the operations
// themselves restore with. Sharing those helpers is what keeps the two from
// drifting: kube-vip in particular is not considered back until its address is
// on lo again, and a node where kube-vip is not configured is skipped rather
// than failed.
//
// The order is a dependency order rather than a preference: kube-vip last,
// because advertising the control-plane VIP from a node whose apiserver is not
// up yet sends traffic to an endpoint that will fail it.
//
// etcd is absent because nothing parks etcd.yaml. StartEtcdDefrag restarts that
// container through the CRI and leaves its manifest in place, which is what
// makes the member kubelet's problem to bring back rather than this
// reconciler's.
//
// The ordering is best-effort, not a health gate: each step waits for a
// container to be running, not for the service behind it to be ready. Waiting
// for the apiserver to serve would mean probing it before the agent serves
// anything, and it already retries an etcd backend that is not up yet, which is
// the same thing it does on an ordinary reboot.
func (s *NodeAgentServer) staticPodParkRestores() []staticPodParkRestore {
	return []staticPodParkRestore{
		{defaultKubeAPIServerManifestName, s.restoreKubeAPIServerLocked},
		{defaultKubeVipManifestName, s.restoreKubeVipLocked},
	}
}

// ReconcileInterruptedStaticPodParks puts back static-pod manifests that an
// operation moved aside and then died before restoring, and is the crash-safety
// half of the bundled etcd defragmentation sequence. Run it at startup, before
// the agent serves RPCs.
//
// The durable record is the backup directory itself. A fixed-name manifest
// there means the agent renamed it out of the manifest directory and has not
// renamed it back, and that survives the agent dying, systemd restarting it and
// the node rebooting, so no separate state file is needed. Only the two
// manifests the stop paths touch can ever appear: kube-apiserver and kube-vip.
// Timestamped forensic snapshots live in the same directory but
// cannot be confused with them, because snapshotStaticPodManifest requires a
// prefix ending in a dot.
//
// A parked manifest is not on its own evidence that a restore is owed, which is
// the trap this function exists to avoid. The graceful-shutdown RPC parks
// kube-vip, and optionally kube-apiserver, deliberately, and expects them to
// stay parked until a caller explicitly restores them - that is how a node is
// taken out of service for recycling. Restoring on sight would re-advertise the
// VIP on a node someone is deliberately taking out of service.
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

	// Settle the records before touching anything, and whether or not there is
	// anything parked. A record left non-terminal is a standing instruction to
	// restore, so leaving it would make every later startup act on it again -
	// and by then the parked kube-vip manifest it finds could be one a
	// deliberate graceful shutdown put there, which is precisely what keying on
	// the operation rather than the file is meant to prevent.
	if err := s.failInterruptedStaticPodParkingOperations(ctx, interrupted); err != nil {
		return err
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

	// Held for the same reason the operations hold it: these are static-pod
	// mutations. Nothing can race this today because it runs before the gRPC
	// server accepts a connection, and taking the lock is what keeps that true
	// if a future caller runs it from somewhere else.
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}
	progress := loggingOperationProgress{reason: "interrupted-operation-recovery"}

	var failures []error
	for _, candidate := range s.staticPodParkRestores() {
		if !parked[candidate.manifestName] {
			continue
		}
		if err := candidate.restore(ctx, runner, progress); err != nil {
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

// interruptedStaticPodParkingOperations returns the IDs of operations in
// staticPodParkingOperationTypes that were left non-terminal with no active
// worker, which is what an agent restart mid-operation leaves behind.
func (s *NodeAgentServer) interruptedStaticPodParkingOperations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureNodeAgentOperationStateLocked()
	var interrupted []string
	for id, op := range s.NodeStatus.NodeAgentOperations {
		if !staticPodParkingOperationTypes[op.Type] || nodeAgentOperationTerminal(op.Phase) {
			continue
		}
		if _, active := s.activeOperations[id]; active {
			continue
		}
		interrupted = append(interrupted, id)
	}
	return interrupted
}

// failInterruptedStaticPodParkingOperations moves the given records to a
// terminal failed phase and persists them, so no later startup treats them as
// a restore still owed.
//
// Failed rather than deleted: a caller polling GetEtcdDefragStatus should be
// told the attempt died with the agent, not given a NotFound that reads as
// though it never ran. Such a poll sees phase FAILED with that message and no
// typed outcome, which is the honest answer - the record is settled before the
// restores are attempted, so at this point nothing can promise one.
func (s *NodeAgentServer) failInterruptedStaticPodParkingOperations(ctx context.Context, ids []string) error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	for _, id := range ids {
		op, ok := s.NodeStatus.NodeAgentOperations[id]
		if !ok {
			continue
		}
		op.Phase = NodeAgentOperationPhaseFailed
		// Deliberately does not claim the restore succeeded. This is written
		// before it is attempted, and the agent log is where the result of that
		// attempt is reported.
		op.ErrorMessage = "the agent restarted while this operation was running; " +
			"it did not complete and any static-pod manifests it had parked are being restored at startup"
		s.NodeStatus.NodeAgentOperations[id] = op
	}
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()

	slog.WarnContext(ctx, "failing operations left running by an agent restart", "operationIDs", ids)
	if s.Persister == nil {
		return nil
	}
	if err := s.Persister.Persist(&statusCopy); err != nil {
		// Returning here, before anything is restored, is deliberate. If the
		// record cannot be settled then restoring now would be undone in the
		// sense that matters: the next startup would find the same standing
		// instruction and act on whatever is parked by then.
		return fmt.Errorf("persist interrupted operation records: %w", err)
	}
	return nil
}

// parkedStaticPodManifests reports which of the restorable fixed-name manifests
// are currently sitting in the backup directory. Missing directory means none,
// which is the normal case and not an error.
func (s *NodeAgentServer) parkedStaticPodManifests() (map[string]bool, error) {
	restores := s.staticPodParkRestores()
	parked := make(map[string]bool, len(restores))
	for _, candidate := range restores {
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
