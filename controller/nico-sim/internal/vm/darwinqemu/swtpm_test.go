// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeFakeExecutable writes an executable shell script at path.
func writeFakeExecutable(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

func TestSwtpmPathsForInstance(t *testing.T) {
	paths := swtpmPathsForInstance("/instances/abc123def456ghi789", "abc123def456ghi789")
	require.Equal(t, "/instances/abc123def456ghi789/tpm", paths.StateDir)
	require.Equal(t, "/instances/abc123def456ghi789/swtpm.pid", paths.PIDFile)
	require.Equal(t, "/instances/abc123def456ghi789/tpm-ek.crt", paths.EKCertPath)
	require.Equal(t, filepath.Join(os.TempDir(), "nvm-tpm-abc123def456.sock"), paths.CtrlSock)
	require.LessOrEqual(t, len(paths.CtrlSock), 104, "macOS sockaddr_un.sun_path is capped at 104 bytes")
}

func TestTpmQEMUArgsDeviceNamePerArch(t *testing.T) {
	paths := swtpmPaths{CtrlSock: "/tmp/x.sock"}
	args := tpmQEMUArgs(context.Background(), "not-invoked-missing-qemu", paths)
	require.Equal(t, []string{
		"-chardev", "socket,id=chrtpm,path=/tmp/x.sock",
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", deviceForArch() + ",tpmdev=tpm0",
	}, args)
}

// deviceForArch mirrors tpmQEMUArgs's arch switch for the assertion above.
// A missing/failing qemu binary means tpmTisHasPPI returns false, so no
// ppi=off is appended regardless of arch.
func deviceForArch() string {
	if goruntime.GOARCH == "arm64" {
		return "tpm-tis-device"
	}
	return "tpm-tis"
}

func TestTpmTisHasPPI(t *testing.T) {
	require.False(t, tpmTisHasPPI(context.Background(), "not-a-real-qemu-binary"))
}

func TestTpmStateSeededReportsUnreadableStateAsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-unreadable")
	require.NoError(t, os.MkdirAll(paths.StateDir, 0o755))
	require.NoError(t, os.Chmod(paths.StateDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(paths.StateDir, 0o755) })

	seeded, err := tpmStateSeeded(paths)
	require.Error(t, err, "an unreadable state dir must not be reported as merely 'unseeded'")
	require.False(t, seeded)
}

func TestEnsureSwtpmStateRollback(t *testing.T) {
	t.Run("swtpm_setup exits 0 without writing state or cert: deleted, clear error", func(t *testing.T) {
		dir := t.TempDir()
		paths := swtpmPathsForInstance(dir, "test-instance-silent-noop")
		fakeSetup := filepath.Join(dir, "swtpm_setup")
		writeFakeExecutable(t, fakeSetup, "exit 0")
		writeFakeExecutable(t, filepath.Join(dir, "swtpm_localca"), "exit 0")
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

		_, err := ensureSwtpmState(context.Background(), fakeSetup, paths)
		require.ErrorContains(t, err, "no EK certificate")
		require.NoDirExists(t, paths.StateDir)
	})

	t.Run("swtpm_setup fails after writing state: half-seeded state deleted so the next attempt reseeds", func(t *testing.T) {
		dir := t.TempDir()
		paths := swtpmPathsForInstance(dir, "test-instance-partial-write")
		fakeSetup := filepath.Join(dir, "swtpm_setup")
		// $3 is the --tpmstate value in our fixed arg order. Simulate a run
		// that wrote real state (e.g. killed by ctx cancellation) but still
		// reports failure.
		writeFakeExecutable(t, fakeSetup, `mkdir -p "$3"; : > "$3/tpm2-00.permall"; exit 1`)
		writeFakeExecutable(t, filepath.Join(dir, "swtpm_localca"), "exit 0")
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

		_, err := ensureSwtpmState(context.Background(), fakeSetup, paths)
		require.ErrorContains(t, err, "swtpm_setup failed")
		require.NoDirExists(t, paths.StateDir)
	})

	t.Run("EK cert write fails after a clean seed: state deleted, safe to retry", func(t *testing.T) {
		dir := t.TempDir()
		paths := swtpmPathsForInstance(dir, "test-instance-cert-write-fails")
		fakeSetup := filepath.Join(dir, "swtpm_setup")
		// $3 is --tpmstate's value, $9 is --write-ek-cert-files' value.
		writeFakeExecutable(t, fakeSetup, `mkdir -p "$3"; : > "$3/tpm2-00.permall"; mkdir -p "$9"; printf fake-der > "$9/ek-rsa2048.crt"; exit 0`)
		writeFakeExecutable(t, filepath.Join(dir, "swtpm_localca"), "exit 0")
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

		// Force the EK cert write to fail by making the destination an
		// existing directory instead of a writable file path.
		require.NoError(t, os.MkdirAll(paths.EKCertPath, 0o755))

		_, err := ensureSwtpmState(context.Background(), fakeSetup, paths)
		require.ErrorContains(t, err, "write EK cert")
		require.NoDirExists(t, paths.StateDir, "nothing has read this cert yet, so it is safe to delete and retry")
	})
}

