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

// staticPodParkingOperationTypes are the operation types that park a static-pod
// manifest and owe its restore within the same call, so an interrupted one
// leaves a record claiming to be running that never will be again.
//
// This drives only the settling of those records, not the restores. Which
// manifests to put back is answered by defaultEtcdDefragParkDir, not from here.
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
		{defaultKubeAPIServerManifestName, func(ctx context.Context, runner nvkeutils.CommandRunner, progress nodeAgentOperationProgress) error {
			return s.restoreKubeAPIServerLocked(ctx, runner, progress, defaultEtcdDefragParkDir)
		}},
		{defaultKubeVipManifestName, func(ctx context.Context, runner nvkeutils.CommandRunner, progress nodeAgentOperationProgress) error {
			// No address to hand over: the withdrawal happened in a process that
			// is gone. Re-resolving works here because reaching this point means
			// the manifest is parked, which is one of the sources.
			return s.restoreKubeVipLocked(ctx, runner, progress, defaultEtcdDefragParkDir, kubeVipWithdrawal{})
		}},
	}
}

// ReconcileInterruptedStaticPodParks puts back static-pod manifests that an
// operation moved aside and then died before restoring, and is the crash-safety
// half of the bundled etcd defragmentation sequence. Run it at startup, before
// the agent serves RPCs.
//
// The durable record is defaultEtcdDefragParkDir itself. A manifest there means
// a defragmentation renamed it out of the manifest directory and has not
// renamed it back, and that survives the agent dying, systemd restarting it and
// the node rebooting, so no separate state, marker or content hash is needed.
// Only the two manifests the sequence parks can ever appear: kube-apiserver and
// kube-vip.
//
// The directory is also the whole of the discrimination. A graceful shutdown
// parks into the parent directory instead, deliberately, and expects its
// manifests to stay parked until a caller asks for them back - that is how a
// node is held out of service for recycling. Restoring one of those on sight
// would re-advertise the VIP underneath whoever is recycling the node, and this
// cannot do that because it never looks there. Timestamped forensic snapshots
// live in the parent too and are equally out of reach.
//
// Keying on the path rather than on an operation record is what makes this
// safe to repeat. Everything below is idempotent and derived from what is on
// disk right now, so an agent that dies partway through recovery simply finds
// the remaining manifests still parked and finishes the job next time. An
// earlier version keyed on the record and settled it first, which meant a crash
// in that window left a terminal record, nothing to act on, and two manifests
// stranded for good.
//
// Settling those records is still worth doing, so a caller polling
// GetEtcdDefragStatus is told the attempt died rather than seeing it run
// forever, but it is now a separate concern with no ordering against the
// restores and no ability to suppress them.
func (s *NodeAgentServer) ReconcileInterruptedStaticPodParks(ctx context.Context) error {
	parked, err := s.parkedStaticPodManifests()
	if err != nil {
		return err
	}

	var failures []error
	if len(parked) > 0 {
		slog.WarnContext(ctx, "restoring static-pod manifests parked by an interrupted etcd defragmentation",
			"dir", defaultEtcdDefragParkDir, "manifests", parked)
		failures = append(failures, s.restoreParkedStaticPodManifests(ctx, parked))
	}

	// After the restores, and not gating them. A record cannot be settled on a
	// node whose Persister is failing, and that must not be a reason to leave
	// the control plane parked.
	if interrupted := s.interruptedStaticPodParkingOperations(); len(interrupted) > 0 {
		failures = append(failures, s.failInterruptedStaticPodParkingOperations(ctx, interrupted))
	}
	return errors.Join(failures...)
}

// restoreParkedStaticPodManifests puts back the given parked manifests in
// dependency order.
func (s *NodeAgentServer) restoreParkedStaticPodManifests(ctx context.Context, parked map[string]bool) error {
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
			// into two. Whatever fails here stays parked, so the next startup
			// tries it again.
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
// terminal failed phase and persists them, so a caller polling for one gets an
// answer instead of watching an operation that no longer exists.
//
// Failed rather than deleted: a caller polling GetEtcdDefragStatus should be
// told the attempt died with the agent, not given a NotFound that reads as
// though it never ran. Such a poll sees phase FAILED with that message and no
// typed outcome, which is the honest answer - the agent cannot know how far the
// dead attempt got.
//
// This no longer decides whether anything is restored; the park directory does.
func (s *NodeAgentServer) failInterruptedStaticPodParkingOperations(ctx context.Context, ids []string) error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	for _, id := range ids {
		op, ok := s.NodeStatus.NodeAgentOperations[id]
		if !ok {
			continue
		}
		op.Phase = NodeAgentOperationPhaseFailed
		// Deliberately does not claim the restore succeeded. Whether it did is
		// in the agent log; this record only says the attempt died.
		op.ErrorMessage = "the agent restarted while this operation was running; " +
			"it did not complete and any static-pod manifests it had parked are restored at startup"
		s.NodeStatus.NodeAgentOperations[id] = op
	}
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()

	slog.WarnContext(ctx, "failing operations left running by an agent restart", "operationIDs", ids)
	if s.Persister == nil {
		return nil
	}
	if err := s.Persister.Persist(&statusCopy); err != nil {
		return fmt.Errorf("persist interrupted operation records: %w", err)
	}
	return nil
}

// parkedStaticPodManifests reports which of the restorable manifests are
// currently sitting in the etcd defragmentation park directory. Missing
// directory means none, which is the normal case and not an error.
func (s *NodeAgentServer) parkedStaticPodManifests() (map[string]bool, error) {
	restores := s.staticPodParkRestores()
	parked := make(map[string]bool, len(restores))
	for _, candidate := range restores {
		path := s.rootedPath(filepath.Join(defaultEtcdDefragParkDir, candidate.manifestName))
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
