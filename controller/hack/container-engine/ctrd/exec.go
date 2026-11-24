// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// exitError is a command's wish to exit with a status, which `exec` passes on from the process it ran.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e *engine) exec(args []string) error {
	flags, err := parseFlags(cmdExec, flagSet{
		"privileged": {Kind: boolFlag}, "interactive": {Kind: boolFlag, Short: "i"}, "tty": {Kind: boolFlag, Short: "t"},
		"detach": {Kind: boolFlag, Short: "d"}, "env": {Kind: listFlag, Short: "e"},
	}, args, true)
	if err != nil {
		return err
	}
	if len(flags.args) < 2 {
		return errors.New("exec: a container and a command are required")
	}
	meta, err := e.findContainer(flags.args[0])
	if err != nil {
		return err
	}
	if running, err := e.isRunning(meta.ID); err != nil {
		return err
	} else if !running {
		return fmt.Errorf("container %s is not running", meta.Name)
	}
	command := flags.args[1:]
	// ctr has no flag for the environment of the new process; env sets it.
	if env := flags.list("env"); len(env) > 0 {
		command = append(append([]string{"env"}, env...), command...)
	}
	cmd := e.ctrExecCommand(meta.ID, ctrExecOptions{ExecID: shortID(randomID()), Tty: flags.has("tty"), Detach: flags.has("detach")}, command)
	cmd.Stdout, cmd.Stderr = e.stdout, e.stderr
	if flags.has("interactive") {
		cmd.Stdin = e.stdin
	}
	return exitStatus(cmd.Run())
}

// exitStatus turns the exit of a child into an exitError carrying its status.
func exitStatus(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitError(exitErr.ExitCode())
	}
	return err
}

// logs prints the container's console, and with -f follows it until the container stops.
func (e *engine) logs(args []string) error {
	flags, err := parseFlags("logs", flagSet{"follow": {Kind: boolFlag, Short: "f"}}, args, false)
	if err != nil {
		return err
	}
	if len(flags.args) != 1 {
		return errors.New("logs: one container is required")
	}
	meta, err := e.findContainer(flags.args[0])
	if err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(e.containerDir(meta.ID), "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if _, err := io.Copy(e.stdout, file); err != nil {
			return err
		}
		if !flags.has("follow") {
			return nil
		}
		running, err := e.isRunning(meta.ID)
		if err != nil {
			return err
		}
		if !running {
			// One more read: what the container wrote as it stopped.
			_, err = io.Copy(e.stdout, file)
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cp copies a file from the host into a container that has not been started or
// has stopped, through a mount of its root file system. The file is owned by root.
func (e *engine) cp(args []string) error {
	flags, err := parseFlags("cp", flagSet{}, args, false)
	if err != nil {
		return err
	}
	if len(flags.args) != 2 {
		return errors.New("cp: SRC CONTAINER:DST is required")
	}
	containerRef, destination, found := strings.Cut(flags.args[1], ":")
	if !found {
		return notSupportedError("cp from a container")
	}
	meta, err := e.findContainer(containerRef)
	if err != nil {
		return err
	}
	if running, err := e.isRunning(meta.ID); err != nil {
		return err
	} else if running {
		return notSupportedError("cp into a running container")
	}
	info, err := os.Stat(flags.args[0])
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return notSupportedError("cp of anything but a file")
	}
	data, err := os.ReadFile(flags.args[0])
	if err != nil {
		return err
	}
	mnt, err := os.MkdirTemp(e.cfg.Root, "mnt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)
	if err := e.ctrMountSnapshot(mnt, meta.ID); err != nil {
		return err
	}
	defer func() { _, _ = e.output("umount", mnt) }()

	root, err := os.OpenRoot(mnt)
	if err != nil {
		return err
	}
	defer root.Close()
	relative := strings.TrimPrefix(path.Clean("/"+destination), "/")
	if target, err := root.Stat(relative); err == nil && target.IsDir() {
		relative = path.Join(relative, filepath.Base(flags.args[0]))
	}
	file, err := root.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("copying to %s in %s: %w", destination, meta.Name, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
