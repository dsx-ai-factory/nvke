// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	// defaultEtcdHealthTimeout bounds the wait for the local member to report
	// healthy after a restart. It is generous because a restart that crosses the
	// bootstrap-defrag threshold rewrites the whole backend file before etcd
	// starts serving, and that scales with database size rather than being a
	// fixed startup cost.
	defaultEtcdHealthTimeout = 10 * time.Minute

	// defaultEtcdHealthPollInterval is the gap between health probes.
	defaultEtcdHealthPollInterval = 2 * time.Second

	// defaultEtcdCallTimeout bounds a single etcd API call.
	//
	// clientv3 waits for the connection to become ready rather than failing
	// fast, so a call made on a context with no deadline blocks for as long as
	// the member is unreachable. The defragmentation path runs on the operation
	// framework's background context, which has no deadline, so one such call
	// would hang the operation in RUNNING forever while it holds
	// staticPodMutationMu - blocking certificate renewal, kube-vip restore and
	// every other static-pod transaction until the agent process restarted. The
	// outer health-wait deadline does not help, because it is only consulted
	// after a call returns.
	defaultEtcdCallTimeout = 15 * time.Second

	// defaultEtcdExitTimeout is the grace period handed to `crictl stop` for the
	// etcd container: how long etcd has to exit on SIGTERM before it is killed.
	//
	// It is sized against the pod's own termination grace period, which kubeadm
	// leaves at the 30s default, with headroom for a member that is slow to
	// release its backend file. A clean exit matters here beyond the usual
	// reasons - the restart exists to have etcd rewrite that file on the way
	// back up - so the budget errs generous rather than cutting a flush short.
	//
	// It is not the three minutes the manifest-move restart used to need. That
	// budget was mostly kubelet's: the agent parked the manifest and then had
	// to wait for kubelet to notice, bounded by its file-check frequency rather
	// than by anything the agent controlled. Stopping the container addresses
	// it directly, so that part of the wait is gone.
	//
	// It is also the one command in this flow that is expected to take a while,
	// and CommandRunner has no deadline, so this bounds how long a wedged
	// container runtime can hold staticPodMutationMu.
	defaultEtcdExitTimeout = 60 * time.Second
)

func (s *NodeAgentServer) etcdHealthTimeoutOrDefault() time.Duration {
	if s != nil && s.etcdHealthTimeout > 0 {
		return s.etcdHealthTimeout
	}
	return defaultEtcdHealthTimeout
}

func (s *NodeAgentServer) etcdHealthPollIntervalOrDefault() time.Duration {
	if s != nil && s.etcdHealthPollInterval > 0 {
		return s.etcdHealthPollInterval
	}
	return defaultEtcdHealthPollInterval
}

// etcdCallContext derives a context for one etcd API call. Every etcd call in
// these RPCs goes through it, so none can block past its own deadline.
func (s *NodeAgentServer) etcdCallContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.etcdCallTimeoutOrDefault())
}

func (s *NodeAgentServer) etcdCallTimeoutOrDefault() time.Duration {
	if s != nil && s.etcdCallTimeout > 0 {
		return s.etcdCallTimeout
	}
	return defaultEtcdCallTimeout
}

func (s *NodeAgentServer) etcdExitTimeoutOrDefault() time.Duration {
	if s != nil && s.etcdExitTimeout > 0 {
		return s.etcdExitTimeout
	}
	return defaultEtcdExitTimeout
}

// waitForLocalEtcdHealthy polls the local member until it reports healthy, or
// the deadline passes. A freshly restarted etcd refuses connections for a while
// and, when it defragments on the way up, stays unreachable for the length of
// the rewrite, so connection failures are expected and retried rather than
// treated as terminal.
//
// It returns the last successful status so callers can read the post-restart
// backend size without a second round trip.
func (s *NodeAgentServer) waitForLocalEtcdHealthy(
	ctx context.Context,
	client etcdMemberClient,
) (*clientv3.StatusResponse, error) {
	deadline := time.Now().Add(s.etcdHealthTimeoutOrDefault())
	for {
		probeCtx, cancel := s.etcdCallContext(ctx)
		st, err := client.Status(probeCtx, etcdLocalEndpoint)
		cancel()
		lastErr := s.localEtcdHealthError(ctx, client, st, err)
		if lastErr == nil {
			return st, nil
		}

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("etcd did not become healthy within %s: %w", s.etcdHealthTimeoutOrDefault(), lastErr)
		}
		if err := sleepOrDone(ctx, s.etcdHealthPollIntervalOrDefault()); err != nil {
			return nil, err
		}
	}
}
