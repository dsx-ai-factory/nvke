// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestartPolicy(t *testing.T) {
	tests := []struct {
		spec        string
		code        int
		wantRestart bool
		wantLimit   int
	}{
		{spec: "", code: 1},
		{spec: "no", code: 1},
		{spec: "always", code: 0, wantRestart: true},
		{spec: "unless-stopped", code: 0, wantRestart: true},
		{spec: "on-failure", code: 0},
		{spec: "on-failure", code: 2, wantRestart: true},
		{spec: "on-failure:3", code: 2, wantRestart: true, wantLimit: 3},
		{spec: "on-failure:1", code: 0, wantLimit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.spec+" after "+string(rune('0'+tt.code)), func(t *testing.T) {
			restart, limit := restartPolicy(tt.spec, tt.code)
			assert.Equal(t, tt.wantRestart, restart)
			assert.Equal(t, tt.wantLimit, limit)
		})
	}
}

// startedWithPolicy is a harness with a running container named c created with --restart POLICY.
func startedWithPolicy(t *testing.T, policy string) (*harness, string) {
	t.Helper()
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pseudo-terminals here")
	}
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/library/img:latest application/vnd.oci.image.manifest.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	require.Equal(t, 0, h.run("run", "-d", "--name", "c", "--network", "kind", "--restart", policy, "--entrypoint", "/bin/sh", "img", "-c", "sleep"), h.stderr.String())
	return h, strings.TrimSpace(h.stdout.String())
}

// supervisorStarts counts the supervisors the engine started: the first start, and each restart.
func (h *harness) supervisorStarts() int { return len(h.callsMatching("supervisor ")) }

// endTask is what happens to a task that exits while its supervisor reads the console: ctr's list no longer has it.
func (h *harness) endTask() { h.setFile("tasks", "") }

func TestRestartAfterTheTaskEnds(t *testing.T) {
	tests := []struct {
		name         string
		policy       string
		exitStatus   string
		wantRestarts int
	}{
		{name: "no policy leaves it stopped", policy: "no", exitStatus: "0"},
		{name: "always restarts after a clean exit, as a reboot from inside the node ends", policy: "always", exitStatus: "0", wantRestarts: 1},
		{name: "unless-stopped restarts after a failure", policy: "unless-stopped", exitStatus: "3", wantRestarts: 1},
		{name: "on-failure leaves a clean exit", policy: "on-failure", exitStatus: "0"},
		{name: "on-failure restarts a failure", policy: "on-failure", exitStatus: "3", wantRestarts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("START_STATUS", tt.exitStatus)
			h, id := startedWithPolicy(t, tt.policy)
			require.Equal(t, 1, h.supervisorStarts())

			h.endTask()
			require.Equal(t, 0, h.run("_supervise", id), h.stderr.String())
			assert.Equal(t, 1+tt.wantRestarts, h.supervisorStarts())
			assert.Len(t, h.callsMatching("bridge ADD"), 1+tt.wantRestarts, "a restarted container is attached to its network again")
			if tt.wantRestarts > 0 {
				require.Equal(t, 0, h.run("inspect", "-f", "{{.State.Running}} {{(index .NetworkSettings.Networks \"kind\").IPAddress}}", id))
				assert.Equal(t, "true 10.4.0.5\n", h.stdout.String())
			}
		})
	}
}