func TestEnsureSwtpmStateIdempotent(t *testing.T) {
	_, swtpmSetupBin, ok := swtpmAvailable()
	if !ok {
		t.Skip("swtpm_setup not installed; set NICO_VMS_SWTPM_SETUP_PATH to exercise this test")
	}
	if _, err := exec.LookPath("swtpm_localca"); err != nil {
		t.Skip("swtpm_localca not installed")
	}

	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-idempotent")
	seeded, err := tpmStateSeeded(paths)
	require.NoError(t, err)
	require.False(t, seeded)

	cert1, err := ensureSwtpmState(context.Background(), swtpmSetupBin, paths)
	require.NoError(t, err)
	require.NotEmpty(t, cert1)
	seeded, err = tpmStateSeeded(paths)
	require.NoError(t, err)
	require.True(t, seeded)

	// A second call must not reseed: swtpm_setup run twice mints a
	// different EK, which would permanently brick an already-bound FDE
	// guest. Assert the state file's mtime and the cert are unchanged.
	stateFile := paths.StateDir + "/tpm2-00.permall"
	before, statErr := os.Stat(stateFile)
	require.NoError(t, statErr)

	cert2, err := ensureSwtpmState(context.Background(), swtpmSetupBin, paths)
	require.NoError(t, err)
	require.Equal(t, cert1, cert2, "second seed must return the byte-identical cached EK cert")

	after, statErr := os.Stat(stateFile)
	require.NoError(t, statErr)
	require.Equal(t, before.ModTime(), after.ModTime(), "second call must not touch swtpm state")
}

func TestStopSwtpmLeavesStateDirIntact(t *testing.T) {
	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-stop")
	require.NoError(t, os.MkdirAll(paths.StateDir, 0o755))
	require.NoError(t, os.WriteFile(paths.StateDir+"/tpm2-00.permall", []byte("state"), 0o644))
	// No PID file: stopSwtpm on an instance with nothing running must be a
	// clean no-op, not an error.
	require.NoError(t, stopSwtpm(paths))

	_, err := os.Stat(paths.StateDir + "/tpm2-00.permall")
	require.NoError(t, err, "stopSwtpm must never remove TPM state; only Delete does, via the instance dir cleanup")
}

func TestStartSwtpmReusesLiveReachableDaemon(t *testing.T) {
	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-reuse")
	// Our own test process is definitely alive; reuse it as a stand-in PID.
	require.NoError(t, writePIDFile(paths.PIDFile, os.Getpid()))
	require.NoError(t, os.WriteFile(paths.CtrlSock, []byte{}, 0o644))

	// A binary that would fail loudly if actually executed proves the reuse
	// path returns before ever invoking swtpmBin.
	err := startSwtpm(context.Background(), "/bin/nonexistent-swtpm-binary", paths)
	require.NoError(t, err)
	require.FileExists(t, paths.PIDFile, "the existing PID file must be left untouched, not removed and re-created")
}

