// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultStopGrace is how long the process group gets to exit on a terminate
// signal before it is killed. kind and helm clean up after themselves when
// asked, and that cleanup is worth waiting for; a bootstrap wedged in a retry
// loop is not.
const DefaultStopGrace = 20 * time.Second

// Logf is a logging sink, satisfied by (*testing.T).Logf.
type Logf func(format string, args ...any)

// BootstrapOptions are the inputs to one `nvkectl bootstrap site` run. Only
// Binary and SiteSpec are required; an unset field leaves the flag off, so the
// command's own default applies.
type BootstrapOptions struct {
	// Binary is the nvkectl to run. See FindNvkectl.
	Binary string
	// SiteSpec is the site spec file, the -f argument.
	SiteSpec string

	// KubeconfigOut is where the finished cluster's kubeconfig is written.
	KubeconfigOut string
	// EncryptionKeyOut is where the etcd encryption key is written. The
	// command refuses to overwrite one, so each run needs its own path.
	EncryptionKeyOut string
	// DiagnosticsDir is where a failed bootstrap dumps cluster state.
	DiagnosticsDir string

	// Timeout is the command's own --timeout, bounding how long it waits for
	// the site cluster. It is not a bound on this process: see Bootstrap.Wait.
	Timeout time.Duration

	// KeepBootstrapCluster leaves the ephemeral Kind cluster standing, so the
	// caller can read it afterwards. The caller then owns deleting it.
	KeepBootstrapCluster bool
	// DisableMTLS stands the site up without SPIRE.
	DisableMTLS bool
	// Verbose passes -v, which makes a successful run write its diagnostics
	// bundle too.
	Verbose bool

	// LogFile is a file to write the command's combined output to, in
	// addition to the Logf sink. The bootstrap lanes grep this file, so it is
	// how they keep the assertions they make today. The file is created, or
	// truncated when it exists, and closed once the command exits.
	LogFile string

	// StopGrace is how long the process group gets to exit on a terminate
	// signal before it is killed, and how long Wait waits for the output pipe
	// after the command has gone. Zero means DefaultStopGrace.
	StopGrace time.Duration

	// Env is added to the environment the command inherits, as "K=V" entries.
	Env []string
}

// stopGrace is the effective StopGrace, applying the default.
func (o BootstrapOptions) stopGrace() time.Duration {
	if o.StopGrace <= 0 {
		return DefaultStopGrace
	}
	return o.StopGrace
}

// args renders the options as the command line, in a fixed order so a test can
// state the whole of it.
func (o BootstrapOptions) args() []string {
	args := []string{"bootstrap", "site"}
	if o.Verbose {
		args = append(args, "-v")
	}
	args = append(args, "-f", o.SiteSpec)
	if o.KeepBootstrapCluster {
		args = append(args, "--keep-bootstrap-cluster")
	}
	if o.DisableMTLS {
		args = append(args, "--disable-mtls")
	}
	if o.Timeout > 0 {
		args = append(args, "--timeout", o.Timeout.String())
	}
	if o.KubeconfigOut != "" {
		args = append(args, "--kubeconfig-out", o.KubeconfigOut)
	}
	if o.EncryptionKeyOut != "" {
		args = append(args, "--encryption-key-out", o.EncryptionKeyOut)
	}
	if o.DiagnosticsDir != "" {
		args = append(args, "--diagnostics-dir", o.DiagnosticsDir)
	}
	return args
}

// validate reports the options a run cannot start without.
func (o BootstrapOptions) validate() error {
	switch {
	case o.Binary == "":
		return errors.New("bootstraptest: BootstrapOptions.Binary is empty: name the nvkectl to run")
	case o.SiteSpec == "":
		return errors.New("bootstraptest: BootstrapOptions.SiteSpec is empty: name the site spec to bootstrap from")
	}
	return nil
}

// Bootstrap is one running `nvkectl bootstrap site`.
//
// The command runs in a process group of its own. nvkectl does not forward
// signals to the kind, helm and nerdctl processes it drives, so stopping it
// means signalling the group; a caller that abandoned the direct child would
// leave those running and the next run would trip over what they hold.
type Bootstrap struct {
	cmd *exec.Cmd

	// finished is closed once cmd.Wait has returned, having set waitErr.
	// Closing it rather than sending on it is what makes Wait, ExitCode,
	// Exited and Stop safe to call in any order and more than once.
	finished chan struct{}
	waitErr  error

	stopGrace time.Duration
	stopOnce  sync.Once
}