func TestRestartLimit(t *testing.T) {
	t.Setenv("START_STATUS", "3")
	h, id := startedWithPolicy(t, "on-failure:2")
	for want := 2; want <= 3; want++ {
		h.endTask()
		require.Equal(t, 0, h.run("_supervise", id), h.stderr.String())
		assert.Equal(t, want, h.supervisorStarts(), "restart %d of 2", want-1)
	}
	h.endTask()
	require.Equal(t, 0, h.run("_supervise", id), h.stderr.String())
	assert.Equal(t, 3, h.supervisorStarts(), "the third failure is not restarted")

	var log bytes.Buffer
	h.eng.restartPerPolicy(id, 3, 11*time.Second, &log)
	assert.Equal(t, 4, h.supervisorStarts(), "a task that ran for a while starts the count again")
	assert.Empty(t, log.String())

	require.Equal(t, 0, h.run("start", id), "start of a running container")
	require.Equal(t, 0, h.run("stop", "--time", "0", id))
	require.Equal(t, 0, h.run("start", id))
	assert.NoFileExists(t, h.eng.containerDir(id)+"/restarts", "a start by the user resets the count")
}

func TestStopKeepsAContainerStopped(t *testing.T) {
	t.Setenv("START_STATUS", "0")
	h, id := startedWithPolicy(t, "always")

	require.Equal(t, 0, h.run("stop", "--time", "0", id), h.stderr.String())
	require.Equal(t, 0, h.run("_supervise", id), "the supervisor sees the task end")
	assert.Equal(t, 1, h.supervisorStarts(), "stop is not undone by the policy")

	require.Equal(t, 0, h.run("start", id), h.stderr.String())
	assert.Equal(t, 2, h.supervisorStarts())
	h.endTask()
	require.Equal(t, 0, h.run("_supervise", id), h.stderr.String())
	assert.Equal(t, 3, h.supervisorStarts(), "a start by the user hands the container back to the policy")

	h.endTask()
	require.Equal(t, 0, h.run("restart", "--time", "0", id), h.stderr.String())
	assert.Equal(t, 4, h.supervisorStarts(), "restart is a stop and a start by the user")
}

func TestAStopDuringTheWaitWins(t *testing.T) {
	t.Setenv("START_STATUS", "0")
	h, id := startedWithPolicy(t, "always")
	h.endTask()
	h.eng.cfg.RestartDelay = 300 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.eng.restartPerPolicy(id, 0, 0, &bytes.Buffer{})
	}()
	time.Sleep(50 * time.Millisecond)
	h.eng.markStopped(id)
	<-done
	assert.Equal(t, 1, h.supervisorStarts())
}

func TestTheExitIsRecordedWithoutARestart(t *testing.T) {
	t.Setenv("START_STATUS", "7")
	h, id := startedWithPolicy(t, "no")
	h.endTask()
	require.Equal(t, 0, h.run("_supervise", id), h.stderr.String())

	meta, err := h.eng.loadContainer(id)
	require.NoError(t, err)
	assert.Equal(t, 7, meta.ExitCode)
	assert.False(t, meta.FinishedAt.IsZero())
	assert.Empty(t, meta.IPv4, "an address that is no longer used is not left in the peers' hosts files")
	assert.Nil(t, meta.CNIResult)
	assert.Len(t, h.callsMatching("bridge DEL"), 2)

	require.Equal(t, 0, h.run("wait", id), h.stderr.String())
	assert.Equal(t, "7\n", h.stdout.String())
}

func TestAnOldSupervisorLeavesALaterStartAlone(t *testing.T) {
	t.Setenv("START_STATUS", "0")
	h, id := startedWithPolicy(t, "always")
	old, err := h.eng.loadContainer(id)
	require.NoError(t, err)

	h.endTask()
	require.Equal(t, 0, h.run("start", id), h.stderr.String())
	require.Equal(t, 2, h.supervisorStarts())

	assert.False(t, h.eng.finishTask(id, old.Generation, 0), "the container belongs to the later start")
	current, err := h.eng.loadContainer(id)
	require.NoError(t, err)
	assert.Equal(t, "10.4.0.5", current.IPv4, "its network is still attached")
	assert.NotNil(t, current.CNIResult)

	h.endTask()
	require.ErrorIs(t, h.eng.startContainer(old, true), errStoppedByUser)
	assert.Equal(t, 2, h.supervisorStarts(), "the restart it had waited for is not made")
}
