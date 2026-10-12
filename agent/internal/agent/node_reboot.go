// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	defaultPackageLockPollInterval = 5 * time.Second
	defaultPackageLockWaitTimeout  = 5 * time.Minute
	procUptimePath                 = "/proc/uptime"
	procBootIDPath                 = "/proc/sys/kernel/random/boot_id"
)

var aptDPKGLockPaths = []string{
	"/var/lib/dpkg/lock-frontend",
	"/var/lib/dpkg/lock",
	"/var/cache/apt/archives/lock",
}

// ScheduleNodeReboot waits for package-manager locks, then starts a detached
// reboot command so the RPC does not depend on the connection surviving reboot.
func (s *NodeAgentServer) ScheduleNodeReboot(ctx context.Context, req *pb.ScheduleNodeRebootRequest) (*pb.ScheduleNodeRebootResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}

	pollInterval := s.rebootLockPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPackageLockPollInterval
	}
	waitTimeout := s.rebootLockWaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = defaultPackageLockWaitTimeout
	}

	timedOut, err := s.waitForPackageLocks(ctx, pollInterval, waitTimeout)
	if err != nil {
		return nil, err
	}
	if timedOut {
		slog.Warn("package-manager locks did not clear before reboot timeout", "timeout", waitTimeout)
	}

	if err := s.Runner.RunDetached("reboot", nil, nil); err != nil {
		slog.Error("failed to schedule node reboot", "err", err)
		return nil, status.Errorf(codes.Internal, "failed to schedule node reboot: %v", err)
	}

	return &pb.ScheduleNodeRebootResponse{PackageLocksTimedOut: timedOut}, nil
}

func (s *NodeAgentServer) waitForPackageLocks(ctx context.Context, pollInterval, waitTimeout time.Duration) (bool, error) {
	elapsed := time.Duration(0)
	for {
		if !s.packageLocksHeld() {
			// fuser exits non-zero both when no files are in use and when fuser is
			// unavailable. In either case there is nothing useful to wait for.
			return false, nil
		}
		if elapsed >= waitTimeout {
			return true, nil
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false, status.FromContextError(ctx.Err()).Err()
		case <-timer.C:
		}
		elapsed += pollInterval
	}
}

func (s *NodeAgentServer) packageLocksHeld() bool {
	_, err := s.Runner.Run("fuser", aptDPKGLockPaths, nil)
	return err == nil
}

// GetNodeBootInfo returns the current boot time and Linux boot ID without
// exposing command output parsing to callers.
func (s *NodeAgentServer) GetNodeBootInfo(_ context.Context, req *pb.GetNodeBootInfoRequest) (*pb.GetNodeBootInfoResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}

	bootTime, journalBootID, err := s.bootTimeFromJournal()
	if err != nil {
		bootTime, err = s.bootTimeFromProc()
		if err != nil {
			slog.Error("failed to determine node boot time", "err", err)
			return nil, status.Errorf(codes.Internal, "failed to determine node boot time: %v", err)
		}
	}

	bootID := journalBootID
	if rawBootID, readErr := os.ReadFile(s.rootedPath(procBootIDPath)); readErr == nil {
		bootID = strings.TrimSpace(string(rawBootID))
	}

	return &pb.GetNodeBootInfoResponse{
		BootTimeUnixSeconds: bootTime.UTC().Unix(),
		BootId:              bootID,
	}, nil
}

func (s *NodeAgentServer) bootTimeFromJournal() (time.Time, string, error) {
	output, err := s.Runner.Run("journalctl", []string{"--utc", "--list-boots", "--no-pager"}, nil)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("list journal boots: %w", err)
	}

	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "0" {
			continue
		}

		bootTime, parseErr := time.ParseInLocation("2006-01-02 15:04:05", fields[3]+" "+fields[4], time.UTC)
		if parseErr != nil {
			return time.Time{}, "", fmt.Errorf("parse current journal boot time: %w", parseErr)
		}
		return bootTime, fields[1], nil
	}

	return time.Time{}, "", fmt.Errorf("current boot is absent from journal output")
}

func (s *NodeAgentServer) bootTimeFromProc() (time.Time, error) {
	rawUptime, err := os.ReadFile(s.rootedPath(procUptimePath))
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s: %w", procUptimePath, err)
	}

	fields := strings.Fields(string(rawUptime))
	if len(fields) == 0 {
		return time.Time{}, fmt.Errorf("parse %s: file is empty", procUptimePath)
	}
	uptimeSeconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s uptime %q: %w", procUptimePath, fields[0], err)
	}

	now := time.Now
	if s.now != nil {
		now = s.now
	}
	return time.Unix(now().Unix()-int64(uptimeSeconds), 0).UTC(), nil
}
