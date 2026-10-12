//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"
	"strings"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// JoinStrings carries the two tokens kubeadm produces after init: one for
// joining additional control-plane nodes (carries the certificate-key
// material) and one for joining workers. Returned by GetJoinStrings.
type JoinStrings struct {
	ControlPlane string
	Worker       string
}

// GetJoinStrings asks the init-node's agent for the current join strings.
// Wraps UpdateAndFetchKubeAdmJoinStrings — the agent will refresh the
// kubeadm join token if it's expired before returning the new strings (the
// "UpdateAnd" prefix reflects this). Safe to call any time after init has
// reached phase=initialize.
func GetJoinStrings(ctx context.Context, addr string) (JoinStrings, error) {
	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return JoinStrings{}, err
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, executeRPCTimeout)
	defer cancel()
	resp, err := client.UpdateAndFetchKubeAdmJoinStrings(rpcCtx, &pb.KubeadmJoinStringRequest{})
	if err != nil {
		return JoinStrings{}, fmt.Errorf("cluster.GetJoinStrings: %s: %w", addr, err)
	}
	js := JoinStrings{
		ControlPlane: resp.GetControlplaneJoinString(),
		Worker:       resp.GetWorkerJoinString(),
	}
	if js.ControlPlane == "" || js.Worker == "" {
		return JoinStrings{}, fmt.Errorf("cluster.GetJoinStrings: %s: empty token(s) — controlplane=%q worker=%q",
			addr, js.ControlPlane, js.Worker)
	}
	return js, nil
}

// ExecuteJoin calls ExecuteKubeadmJoin on the agent at addr with the given
// join string (which encodes the discovery endpoint, token, and CA cert
// hash). Like ExecuteInit, this is non-blocking — the request must be
// followed by WaitForPhase(ctx, addr, "join") to confirm the agent's
// background kubeadm goroutine completed.
//
// joinString is one of {JoinStrings.ControlPlane, JoinStrings.Worker} —
// the agent decides control-plane vs worker role from the string itself
// (control-plane variants carry --control-plane and a certificate-key flag
// that workers don't).
func ExecuteJoin(ctx context.Context, addr, joinString string) error {
	if joinString == "" {
		return fmt.Errorf("cluster.ExecuteJoin: joinString required")
	}

	// The deployed node-agent runs `kubeadm <fields(joinString)>` verbatim: its
	// ExecuteKubeadmJoin no longer drops a leading "kubeadm" token (the historical
	// strings.Fields(...)[1:] slice is commented out in agent.go). But its own
	// UpdateAndFetchKubeAdmJoinStrings still returns the full "kubeadm join ..."
	// command. Strip a leading "kubeadm" here so the agent runs `kubeadm join ...`
	// rather than `kubeadm kubeadm join ...` (which fails: unknown command "kubeadm").
	joinString = strings.TrimSpace(joinString)
	if rest, found := strings.CutPrefix(joinString, "kubeadm "); found {
		joinString = strings.TrimSpace(rest)
	}

	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, executeRPCTimeout)
	defer cancel()
	resp, err := client.ExecuteKubeadmJoin(rpcCtx, &pb.KubeadmJoinRequest{
		KubeadmJoinString: joinString,
	})
	if err != nil {
		return fmt.Errorf("cluster.ExecuteJoin: %s: %w", addr, err)
	}
	// Same accept-criterion as ExecuteInit: status MUST be "pending".
	// Anything else (especially "error") = the agent refused the request
	// before kubeadm started.
	if got := resp.GetStatus(); got != "pending" {
		return fmt.Errorf("cluster.ExecuteJoin: %s: status=%q (want pending) errorMessage=%q",
			addr, got, resp.GetErrorMessage())
	}
	return nil
}