// StartBootstrap starts `nvkectl bootstrap site` and returns without waiting
// for it. Output is captured, written to opts.LogFile when one is named, and
// handed line by line to logf.
//
// The caller must call Stop, normally deferred: a caller that fails part way
// through otherwise leaves a bootstrap running.
func StartBootstrap(ctx context.Context, opts BootstrapOptions, logf Logf) (*Bootstrap, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// The log file is the record of the run when there is one, and the only
	// one: the lane greps it and scans it for credentials, and a second copy
	// on this process's stdout would be a copy nothing scans. Without a log
	// file the output goes to logf, so a run outside a lane still shows it.
	var (
		logFile *os.File
		sink    io.Writer
	)
	if opts.LogFile != "" {
		f, err := os.Create(opts.LogFile)
		if err != nil {
			return nil, fmt.Errorf("creating bootstrap log file %q: %w", opts.LogFile, err)
		}
		logFile, sink = f, f
	} else {
		sink = newLineWriter(logf)
	}

	args := opts.args()
	cmd := exec.CommandContext(ctx, opts.Binary, args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	cmd.Stdout = sink
	cmd.Stderr = cmd.Stdout
	setProcessGroup(cmd)
	// Cancelling ctx must reach the whole group, as Stop does, and for the
	// same reason: the default cancel signals nvkectl alone and leaves the
	// kind, helm and nerdctl it drives running.
	cmd.Cancel = func() error {
		terminateProcessGroup(cmd)
		return nil
	}
	// Output goes to a pipe, so Wait does not return until every process
	// holding the write end has closed it -- a grandchild that outlives
	// nvkectl would block it indefinitely. WaitDelay bounds that wait and
	// closes the pipe itself. See the ErrWaitDelay case below.
	cmd.WaitDelay = opts.stopGrace()

	logf("bootstraptest: running %s %s", opts.Binary, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("starting %s %s: %w", opts.Binary, strings.Join(args, " "), err)
	}

	b := &Bootstrap{cmd: cmd, finished: make(chan struct{}), stopGrace: opts.stopGrace()}
	go func() {
		b.waitErr = waitFor(cmd, opts.stopGrace(), logf)
		// After Wait, so nothing is still writing to it.
		if logFile != nil {
			_ = logFile.Close()
		}
		close(b.finished)
	}()
	return b, nil
}

// waitFor waits for the command and reports what it exited with.
//
// It separates "the command failed" from "the command finished and something
// it started still held the output pipe", which WaitDelay reports as
// exec.ErrWaitDelay. Only the first is a failed bootstrap. The shell lanes
// this replaces waited on the direct child and never saw the second, so
// reporting it as a failure would fail runs that pass today.
func waitFor(cmd *exec.Cmd, grace time.Duration, logf Logf) error {
	err := cmd.Wait()
	if err != nil && errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		logf("bootstraptest: bootstrap site exited 0; something it started still held the output pipe after %s", grace)
		return nil
	}
	return err
}

// Wait blocks until the command exits and reports its exit error, nil on a
// clean exit. It is safe to call more than once and after Stop.
func (b *Bootstrap) Wait() error {
	<-b.finished
	return b.waitErr
}

// ExitCode reports the command's exit status once it has exited. It returns -1
// while the command is still running, and for a command ended by a signal.
func (b *Bootstrap) ExitCode() int {
	select {
	case <-b.finished:
		return b.cmd.ProcessState.ExitCode()
	default:
		return -1
	}
}

// Exited reports whether the command has already finished.
func (b *Bootstrap) Exited() bool {
	select {
	case <-b.finished:
		return true
	default:
		return false
	}
}

// Stop signals the command's process group and waits for it. It is a no-op
// once the command has exited, and safe to call more than once.
func (b *Bootstrap) Stop() {
	b.stopOnce.Do(func() {
		if b.Exited() {
			return
		}
		terminateProcessGroup(b.cmd)
		select {
		case <-b.finished:
		case <-time.After(b.stopGrace):
			killProcessGroup(b.cmd)
		}
	})
	_ = b.Wait()
}

// The bootstrap e2e lanes run on Linux, and nvkectl ships for linux and darwin
// only, so these use the unix syscalls directly rather than through a
// build-tagged shim for a platform this module never targets.

// setProcessGroup puts the command in a process group of its own, so the whole
// tree it starts can be signalled as one. Without it the command shares this
// process's group, and signalling that group would signal this process too.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup asks the command's whole process group to exit.
func terminateProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGTERM) }

// killProcessGroup ends the command's whole process group without asking.
func killProcessGroup(cmd *exec.Cmd) { signalProcessGroup(cmd, syscall.SIGKILL) }

// signalProcessGroup sends sig to the group, falling back to the process alone
// when the group is not ours to signal.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// A negative pid is the group. Setpgid made the child its own leader, so
	// the group id is the child's pid.
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

// FindNvkectl resolves the nvkectl binary to exercise: NVKECTL when set,
// otherwise bin/nvkectl under cliDir. It reports an error rather than falling
// back to PATH, so a lane cannot silently test an nvkectl from somewhere else.
func FindNvkectl(cliDir string) (string, error) {
	path := os.Getenv("NVKECTL")
	if path == "" {
		path = cliDir + "/bin/nvkectl"
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("nvkectl %q is not there (%w): build it with `make -C cli build`, or name another in NVKECTL", path, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("nvkectl %q is not an executable file", path)
	}
	return path, nil
}

// lineWriter forwards whole lines to a Logf, so the test log reads like the
// command's own output rather than being split at arbitrary read boundaries.
type lineWriter struct {
	logf    Logf
	partial []byte
}

func newLineWriter(logf Logf) *lineWriter { return &lineWriter{logf: logf} }

func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.logf("%s", strings.TrimRight(string(w.partial[:i]), "\r"))
		w.partial = w.partial[i+1:]
	}
}
