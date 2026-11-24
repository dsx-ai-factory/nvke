// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// A container with a tty needs a console that is read for as long as it runs:
// systemd writes its boot messages to it, and kind waits for one of them. ctr
// requires a terminal of its own to start such a task and then stays attached,
// copying the console. So each started container has a supervisor: this binary,
// detached from the process that started the container, which gives ctr a
// pseudo-terminal, appends what comes out of it to the container's log, and
// records the exit status when the task ends. The supervisor moves itself into
// a cgroup of its own first: a restart of the unit that ran the tool which
// started the container would otherwise kill it, and the console with it.

const (
	startTimeout = 60 * time.Second
	// A task that ran this long starts the restart policy's backoff again.
	healthyRun = 10 * time.Second
	// The wait before the first automatic restart; each one after it doubles the wait, up to a minute.
	restartMaxDelay = time.Minute
	stoppedFile     = "stopped"
	restartsFile    = "restarts"
)

// errStoppedByUser ends an automatic start that a stop got in front of.
var errStoppedByUser = errors.New("the container was stopped")

// superviseStart starts the supervisor and waits for the task to run.
func (e *engine) superviseStart(meta *containerMeta) error {
	_ = os.Remove(e.exitCodeFile(meta.ID))
	cmd := exec.Command(e.exe, "_supervise", meta.ID) //nolint:gosec,noctx // the supervisor outlives this command, so it must not be tied to its context
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(startTimeout)
	for {
		if running, err := e.isRunning(meta.ID); err != nil || running {
			return err
		}
		if _, err := os.Stat(e.exitCodeFile(meta.ID)); err == nil {
			return fmt.Errorf("container %s exited before it was running:\n%s", meta.Name, e.logTail(meta.ID))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s is not running after %s; the console is %s\n%s",
				meta.Name, startTimeout, filepath.Join(e.containerDir(meta.ID), "log"), e.logTail(meta.ID))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *engine) logTail(id string) string {
	data, _ := os.ReadFile(filepath.Join(e.containerDir(id), "log"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return strings.Join(lines[max(0, len(lines)-10):], "\n")
}

// supervise is the supervisor: `ctrd _supervise ID`.
func (e *engine) supervise(id string) error {
	defer leaveCallerCgroup(filepath.Join(e.cfg.SupervisorCgroups, shortID(id)))()
	log, err := os.OpenFile(filepath.Join(e.containerDir(id), "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	master, slave, err := openPty()
	if err != nil {
		return err
	}
	defer master.Close()
	cmd := e.ctrStartCommand(id)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		slave.Close()
		return err
	}
	slave.Close()
	copied := make(chan struct{})
	go func() {
		// Reading the master fails with EIO once nothing holds the slave.
		_, _ = io.Copy(log, master)
		close(copied)
	}()
	began := time.Now()
	code := 0
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if code = 1; errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
	}
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
	}
	if err := os.WriteFile(e.exitCodeFile(id), []byte(strconv.Itoa(code)+"\n"), 0o600); err != nil {
		return err
	}
	e.restartPerPolicy(id, code, time.Since(began), log)
	return nil
}

// restartPolicy reads a restart policy, NAME or NAME:MAX, and says whether it
// asks for a restart after the task ended with code, and how many it allows
// (0 is no limit). Docker's policies: always and unless-stopped restart after any
// exit, on-failure after a failure.
func restartPolicy(spec string, code int) (bool, int) {
	name, retries, _ := strings.Cut(spec, ":")
	switch name {
	case "always", "unless-stopped":
		return true, 0
	case "on-failure":
		limit, _ := strconv.Atoi(retries)
		return code != 0, limit
	}
	return false, 0
}

func (e *engine) stoppedMarker(id string) string {
	return filepath.Join(e.containerDir(id), stoppedFile)
}

// markStopped records that the container was stopped, or could not be started,
// so that its restart policy leaves it alone until the next start.
func (e *engine) markStopped(id string) {
	_ = os.WriteFile(e.stoppedMarker(id), nil, 0o600)
}

func (e *engine) stoppedByUser(id string) bool {
	_, err := os.Stat(e.stoppedMarker(id))
	return err == nil
}

func (e *engine) clearStopped(id string) {
	_ = os.Remove(e.stoppedMarker(id))
	_ = os.Remove(filepath.Join(e.containerDir(id), restartsFile))
}

// restartPerPolicy starts the container again when its restart policy says so and
// nothing stopped it: a reboot from inside a node ends its init, and nobody but
// this supervisor is left to notice. The task of a container that has ended is
// gone by now; the new start gets a supervisor of its own, so this one can end.
// Everything it reports goes to the container's log.
func (e *engine) restartPerPolicy(id string, code int, ranFor time.Duration, log io.Writer) {
	meta, err := e.loadContainer(id)
	if err != nil || e.stoppedByUser(id) {
		return
	}
	restart, limit := restartPolicy(meta.Restart, code)
	if !restart {
		return
	}
	count := e.restartCount(id)
	if ranFor >= healthyRun {
		count = 0
	}
	if limit > 0 && count >= limit {
		return
	}
	_ = os.WriteFile(filepath.Join(e.containerDir(id), restartsFile), []byte(strconv.Itoa(count+1)+"\n"), 0o600)
	time.Sleep(min(e.cfg.RestartDelay<<min(count, 10), restartMaxDelay))
	if e.stoppedByUser(id) {
		return
	}
	_ = e.withLock(func() error {
		current, err := e.loadContainer(id)
		if err != nil {
			return err
		}
		return e.detach(current)
	})
	if err := e.startContainer(meta, true); err != nil && !errors.Is(err, errStoppedByUser) {
		_, _ = fmt.Fprintf(log, "ctrd: restarting the container (policy %s): %v\n", meta.Restart, err)
	}
}

func (e *engine) restartCount(id string) int {
	data, err := os.ReadFile(filepath.Join(e.containerDir(id), restartsFile))
	if err != nil {
		return 0
	}
	count, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return count
}

// openPty opens a pseudo-terminal pair.
func openPty() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var number, unlock uint32
	for _, request := range []struct {
		code uintptr
		arg  *uint32
	}{{syscall.TIOCSPTLCK, &unlock}, {syscall.TIOCGPTN, &number}} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), request.code, uintptr(unsafe.Pointer(request.arg))); errno != 0 {
			master.Close()
			return nil, nil, fmt.Errorf("opening a pseudo-terminal: %w", errno)
		}
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(number)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// leaveCallerCgroup moves the calling process into a cgroup of its own, as far
// as the host allows (cgroup v2), and returns what removes that cgroup. dir is a
// child of the cgroup the supervisors share, whose parent it moves back to.
func leaveCallerCgroup(dir string) func() {
	// The paths are built from the engine's settings and a container ID.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // see above
		return func() {}
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("0\n"), 0o644); err != nil { //nolint:gosec // a cgroup control file
		_ = os.Remove(dir) //nolint:gosec // see above
		return func() {}
	}
	// A cgroup with a process in it cannot be removed: go back to the parent first.
	return func() {
		_ = os.WriteFile(filepath.Join(filepath.Dir(dir), "cgroup.procs"), []byte("0\n"), 0o644) //nolint:gosec // see above
		_ = os.Remove(dir)                                                                       //nolint:gosec // see above
	}
}
