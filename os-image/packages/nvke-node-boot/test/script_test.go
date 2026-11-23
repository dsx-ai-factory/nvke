// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkenodeboot_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configureConsole is the script the package installs, run here against a
// temporary drop-in directory and a stand-in for update-grub.
var configureConsole = filepath.Join(filesDir, "configure-console.sh")

// run invokes the script and returns its combined output and exit code. The
// stand-in update-grub appends a line to grubLog, so a test can see it ran.
func run(t *testing.T, dir, grubLog string, args ...string) (string, int) {
	t.Helper()
	grub := filepath.Join(t.TempDir(), "update-grub")
	require.NoError(t, os.WriteFile(grub, []byte("#!/bin/sh\necho ran >>\""+grubLog+"\"\n"), 0o755))
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{configureConsole}, args...)...)
	cmd.Env = append(os.Environ(), "NVKE_GRUB_DROPIN_DIR="+dir, "NVKE_UPDATE_GRUB="+grub)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "script did not run")
	return string(out), exitErr.ExitCode()
}

func TestConfigureConsoleWritesDropIn(t *testing.T) {
	for _, tty := range []string{"ttyS0", "ttyS1", "ttyAMA0"} {
		t.Run(tty, func(t *testing.T) {
			dir := t.TempDir()
			grubLog := filepath.Join(t.TempDir(), "grub.log")

			out, code := run(t, dir, grubLog, tty)
			require.Equal(t, 0, code, out)

			got, err := os.ReadFile(filepath.Join(dir, "50-nvke-console.cfg"))
			require.NoError(t, err)
			assert.Contains(t, string(got), "GRUB_CMDLINE_LINUX=\"console=tty0 console="+tty+",115200\"\n")
			info, err := os.Stat(filepath.Join(dir, "50-nvke-console.cfg"))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

			ran, err := os.ReadFile(grubLog)
			require.NoError(t, err)
			assert.Equal(t, "ran\n", string(ran), "update-grub runs once")

			// No temporary file is left behind.
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		})
	}
}

// TestConfigureConsoleRejectsAnythingButADeviceName is the check that keeps a
// value from reaching grub's shell-sourced configuration.
func TestConfigureConsoleRejectsAnythingButADeviceName(t *testing.T) {
	for name, args := range map[string][]string{
		"empty":           {""},
		"no argument":     {},
		"two arguments":   {"ttyS0", "ttyS1"},
		"path":            {"../../etc/passwd"},
		"space":           {"tty S0"},
		"quote":           {`ttyS0"`},
		"semicolon":       {"ttyS0; rm -rf /"},
		"substitution":    {"$(id)"},
		"newline":         {"ttyS0\nGRUB_TIMEOUT=0"},
		"not a tty":       {"ttS0"},
		"trailing comma":  {"ttyS0,115200"},
		"leading space":   {" ttyS0"},
		"absolute device": {"/dev/ttyS0"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			grubLog := filepath.Join(t.TempDir(), "grub.log")

			out, code := run(t, dir, grubLog, args...)
			assert.Equal(t, 2, code, out)
			assert.NoFileExists(t, filepath.Join(dir, "50-nvke-console.cfg"))
			assert.NoFileExists(t, grubLog, "update-grub must not run for a rejected value")
		})
	}
}

func TestConfigureConsoleIsIdempotentAndChangesDevice(t *testing.T) {
	dir := t.TempDir()
	grubLog := filepath.Join(t.TempDir(), "grub.log")

	for _, tty := range []string{"ttyS0", "ttyS0", "ttyS1"} {
		out, code := run(t, dir, grubLog, tty)
		require.Equal(t, 0, code, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "50-nvke-console.cfg"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "console=ttyS1,115200", "a later run replaces the device")
	assert.NotContains(t, string(got), "ttyS0")
}

// TestConfigureConsoleRetiresTheLateCommandFile covers an image built before the
// package: the late-command's 99-console.cfg sets the same variable and sorts
// after 50, so it would keep the old device if left.
func TestConfigureConsoleRetiresTheLateCommandFile(t *testing.T) {
	t.Run("the exact line the late-command wrote is removed", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "99-console.cfg")
		require.NoError(t, os.WriteFile(old, []byte("GRUB_CMDLINE_LINUX=\"console=tty0 console=ttyS0,115200\"\n"), 0o644))

		out, code := run(t, dir, filepath.Join(t.TempDir(), "grub.log"), "ttyS1")
		require.Equal(t, 0, code, out)
		assert.NoFileExists(t, old)
		assert.FileExists(t, filepath.Join(dir, "50-nvke-console.cfg"))
	})

	t.Run("an edited file stays and is reported", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "99-console.cfg")
		edited := "GRUB_CMDLINE_LINUX=\"console=tty0 console=ttyS0,115200 quiet\"\n"
		require.NoError(t, os.WriteFile(old, []byte(edited), 0o644))

		out, code := run(t, dir, filepath.Join(t.TempDir(), "grub.log"), "ttyS1")
		require.Equal(t, 0, code, out)
		assert.Contains(t, out, "was edited, so it is left in place")
		got, err := os.ReadFile(old)
		require.NoError(t, err)
		assert.Equal(t, edited, string(got))
	})

	t.Run("a file with more than the one line stays", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "99-console.cfg")
		content := "GRUB_CMDLINE_LINUX=\"console=tty0 console=ttyS0,115200\"\nGRUB_TIMEOUT=0\n"
		require.NoError(t, os.WriteFile(old, []byte(content), 0o644))

		out, code := run(t, dir, filepath.Join(t.TempDir(), "grub.log"), "ttyS1")
		require.Equal(t, 0, code, out)
		assert.Contains(t, out, "was edited")
		assert.FileExists(t, old)
	})
}

func TestConfigureConsoleFailsWhenUpdateGrubFails(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(t.TempDir(), "update-grub")
	require.NoError(t, os.WriteFile(bad, []byte("#!/bin/sh\nexit 1\n"), 0o755))
	cmd := exec.CommandContext(t.Context(), "bash", configureConsole, "ttyS0")
	cmd.Env = append(os.Environ(), "NVKE_GRUB_DROPIN_DIR="+dir, "NVKE_UPDATE_GRUB="+bad)
	_, err := cmd.CombinedOutput()
	require.Error(t, err, "a node whose grub.cfg was not regenerated must not look configured")
}
