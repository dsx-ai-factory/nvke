// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests that start a process do not call t.Parallel. The fake nvkectl is
// written and then executed, and a fork from a sibling test holding the
// still-open write descriptor makes that exec fail with ETXTBSY
// (https://go.dev/issue/22315). The whole suite runs in seconds, so there is
// nothing to win by running them at once.
//
// fakeNvkectl writes a shell script that stands in for nvkectl, and returns
// its path. The script runs body with the command's arguments in "$@".
func fakeNvkectl(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nvkectl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return path
}

// collectLogf returns a Logf that records what it is given, and a func to read
// the record. Both are safe to use from more than one goroutine.
func collectLogf() (Logf, func() []string) {
	var (
		mu    sync.Mutex
		lines []string
	)
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(args) == 0 {
			lines = append(lines, format)
			return
		}
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}
	read := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
	return logf, read
}

func TestBootstrapOptionsArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts BootstrapOptions
		want []string
	}{
		{
			name: "only the site spec, so every other flag keeps the command's default",
			opts: BootstrapOptions{Binary: "nvkectl", SiteSpec: "site.yaml"},
			want: []string{"bootstrap", "site", "-f", "site.yaml"},
		},
		{
			name: "the provisioned lane's command line",
			opts: BootstrapOptions{
				Binary:               "nvkectl",
				SiteSpec:             "site.yaml",
				Verbose:              true,
				KeepBootstrapCluster: true,
				Timeout:              15 * time.Minute,
				KubeconfigOut:        "/tmp/kubeconfig",
				EncryptionKeyOut:     "/tmp/etcd.key",
			},
			want: []string{
				"bootstrap", "site", "-v", "-f", "site.yaml", "--keep-bootstrap-cluster",
				"--timeout", "15m0s", "--kubeconfig-out", "/tmp/kubeconfig",
				"--encryption-key-out", "/tmp/etcd.key",
			},
		},
		{
			name: "disabling mTLS and naming a diagnostics directory",
			opts: BootstrapOptions{
				Binary:         "nvkectl",
				SiteSpec:       "site.yaml",
				DisableMTLS:    true,
				DiagnosticsDir: "/tmp/diag",
			},
			want: []string{"bootstrap", "site", "-f", "site.yaml", "--disable-mtls", "--diagnostics-dir", "/tmp/diag"},
		},
		{
			name: "a zero timeout leaves --timeout off",
			opts: BootstrapOptions{Binary: "nvkectl", SiteSpec: "site.yaml", Timeout: 0},
			want: []string{"bootstrap", "site", "-f", "site.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.opts.args())
		})
	}
}

func TestBootstrapOptionsStopGrace(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultStopGrace, BootstrapOptions{}.stopGrace(), "an unset StopGrace must fall back to the default")
	assert.Equal(t, DefaultStopGrace, BootstrapOptions{StopGrace: -time.Second}.stopGrace(), "a negative StopGrace must not disable the bound")
	assert.Equal(t, 5*time.Second, BootstrapOptions{StopGrace: 5 * time.Second}.stopGrace())
}

func TestBootstrapOptionsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    BootstrapOptions
		wantErr string
	}{
		{
			name: "complete options validate",
			opts: BootstrapOptions{Binary: "nvkectl", SiteSpec: "site.yaml"},
		},
		{
			name:    "no binary",
			opts:    BootstrapOptions{SiteSpec: "site.yaml"},
			wantErr: "Binary is empty",
		},
		{
			name:    "no site spec",
			opts:    BootstrapOptions{Binary: "nvkectl"},
			wantErr: "SiteSpec is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.opts.validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestStartBootstrapRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()

	_, err := StartBootstrap(context.Background(), BootstrapOptions{SiteSpec: "site.yaml"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Binary is empty")
}

func TestStartBootstrapReportsAnUnstartableBinary(t *testing.T) {
	opts := BootstrapOptions{Binary: filepath.Join(t.TempDir(), "absent"), SiteSpec: "site.yaml"}
	_, err := StartBootstrap(context.Background(), opts, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "starting")
}

func TestBootstrapWaitReportsTheExitStatus(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  bool
		wantCode int
	}{
		{name: "a clean exit", body: "exit 0", wantCode: 0},
		{name: "a failure", body: "exit 3", wantErr: true, wantCode: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := BootstrapOptions{Binary: fakeNvkectl(t, tt.body), SiteSpec: "site.yaml"}
			b, err := StartBootstrap(context.Background(), opts, nil)
			require.NoError(t, err)
			t.Cleanup(b.Stop)

			err = b.Wait()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantCode, b.ExitCode())
			assert.True(t, b.Exited())
			// Wait is safe to call more than once and reports the same thing.
			assert.Equal(t, err != nil, b.Wait() != nil)
		})
	}
}

