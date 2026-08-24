// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/agent/pkg/utils"
)

type rebootTestRunner struct {
	mu            sync.Mutex
	lockResults   []error
	journalOutput []byte
	journalErr    error
	detachedName  string
	detachedErr   error
	fuserCalls    int
}

func (r *rebootTestRunner) Run(name string, _ []string, _ *nkeutils.CommandOptions) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch name {
	case "fuser":
		resultIndex := r.fuserCalls
		r.fuserCalls++
		if resultIndex >= len(r.lockResults) {
			return nil, errors.New("no lock holder")
		}
		return nil, r.lockResults[resultIndex]
	case "journalctl":
		return r.journalOutput, r.journalErr
	default:
		return nil, errors.New("unexpected command")
	}
}

func (r *rebootTestRunner) RunSplitOutput(name string, args []string, opts *nkeutils.CommandOptions) (nkeutils.CommandOutput, error) {
	output, err := r.Run(name, args, opts)
	return nkeutils.CommandOutput{Stdout: output}, err
}

func (r *rebootTestRunner) RunDetached(name string, _ []string, _ *nkeutils.CommandOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detachedName = name
	return r.detachedErr
}

func TestScheduleNodeReboot(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		lockResults []error
		detachedErr error
		wantTimeout bool
		wantErr     string
		wantCalls   int
	}{
		"reboots immediately when locks are clear": {
			lockResults: []error{errors.New("no lock holder")},
			wantCalls:   1,
		},
		"waits for locks to clear": {
			lockResults: []error{nil, errors.New("no lock holder")},
			wantCalls:   2,
		},
		"reboots after lock wait timeout": {
			lockResults: []error{nil, nil, nil},
			wantTimeout: true,
			wantCalls:   3,
		},
		"reports detached reboot failure": {
			lockResults: []error{errors.New("no lock holder")},
			detachedErr: errors.New("reboot unavailable"),
			wantErr:     "failed to schedule node reboot",
			wantCalls:   1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := &rebootTestRunner{lockResults: tt.lockResults, detachedErr: tt.detachedErr}
			server := &NodeAgentServer{
				Runner:                 runner,
				rebootLockPollInterval: time.Millisecond,
				rebootLockWaitTimeout:  2 * time.Millisecond,
			}

			response, err := server.ScheduleNodeReboot(context.Background(), &pb.ScheduleNodeRebootRequest{})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantTimeout, response.GetPackageLocksTimedOut())
			require.Equal(t, "reboot", runner.detachedName)
			require.Equal(t, tt.wantCalls, runner.fuserCalls)
		})
	}
}

func TestScheduleNodeRebootHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	runner := &rebootTestRunner{lockResults: []error{nil}}
	server := &NodeAgentServer{
		Runner:                 runner,
		rebootLockPollInterval: time.Hour,
		rebootLockWaitTimeout:  2 * time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := server.ScheduleNodeReboot(ctx, &pb.ScheduleNodeRebootRequest{})
	require.Error(t, err)
	require.Empty(t, runner.detachedName)
}

func TestGetNodeBootInfoUsesJournal(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	bootIDPath := filepath.Join(rootPath, "proc/sys/kernel/random/boot_id")
	require.NoError(t, os.MkdirAll(filepath.Dir(bootIDPath), 0o755))
	require.NoError(t, os.WriteFile(bootIDPath, []byte("proc-boot-id\n"), 0o600))
	runner := &rebootTestRunner{journalOutput: []byte(" 0 journal-boot-id Tue 2026-08-18 14:30:45 UTC—Tue 2026-08-18 15:00:00 UTC\n")}
	server := &NodeAgentServer{Runner: runner, RootPath: rootPath}

	response, err := server.GetNodeBootInfo(context.Background(), &pb.GetNodeBootInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, time.August, 18, 14, 30, 45, 0, time.UTC).Unix(), response.GetBootTimeUnixSeconds())
	require.Equal(t, "proc-boot-id", response.GetBootId())
}

func TestGetNodeBootInfoFallsBackToProcUptime(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	uptimePath := filepath.Join(rootPath, "proc/uptime")
	require.NoError(t, os.MkdirAll(filepath.Dir(uptimePath), 0o755))
	require.NoError(t, os.WriteFile(uptimePath, []byte("125.75 200.00\n"), 0o600))
	now := time.Date(2026, time.August, 18, 14, 30, 45, 0, time.UTC)
	server := &NodeAgentServer{
		Runner:   &rebootTestRunner{journalErr: errors.New("journal unavailable")},
		RootPath: rootPath,
		now:      func() time.Time { return now },
	}

	response, err := server.GetNodeBootInfo(context.Background(), &pb.GetNodeBootInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, now.Add(-125*time.Second).Unix(), response.GetBootTimeUnixSeconds())
	require.Empty(t, response.GetBootId())
}
