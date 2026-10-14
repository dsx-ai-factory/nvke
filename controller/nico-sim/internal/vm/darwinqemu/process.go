// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package darwinqemu

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var errNativeProcessUnjoined = errors.New("native process teardown did not join")

const nativeKillJoinTimeout = 5 * time.Second

// ownedProcess is issued only after this package starts the command. Status
// and termination never reconstruct a process capability from a saved PID.
type ownedProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func startOwnedProcess(command *exec.Cmd, output io.Closer) (*ownedProcess, error) {
	if err := command.Start(); err != nil {
		if output != nil {
			output.Close()
		}
		return nil, err
	}
	process := &ownedProcess{command: command, done: make(chan struct{})}
	go func() {
		process.err = command.Wait()
		if output != nil {
			output.Close()
		}
		close(process.done)
	}()
	return process, nil
}

func (p *ownedProcess) stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("process teardown requires a context")
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errors.Join(ctx.Err(), err)
		}
		// SIGKILL does not prove Wait and output finalization have returned.
		// Keep the owner's handle on timeout so a later teardown can join it.
		joined := time.NewTimer(nativeKillJoinTimeout)
		defer joined.Stop()
		select {
		case <-p.done:
			return ctx.Err()
		case <-joined.C:
			return errors.Join(ctx.Err(), errNativeProcessUnjoined)
		}
	}
}