func TestBootstrapExitCodeIsUnknownWhileRunning(t *testing.T) {
	opts := BootstrapOptions{Binary: fakeNvkectl(t, "sleep 30"), SiteSpec: "site.yaml"}
	b, err := StartBootstrap(context.Background(), opts, nil)
	require.NoError(t, err)
	t.Cleanup(b.Stop)

	assert.False(t, b.Exited())
	assert.Equal(t, -1, b.ExitCode())
}

func TestBootstrapCapturesOutputToTheLogFileOnly(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bootstrap.log")
	logf, lines := collectLogf()
	opts := BootstrapOptions{
		Binary:   fakeNvkectl(t, "echo to stdout; echo to stderr >&2"),
		SiteSpec: "site.yaml",
		LogFile:  logPath,
	}

	b, err := StartBootstrap(context.Background(), opts, logf)
	require.NoError(t, err)
	t.Cleanup(b.Stop)
	require.NoError(t, b.Wait())

	// The lane greps this file, so both streams have to reach it, and it has
	// to be closed and flushed by the time Wait returns.
	onDisk, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "to stdout")
	assert.Contains(t, string(onDisk), "to stderr")

	// And it is the only copy: a second one on stdout is a copy the lane's
	// credential scan would not see.
	assert.NotContains(t, lines(), "to stdout")
}

// Without a log file there is nothing to grep, so the output goes to the log
// sink instead and a run outside a lane still shows it.
func TestBootstrapCapturesOutputToTheLogSinkWithoutALogFile(t *testing.T) {
	logf, lines := collectLogf()
	opts := BootstrapOptions{Binary: fakeNvkectl(t, "echo to stdout; echo to stderr >&2"), SiteSpec: "site.yaml"}

	b, err := StartBootstrap(context.Background(), opts, logf)
	require.NoError(t, err)
	t.Cleanup(b.Stop)
	require.NoError(t, b.Wait())

	assert.Contains(t, lines(), "to stdout")
	assert.Contains(t, lines(), "to stderr")
}

func TestStartBootstrapReportsAnUnwritableLogFile(t *testing.T) {
	opts := BootstrapOptions{
		Binary:   fakeNvkectl(t, "exit 0"),
		SiteSpec: "site.yaml",
		LogFile:  filepath.Join(t.TempDir(), "no-such-directory", "bootstrap.log"),
	}
	_, err := StartBootstrap(context.Background(), opts, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating bootstrap log file")
}

func TestStopEndsARunningCommandAndIsRepeatable(t *testing.T) {
	opts := BootstrapOptions{Binary: fakeNvkectl(t, "sleep 60"), SiteSpec: "site.yaml"}
	b, err := StartBootstrap(context.Background(), opts, nil)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		b.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(DefaultStopGrace + 10*time.Second):
		t.Fatal("Stop did not return")
	}

	assert.True(t, b.Exited())
	assert.NotPanics(t, b.Stop)
}

func TestStopOnAnExitedCommandIsANoOp(t *testing.T) {
	opts := BootstrapOptions{Binary: fakeNvkectl(t, "exit 0"), SiteSpec: "site.yaml"}
	b, err := StartBootstrap(context.Background(), opts, nil)
	require.NoError(t, err)
	require.NoError(t, b.Wait())

	assert.NotPanics(t, b.Stop)
	assert.NoError(t, b.Wait())
}

