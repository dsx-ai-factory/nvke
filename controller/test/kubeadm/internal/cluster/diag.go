//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"
	"time"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

// RunScript executes an inline diagnostic script on the node-agent at addr and
// returns its combined output. Used for on-failure post-mortem over the agent's
// existing gRPC — no SSH, and it runs while the VM is still alive (before the
// driver deletes it), so nothing leaks on the shared runner.
func RunScript(ctx context.Context, addr, name, contents string) (string, error) {
	out, _, err := RunScriptWithTimeout(ctx, addr, name, contents, 90*time.Second)
	return out, err
}

// RunScriptWithTimeout is RunScript with a caller-chosen RPC budget, and it
// returns the script's exit code separately rather than only embedding it in
// the output string.
//
// Both exist because the two callers want opposite things. A post-mortem is
// bounded work that must not hang, and its exit code is noise — RunScript's
// fixed 90s is right for it. Installing a CNI is neither: it waits on CRDs
// appearing under emulation, well past 90s, and a non-zero exit has to fail the
// test rather than be formatted into a log line nobody reads.
func RunScriptWithTimeout(ctx context.Context, addr, name, contents string, timeout time.Duration) (string, int32, error) {
	conn, client, err := Dial(ctx, addr)
	if err != nil {
		return "", -1, err
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := client.ExecuteScript(rpcCtx, &pb.ExecuteScriptRequest{
		ScriptName:     name,
		ScriptContents: contents,
	})
	if err != nil {
		return "", -1, err
	}
	return fmt.Sprintf("[exit=%d status=%s error=%q]\n%s",
			resp.GetExitCode(), resp.GetStatus(), resp.GetErrorMessage(), resp.GetOutput()),
		resp.GetExitCode(), nil
}
