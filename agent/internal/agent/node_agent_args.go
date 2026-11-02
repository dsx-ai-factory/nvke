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
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// nodeAgentEnvFilePath is the agent's own environment file, the one the
// controller renders at bootstrap and systemd expands into the command line.
// The old nke-agent spelling is a symlink to it after migrate-agent-paths.sh,
// so writing here reaches both names.
const nodeAgentEnvFilePath = "/etc/default/nvke-agent"

// legacyNodeAgentEnvFilePath is the pre-rename spelling. migrate-agent-paths.sh
// turns it into a symlink to nodeAgentEnvFilePath, so on a migrated node the two
// names are the same file.
const legacyNodeAgentEnvFilePath = "/etc/default/nke-agent"

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
		// The whole list is one value that systemd re-splits on whitespace, so
		// an argument carrying any would reach the agent as several and the
		// running list could never match what was asked for.
		if arg == "" || strings.ContainsFunc(arg, unicode.IsSpace) {
			return nil, status.Error(codes.InvalidArgument, "an argument is empty or contains whitespace, which systemd would split into separate arguments")
		}
	}
	if s.ValidateArgs != nil {
		if err := s.ValidateArgs(args); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "args do not parse as agent flags: %v", err)
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
	if err := s.checkLegacyEnvFile(path); err != nil {
		return nil, err
	}
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
		// A bootstrap profile may have written the file stricter than 0644;
		// rewriting its contents is not a reason to widen it.
		perm := os.FileMode(0o644)
		if info, statErr := os.Stat(path); statErr == nil {
			perm = info.Mode().Perm()
		} else if !os.IsNotExist(statErr) {
			return nil, status.Errorf(codes.Internal, "stat %s: %v", nodeAgentEnvFilePath, statErr)
		}
		if err := atomicWriteFile(path, []byte(updated), perm); err != nil {
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

// checkLegacyEnvFile refuses the rewrite when the pre-rename environment file
// is still a regular file of its own rather than a symlink to the current one.
// Writing would leave the node with two independent files, and the next package
// upgrade's migrate-agent-paths.sh refuses to choose between them.
func (s *NodeAgentServer) checkLegacyEnvFile(path string) error {
	legacy, err := os.Lstat(s.resolveFilePath(legacyNodeAgentEnvFilePath))
	if err == nil && legacy.Mode().IsRegular() {
		// A migrated node reaches the same inode through both names, so only a
		// legacy file of its own is a collision.
		current, statErr := os.Stat(path)
		if statErr != nil || !os.SameFile(current, legacy) {
			return status.Errorf(
				codes.FailedPrecondition,
				"%s is a separate regular file; install the agent package so it becomes a symlink to %s before reconfiguring arguments",
				legacyNodeAgentEnvFilePath,
				nodeAgentEnvFilePath,
			)
		}
	}
	return nil
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