// Stopping the command must end what it started, not only the command itself.
// nvkectl does not forward signals to the kind, helm and nerdctl it drives, so
// a lane that left those running would wedge the next one.
func TestStopEndsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-alive")
	// The child outlives its parent unless the group is signalled, and keeps
	// touching the marker for as long as it runs.
	body := "sh -c 'while true; do touch " + marker + "; sleep 0.1; done' &\nsleep 60"
	opts := BootstrapOptions{Binary: fakeNvkectl(t, body), SiteSpec: "site.yaml"}

	b, err := StartBootstrap(context.Background(), opts, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "the child never started")

	b.Stop()

	// Give anything still running a chance to touch the marker again, then
	// check it has stopped being touched.
	require.NoError(t, os.Remove(marker))
	time.Sleep(500 * time.Millisecond)
	_, err = os.Stat(marker)
	assert.Error(t, err, "the child outlived Stop: the process group was not signalled")
}

// nvkectl exiting 0 while something it started still holds the output pipe is
// not a failed bootstrap. The shell lanes waited on the direct child and never
// saw this, so reporting it as a failure would fail runs that pass today.
func TestWaitTreatsAHeldOutputPipeAfterACleanExitAsSuccess(t *testing.T) {
	// The grandchild inherits stdout and outlives the command, holding the
	// write end of the pipe open past the command's own exit.
	body := "sh -c 'sleep 60' &\nexit 0"
	opts := BootstrapOptions{Binary: fakeNvkectl(t, body), SiteSpec: "site.yaml", StopGrace: time.Second}

	b, err := StartBootstrap(context.Background(), opts, nil)
	require.NoError(t, err)
	t.Cleanup(b.Stop)

	start := time.Now()
	err = b.Wait()
	elapsed := time.Since(start)

	assert.NoError(t, err, "a clean exit was reported as a failure because the pipe stayed open")
	assert.Equal(t, 0, b.ExitCode())
	// Bounded by StopGrace rather than by the grandchild's own lifetime.
	assert.Less(t, elapsed, 30*time.Second)
}

func TestFindNvkectl(t *testing.T) {
	cliDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cliDir, "bin"), 0o755))
	built := filepath.Join(cliDir, "bin", "nvkectl")
	require.NoError(t, os.WriteFile(built, []byte("#!/bin/sh\n"), 0o755))

	t.Run("falls back to the build output under cliDir", func(t *testing.T) {
		t.Setenv("NVKECTL", "")
		got, err := FindNvkectl(cliDir)
		require.NoError(t, err)
		assert.Equal(t, built, got)
	})

	t.Run("NVKECTL names another binary", func(t *testing.T) {
		other := fakeNvkectl(t, "exit 0")
		t.Setenv("NVKECTL", other)
		got, err := FindNvkectl(cliDir)
		require.NoError(t, err)
		assert.Equal(t, other, got)
	})

	t.Run("reports a binary that is not there rather than falling back to PATH", func(t *testing.T) {
		t.Setenv("NVKECTL", filepath.Join(t.TempDir(), "absent"))
		_, err := FindNvkectl(cliDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not there")
	})

	t.Run("reports a file that is not executable", func(t *testing.T) {
		notExec := filepath.Join(t.TempDir(), "nvkectl")
		require.NoError(t, os.WriteFile(notExec, []byte("text"), 0o644))
		t.Setenv("NVKECTL", notExec)
		_, err := FindNvkectl(cliDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not an executable file")
	})

	t.Run("reports a directory", func(t *testing.T) {
		t.Setenv("NVKECTL", t.TempDir())
		_, err := FindNvkectl(cliDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not an executable file")
	})
}

func TestLineWriterForwardsWholeLines(t *testing.T) {
	t.Parallel()

	logf, lines := collectLogf()
	w := newLineWriter(logf)

	// Split mid-line, as a pipe read would be.
	_, err := w.Write([]byte("first li"))
	require.NoError(t, err)
	assert.Empty(t, lines(), "a partial line was forwarded before its newline")

	_, err = w.Write([]byte("ne\r\nsecond line\npart"))
	require.NoError(t, err)
	assert.Equal(t, []string{"first line", "second line"}, lines())
}
