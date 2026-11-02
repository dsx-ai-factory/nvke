// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStopVMProcessesStopsQEMUWhenGroupLeaderExited reproduces vmnet-client
// exiting while its QEMU child survives: the leader PID is gone, so
// stopProcessGroup reports success, but the pidfile QEMU must still be stopped.
func TestStopVMProcessesStopsQEMUWhenGroupLeaderExited(t *testing.T) {
	dir := t.TempDir()
	paths := darwinInstancePaths{
		Dir:                dir,
		PIDFile:            filepath.Join(dir, "qemu.pid"),
		VMNetClientPIDFile: filepath.Join(dir, "vmnet-client.pid"),
		QMPSock:            filepath.Join(dir, "qmp.sock"),
	}
	require.NoError(t, os.WriteFile(paths.QMPSock, nil, 0o644))

	leader := exec.Command("sh", "-c", `sleep 300 >/dev/null 2>&1 & echo $! > "$1"`, "sh", paths.PIDFile)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, leader.Run())
	leaderPID := leader.Process.Pid
	require.NoError(t, writePIDFile(paths.VMNetClientPIDFile, leaderPID))

	qemuPID, err := readPIDFile(paths.PIDFile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(qemuPID, syscall.SIGKILL) })

	alive, _ := isPIDAlive(leaderPID)
	require.False(t, alive, "leader must have exited")
	alive, _ = isPIDAlive(qemuPID)
	require.True(t, alive, "child must survive the leader")

	require.NoError(t, stopVMProcesses(paths))

	require.Eventually(t, func() bool {
		alive, _ := isPIDAlive(qemuPID)
		return !alive
	}, 5*time.Second, 50*time.Millisecond, "QEMU child must be stopped")
	for _, f := range []string{paths.PIDFile, paths.VMNetClientPIDFile, paths.QMPSock} {
		require.NoFileExists(t, f)
	}
}

func TestStopVMProcessesKeepsFilesWhenPIDFileInvalid(t *testing.T) {
	dir := t.TempDir()
	paths := darwinInstancePaths{
		PIDFile:            filepath.Join(dir, "qemu.pid"),
		VMNetClientPIDFile: filepath.Join(dir, "vmnet-client.pid"),
		QMPSock:            filepath.Join(dir, "qmp.sock"),
	}
	require.NoError(t, os.WriteFile(paths.PIDFile, []byte("not-a-pid"), 0o644))
	require.NoError(t, writePIDFile(paths.VMNetClientPIDFile, 0))

	require.Error(t, stopVMProcesses(paths))
	require.FileExists(t, paths.PIDFile)
	require.FileExists(t, paths.VMNetClientPIDFile)
}
