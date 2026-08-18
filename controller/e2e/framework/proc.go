//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ProcessConfig describes a helper process launched from the current test binary.
type ProcessConfig struct {
	Command       string
	Args          []string
	WorkingDir    string
	Env           []string
	ReadyContains string
	ReadyTimeout  time.Duration
}

// Process wraps a helper process and its captured stdout/stderr.
type Process struct {
	cmd          *exec.Cmd
	outputs      *lockedOutput
	ready        chan struct{}
	exited       chan error
	readyTimeout time.Duration
}

// StartProcess starts a helper process.
func StartProcess(t testing.TB, cfg ProcessConfig) *Process {
	t.Helper()

	command := cfg.Command
	if command == "" {
		command = os.Args[0]
	}
	cmd := exec.Command(command, cfg.Args...) // #nosec G204 -- e2e helpers intentionally launch test-controlled binaries.
	cmd.Dir = cfg.WorkingDir
	cmd.Env = append(os.Environ(), cfg.Env...)

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)

	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 10 * time.Second
	}
	p := &Process{
		cmd:          cmd,
		outputs:      &lockedOutput{},
		ready:        make(chan struct{}),
		exited:       make(chan error, 1),
		readyTimeout: cfg.ReadyTimeout,
	}
	var readyOnce sync.Once
	readOutput := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			p.outputs.add(line)
			if cfg.ReadyContains != "" && strings.Contains(line, cfg.ReadyContains) {
				readyOnce.Do(func() { close(p.ready) })
			}
		}
		if err := scanner.Err(); err != nil {
			p.outputs.add("failed to read process output: " + err.Error())
		}
	}

	require.NoError(t, cmd.Start())
	go readOutput(stdout)
	go readOutput(stderr)
	go func() {
		p.exited <- cmd.Wait()
	}()

	return p
}

// WaitReady blocks until the configured readiness output is observed or the process exits.
func (p *Process) WaitReady(t testing.TB) {
	t.Helper()

	select {
	case <-p.ready:
	case err := <-p.exited:
		require.FailNowf(t, "process exited before it was ready", "wait error: %v\nprocess output:\n%s", err, p.Output())
	case <-time.After(p.readyTimeout):
		require.FailNowf(t, "process did not become ready", "process output:\n%s", p.Output())
	}
}

// WaitExit waits for the process to exit and returns its wait error.
func (p *Process) WaitExit(t testing.TB, timeout time.Duration) error {
	t.Helper()

	select {
	case err := <-p.exited:
		return err
	case <-time.After(timeout):
		require.FailNowf(t, "process did not exit", "process output:\n%s", p.Output())
		return nil
	}
}

// WaitForOutput waits until the process emits output containing substring.
func (p *Process) WaitForOutput(ctx context.Context, substring string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if strings.Contains(p.Output(), substring) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Kill terminates the process and returns its wait error.
func (p *Process) Kill(t testing.TB) error {
	t.Helper()

	require.NotNil(t, p.cmd.Process)
	require.NoError(t, p.cmd.Process.Kill())
	return p.WaitExit(t, 5*time.Second)
}

// Stop interrupts the process and kills it if it does not exit promptly.
func (p *Process) Stop(t testing.TB) {
	t.Helper()

	if p.cmd.Process == nil {
		return
	}
	require.NoError(t, p.cmd.Process.Signal(os.Interrupt))
	select {
	case err := <-p.exited:
		require.NoError(t, err, "process output:\n%s", p.Output())
	case <-time.After(5 * time.Second):
		require.NoError(t, p.cmd.Process.Kill())
		err := <-p.exited
		require.Error(t, err, "process should report an error after kill")
	}
}

// Output returns captured stdout and stderr, line by line.
func (p *Process) Output() string {
	return p.outputs.String()
}

type lockedOutput struct {
	mu sync.Mutex
	b  strings.Builder
}

func (o *lockedOutput) add(line string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.b.WriteString(line)
	o.b.WriteByte('\n')
}

func (o *lockedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}
