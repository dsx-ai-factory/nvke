//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cluster drives node-agent's gRPC kubeadm RPCs.
//
// Step 2 scope: ExecuteKubeadmInit + GetKubeadmStatus polling. Joins and
// InstallCNI land in Step 3.
//
// This is a direct port of the bash polling logic in
// nke/node-agent/scripts/grpcurl-kubeadm-cni-test.sh (wait_for_async_kubeadm)
// — the manual procedure Phase 4 automates. Keep the polling semantics in
// sync with that script: a phase is "done" only when
//
//	GetKubeadmStatus.kubeadmStatus == "ready"  AND
//	GetKubeadmStatus.kubeadmPhase  == <want>   AND
//	<want> ∈ GetKubeadmStatus.kubeadmCompletedPhases
//
// `kubeadmStatus == "error"` is treated as a fast-fail.
package cluster

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// Plaintext default — VMs run nvke-agent with AGENT_ARGS=--disable-mtls per
// the cloud-init drop-in. Production never uses this path.
const DefaultAgentPort = 50051

// Per-RPC timeouts. The agent's Execute*/Get* RPCs are non-blocking (kubeadm
// runs in a background goroutine, see grpcurl-kubeadm-cni-test.sh header),
// so individual calls should always return in well under a second. Generous
// caps here just guard against network flakes or a stuck agent — we'd
// rather fail an iteration and retry on the next poll than hang the whole
// driver waiting for the outer ctx.
const (
	executeRPCTimeout = 30 * time.Second
	statusRPCTimeout  = 15 * time.Second
)

// Dial opens a plaintext gRPC connection to a node-agent at ip:50051.
// Caller closes the returned conn.
func Dial(ctx context.Context, addr string) (*grpc.ClientConn, pb.NodeAgentClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("cluster.Dial: %s: %w", addr, err)
	}
	return conn, pb.NewNodeAgentClient(conn), nil
}

// InitRequest is the small set of init knobs the test exposes. The plan
// defaults match the manual grpcurl script (10.244.0.0/16 / 10.96.0.0/12)
// — these are kubeadm conventions and don't overlap with forge-vms's
// 192.168.200.0/24 bridge subnet.
type InitRequest struct {
	PodNetworkCIDR       string // default: 10.244.0.0/16
	ServiceCIDR          string // default: 10.96.0.0/12
	ControlPlaneEndpoint string // required, e.g. "192.168.200.21:6443"
}

func (r InitRequest) withDefaults() InitRequest {
	if r.PodNetworkCIDR == "" {
		r.PodNetworkCIDR = "10.244.0.0/16"
	}
	if r.ServiceCIDR == "" {
		r.ServiceCIDR = "10.96.0.0/12"
	}
	return r
}

// ExecuteInit calls ExecuteKubeadmInit on the agent at addr and verifies
// the request was accepted (status="pending"). It does NOT wait for kubeadm
// to finish — use WaitForPhase(ctx, addr, "initialize") next.
//
// noCniInstall is always true: the test installs CNI via InstallCNI after
// init (matches the modern path; mirrors the grpcurl script default).
func ExecuteInit(ctx context.Context, addr string, req InitRequest) error {
	if req.ControlPlaneEndpoint == "" {
		return fmt.Errorf("cluster.ExecuteInit: ControlPlaneEndpoint required")
	}
	req = req.withDefaults()

	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, executeRPCTimeout)
	defer cancel()
	resp, err := client.ExecuteKubeadmInit(rpcCtx, &pb.KubeadmInitRequest{
		PodNetworkCidr:       req.PodNetworkCIDR,
		ServiceCidr:          req.ServiceCIDR,
		ControlPlaneEndpoint: req.ControlPlaneEndpoint,
		NoCniInstall:         true,
	})
	if err != nil {
		return fmt.Errorf("cluster.ExecuteInit: %s: %w", addr, err)
	}
	// Same assertion as grpcurl script's assert_execute_kubeadm_init_response:
	// status MUST be "pending" — anything else (esp. "error") is a fatal
	// rejection BEFORE kubeadm even starts.
	if got := resp.GetStatus(); got != "pending" {
		return fmt.Errorf("cluster.ExecuteInit: %s: status=%q (want pending) errorMessage=%q",
			addr, got, resp.GetErrorMessage())
	}
	return nil
}

// WaitForPhase polls GetKubeadmStatus on the agent until kubeadm reports
// the target phase as completed, or fails with status="error".
//
// timeout matches the bash script's 900s default. pollInterval defaults
// to 5s.
func WaitForPhase(ctx context.Context, addr, phase string, timeout, pollInterval time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	for {
		// Per-iteration timeout — don't defer across iterations, otherwise the
		// timers accumulate until the for-loop returns.
		rpcCtx, cancel := context.WithTimeout(ctx, statusRPCTimeout)
		resp, err := client.GetKubeadmStatus(rpcCtx, &pb.KubeadmStatusRequest{})
		cancel()
		if err == nil {
			st := resp.GetKubeadmStatus()
			ph := resp.GetKubeadmPhase()
			msg := resp.GetKubeadmCurrentMessage()
			completed := resp.GetKubeadmCompletedPhases()
			switch st {
			case "error":
				return fmt.Errorf("cluster.WaitForPhase: %s: kubeadm reported error (phase=%s, message=%s)",
					addr, ph, msg)
			case "ready":
				if ph == phase && slices.Contains(completed, phase) {
					return nil
				}
				// Bogus state: status=ready + phase matches but the phase
				// isn't in the completed list. Same defensive check as the
				// bash script — surfaces an agent-side bookkeeping bug.
				if ph == phase {
					return fmt.Errorf("cluster.WaitForPhase: %s: status=ready phase=%s but phase not in completedPhases=%v",
						addr, phase, completed)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cluster.WaitForPhase: %s: timed out after %s waiting for phase=%s",
				addr, timeout, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// WaitForAgent polls the node-agent until it answers a GetKubeadmStatus RPC —
// i.e. until nvke-agent is installed and serving. Needed when VM readiness is
// gated on SSH (VM booted) instead of cloud-init phone_home: SSH comes up early
// in boot, so the agent may not be installed yet when the VM first becomes
// reachable. Polling here bridges the gap before we drive ExecuteInit.
func WaitForAgent(ctx context.Context, addr string, timeout, pollInterval time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	for {
		rpcCtx, cancel := context.WithTimeout(ctx, statusRPCTimeout)
		_, err := client.GetKubeadmStatus(rpcCtx, &pb.KubeadmStatusRequest{})
		cancel()
		if err == nil {
			return nil // agent is up and responding
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cluster.WaitForAgent: %s: agent not reachable after %s: %w", addr, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
