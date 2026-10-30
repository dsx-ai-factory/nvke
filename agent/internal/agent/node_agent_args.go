// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// nodeAgentEnvFilePath is the agent's own environment file, the one the
// controller renders at bootstrap and systemd expands into the command line.
// The old nke-agent spelling is a symlink to it after migrate-agent-paths.sh,
// so writing here reaches both names.
const nodeAgentEnvFilePath = "/etc/default/nvke-agent"

// agentArgsKey is the variable the unit's ExecStart expands.
const agentArgsKey = "AGENT_ARGS"

// maxNodeAgentArgs caps the argument list. The env file is a single line that
// systemd expands, not a place to stage data.
const maxNodeAgentArgs = 256

// ReconfigureNodeAgentArgs replaces AGENT_ARGS in the agent's environment file.
//
// It deliberately does not restart the agent. The arguments are read once at
// startup, so the rewrite takes effect at the next restart, and the response
// says whether that restart is still pending. Restarting here would drop the
// connection this RPC answered on, and that has to be coordinated by the
// caller rather than decided on the node.
func (s *NodeAgentServer) ReconfigureNodeAgentArgs(
	ctx context.Context,
	req *pb.ReconfigureNodeAgentArgsRequest,
) (*pb.ReconfigureNodeAgentArgsResponse, error) {
	defer logRPCSpan(ctx, "ReconfigureNodeAgentArgs")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	args := req.GetArgs()
	if len(args) == 0 {
		return nil, status.Error(codes.InvalidArgument, "args is required; an empty list would strip every argument the agent needs")
	}
	if len(args) > maxNodeAgentArgs {
		return nil, status.Errorf(codes.InvalidArgument, "args has %d entries, more than the %d allowed", len(args), maxNodeAgentArgs)
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, "\n\r") {
			return nil, status.Error(codes.InvalidArgument, "an argument contains a newline, which would split the environment file")
		}
	}

	s.nodeAgentArgsMu.Lock()
	defer s.nodeAgentArgsMu.Unlock()
	// A request that queued behind another one must not change the node after
	// its caller gave up.
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}

	path := s.resolveFilePath(nodeAgentEnvFilePath)
	existing, err := os.ReadFile(path) //nolint:gosec // a fixed path under the agent's own root
	if err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "read %s: %v", nodeAgentEnvFilePath, err)
	}

	updated := replaceAgentArgsLine(string(existing), args)
	outcome := pb.ReconfigureNodeAgentArgsResponse_OUTCOME_UNCHANGED
	if updated != string(existing) {
		// /etc/default exists on any real node; this is for a RootPath-rooted
		// tree, where the atomic write's temporary file needs the directory.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, status.Errorf(codes.Internal, "create directory for %s: %v", nodeAgentEnvFilePath, err)
		}
		if err := atomicWriteFile(path, []byte(updated), 0o644); err != nil {
			return nil, status.Errorf(codes.Internal, "write %s: %v", nodeAgentEnvFilePath, err)
		}
		outcome = pb.ReconfigureNodeAgentArgsResponse_OUTCOME_APPLIED
	}

	running := s.processArgs()
	pending := !slices.Equal(running, args)
	if outcome == pb.ReconfigureNodeAgentArgsResponse_OUTCOME_APPLIED || pending {
		slog.Info("reconfigured node-agent arguments",
			"path", nodeAgentEnvFilePath,
			"outcome", outcome.String(),
			"restart_pending", pending,
		)
	}

	return &pb.ReconfigureNodeAgentArgsResponse{
		Outcome:        outcome,
		RestartPending: pending,
		RunningArgs:    running,
	}, nil
}

// processArgs is the argument list this process started with, which is what the
// environment file held at the last restart rather than what it holds now.
func (s *NodeAgentServer) processArgs() []string {
	if s.runningArgs != nil {
		return slices.Clone(s.runningArgs)
	}
	if len(os.Args) < 2 {
		return nil
	}
	return slices.Clone(os.Args[1:])
}

// replaceAgentArgsLine rewrites the AGENT_ARGS assignment and leaves every
// other line of the environment file alone, so a value an operator or another
// package added survives. The assignment is appended when the file does not
// have one.
func replaceAgentArgsLine(content string, args []string) string {
	rendered := fmt.Sprintf("%s=%s", agentArgsKey, strconv.Quote(strings.Join(args, " ")))

	if strings.TrimSpace(content) == "" {
		return rendered + "\n"
	}

	lines := strings.Split(content, "\n")
	replaced := false
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), agentArgsKey+"=") {
			continue
		}
		if replaced {
			// A second assignment would win at expansion time, so a stale one
			// left behind would silently undo the rewrite.
			lines[i] = ""
			continue
		}
		lines[i] = rendered
		replaced = true
	}
	if !replaced {
		return strings.TrimRight(content, "\n") + "\n" + rendered + "\n"
	}

	out := strings.Join(lines, "\n")
	// Splitting a trailing newline yields an empty final element, so joining
	// restores it; a file that had none still gets one.
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}