func TestStartSwtpmThenStop(t *testing.T) {
	swtpmBin, swtpmSetupBin, ok := swtpmAvailable()
	if !ok {
		t.Skip("swtpm not installed; set NICO_VMS_SWTPM_PATH to exercise this test")
	}
	if _, err := exec.LookPath("swtpm_localca"); err != nil {
		t.Skip("swtpm_localca not installed")
	}

	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-start-stop")

	_, err := ensureSwtpmState(context.Background(), swtpmSetupBin, paths)
	require.NoError(t, err)

	require.NoError(t, startSwtpm(context.Background(), swtpmBin, paths))
	require.FileExists(t, paths.CtrlSock)
	require.FileExists(t, paths.PIDFile)

	require.NoError(t, stopSwtpm(paths))
	require.NoFileExists(t, paths.PIDFile)
	require.NoFileExists(t, paths.CtrlSock)
	// State survives the stop — it is only ever removed by the caller
	// deleting the whole instance directory.
	seeded, err := tpmStateSeeded(paths)
	require.NoError(t, err)
	require.True(t, seeded)
}

func TestSwtpmAvailableRespectsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fakeSwtpm := filepath.Join(dir, "custom-swtpm")
	fakeSwtpmSetup := filepath.Join(dir, "custom-swtpm-setup")
	writeFakeExecutable(t, fakeSwtpm, "exit 0")
	writeFakeExecutable(t, fakeSwtpmSetup, "exit 0")

	t.Setenv("NICO_VMS_SWTPM_PATH", fakeSwtpm)
	t.Setenv("NICO_VMS_SWTPM_SETUP_PATH", fakeSwtpmSetup)
	swtpmBin, swtpmSetupBin, ok := swtpmAvailable()
	require.True(t, ok)
	require.Equal(t, fakeSwtpm, swtpmBin)
	require.Equal(t, fakeSwtpmSetup, swtpmSetupBin)
}

func TestResolveSwtpmLocalCAPrefersSiblingOverPath(t *testing.T) {
	setupDir := t.TempDir()
	fakeSetup := filepath.Join(setupDir, "swtpm_setup")
	writeFakeExecutable(t, fakeSetup, "exit 0")
	siblingLocalCA := filepath.Join(setupDir, "swtpm_localca")
	writeFakeExecutable(t, siblingLocalCA, "exit 0")

	// A decoy earlier on PATH must lose to the sibling next to swtpmSetupBin.
	decoyDir := t.TempDir()
	writeFakeExecutable(t, filepath.Join(decoyDir, "swtpm_localca"), "exit 0")
	t.Setenv("PATH", decoyDir+":"+os.Getenv("PATH"))

	got, err := resolveSwtpmLocalCA(fakeSetup)
	require.NoError(t, err)
	require.Equal(t, siblingLocalCA, got)
}

func TestResolveSwtpmLocalCAFallsBackToPath(t *testing.T) {
	setupDir := t.TempDir()
	fakeSetup := filepath.Join(setupDir, "swtpm_setup")
	writeFakeExecutable(t, fakeSetup, "exit 0")
	// No sibling swtpm_localca in setupDir.

	pathDir := t.TempDir()
	pathLocalCA := filepath.Join(pathDir, "swtpm_localca")
	writeFakeExecutable(t, pathLocalCA, "exit 0")
	t.Setenv("PATH", pathDir+":"+os.Getenv("PATH"))

	got, err := resolveSwtpmLocalCA(fakeSetup)
	require.NoError(t, err)
	require.Equal(t, pathLocalCA, got)
}
