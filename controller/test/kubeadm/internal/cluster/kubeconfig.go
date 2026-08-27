//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

// FetchKubeconfig calls the node-agent's GetKubeconfig RPC and returns the
// admin kubeconfig as a string. Call this only on a control-plane node
// (the init node is the natural choice — addr should be vm-1's
// node-agent endpoint).
//
// Rationale for using gRPC instead of SSH + scp /etc/kubernetes/admin.conf:
// the plaintext gRPC channel is already established for the kubeadm RPCs;
// adding it here avoids pulling in golang.org/x/crypto/ssh and managing a
// second authentication path.
func FetchKubeconfig(ctx context.Context, addr string) (string, error) {
	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, executeRPCTimeout)
	defer cancel()
	resp, err := client.GetKubeconfig(rpcCtx, &pb.KubeconfigRequest{})
	if err != nil {
		return "", fmt.Errorf("cluster.FetchKubeconfig: %s: %w", addr, err)
	}
	if msg := resp.GetErrorMessage(); msg != "" {
		return "", fmt.Errorf("cluster.FetchKubeconfig: %s: agent reported error: %s", addr, msg)
	}
	kc := resp.GetKubeconfig()
	if kc == "" {
		return "", fmt.Errorf("cluster.FetchKubeconfig: %s: empty kubeconfig in response", addr)
	}
	return kc, nil
}
