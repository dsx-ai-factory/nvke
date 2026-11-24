// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Every invocation of ctr is in this file. containerd documents ctr as a
// debugging client whose command line is not a stable interface: when a release
// changes a flag or an output, the fix is here.

// stopSignalLabel is the label containerd puts on a container for the image's
// STOPSIGNAL; ctr create sets it.
const (
	stopSignalLabel = "io.containerd.image.config.stop-signal"
)

func (e *engine) ctrArgs(args ...string) []string {
	return append([]string{"--address", e.cfg.Address, "--namespace", e.cfg.Namespace}, args...)
}

func (e *engine) ctrCommand(args ...string) *exec.Cmd {
	return e.command("ctr", e.ctrArgs(args...)...)
}

func (e *engine) ctr(args ...string) (string, error) {
	return e.output("ctr", e.ctrArgs(args...)...)
}

// ctrServerVersion is containerd's version, as `ctr version` prints it under "Server:".
func (e *engine) ctrServerVersion() (string, error) {
	out, err := e.ctr("version")
	if err != nil {
		return "", err
	}
	_, server, found := strings.Cut(out, "Server:")
	if found {
		for _, line := range strings.Split(server, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "Version:" {
				return fields[1], nil
			}
		}
	}
	return "", fmt.Errorf("ctr version printed no server version: %q", out)
}

// ctrCreateSpec is a container as ctr create takes it.
type ctrCreateSpec struct {
	ID         string
	Image      string
	Command    []string
	Env        []string
	Labels     []string
	Mounts     []string
	CapAdd     []string
	Devices    []string
	WithNS     []string
	Hostname   string
	Privileged bool
	Tty        bool
	HostNet    bool
}

func (s ctrCreateSpec) args() []string {
	args := []string{"containers", cmdCreate}
	flags := func(name string, values []string) {
		for _, value := range values {
			args = append(args, name, value)
		}
	}
	flags("--env", s.Env)
	flags("--label", s.Labels)
	flags("--mount", s.Mounts)
	flags("--cap-add", s.CapAdd)
	flags("--device", s.Devices)
	flags("--with-ns", s.WithNS)
	if s.Hostname != "" {
		args = append(args, "--hostname", s.Hostname)
	}
	if s.Privileged {
		args = append(args, "--privileged")
	}
	if s.Tty {
		args = append(args, "--tty")
	}
	if s.HostNet {
		args = append(args, "--net-host")
	}
	args = append(args, s.Image, s.ID)
	return append(args, s.Command...)
}

func (e *engine) ctrCreate(spec ctrCreateSpec) error {
	_, err := e.ctr(spec.args()...)
	return err
}

// ctrStopSignal is the signal the image asks to be stopped with.
func (e *engine) ctrStopSignal(id string) string {
	out, err := e.ctr("containers", "info", id)
	if err != nil {
		return "SIGTERM"
	}
	var info struct {
		Labels map[string]string `json:"Labels"`
	}
	if json.Unmarshal([]byte(out), &info) != nil || info.Labels[stopSignalLabel] == "" {
		return "SIGTERM"
	}
	return info.Labels[stopSignalLabel]
}

func (e *engine) ctrDeleteContainer(id string) error {
	_, err := e.ctr("containers", "delete", id)
	return err
}

// ctrStartCommand is the attached start the supervisor of a container runs: ctr
// stays for the life of the task and copies its console.
func (e *engine) ctrStartCommand(id string) *exec.Cmd {
	return e.ctrCommand("tasks", "start", id)
}

// ctrTasks lists the tasks by container ID, with their status (RUNNING, STOPPED, ...).
func (e *engine) ctrTasks() (map[string]string, error) {
	out, err := e.ctr("tasks", "list")
	if err != nil {
		return nil, err
	}
	tasks := map[string]string{}
	for _, line := range strings.Split(out, "\n")[1:] {
		if fields := strings.Fields(line); len(fields) == 3 {
			tasks[fields[0]] = fields[2]
		}
	}
	return tasks, nil
}

func (e *engine) ctrKill(id, signal string) error {
	_, err := e.ctr("tasks", "kill", "--signal", signal, id)
	return err
}

func (e *engine) ctrDeleteTask(id string) {
	_, _ = e.ctr("tasks", "delete", id)
}

type ctrExecOptions struct {
	ExecID string
	Tty    bool
	// Detach starts the process and returns; its output goes nowhere.
	Detach bool
}

// ctrExecCommand runs a process in the container's task.
func (e *engine) ctrExecCommand(id string, opts ctrExecOptions, command []string) *exec.Cmd {
	args := []string{"tasks", cmdExec, "--exec-id", opts.ExecID}
	if opts.Tty {
		args = append(args, "--tty")
	}
	if opts.Detach {
		args = append(args, "--detach", "--log-uri", "file:///dev/null")
	}
	return e.ctrCommand(append(append(args, id), command...)...)
}

// ctrImage is a row of `ctr images list`.
type ctrImage struct {
	Ref    string
	Digest string
}

// ctrImages lists the images matching the filters (for example name==REF); none lists all.
func (e *engine) ctrImages(filters ...string) ([]ctrImage, error) {
	out, err := e.ctr(append([]string{"images", "list"}, filters...)...)
	if err != nil {
		return nil, err
	}
	var images []ctrImage
	for _, line := range strings.Split(out, "\n")[1:] {
		if fields := strings.Fields(line); len(fields) >= 3 {
			images = append(images, ctrImage{Ref: fields[0], Digest: fields[2]})
		}
	}
	return images, nil
}

// ctrPull pulls an image. ctr prints its progress as many lines when it is not on a terminal; they are discarded unless the pull fails.
func (e *engine) ctrPull(ref, platform string) error {
	_, err := e.ctr("images", "pull", "--platform", platform, ref)
	return err
}

func (e *engine) ctrExport(path, platform string, refs []string) error {
	_, err := e.ctr(append([]string{"images", "export", "--platform", platform, path}, refs...)...)
	return err
}

func (e *engine) ctrImport(path, platform string) error {
	_, err := e.ctr("images", "import", "--platform", platform, path)
	return err
}

// ctrMountSnapshot mounts the container's root file system, the snapshot ctr
// create made for it, at target. ctr prints the mount command rather than running it.
// mount is told to use mount(2): through the newer mount API the kernel takes
// 256 bytes of lowerdir at most, which an image of four layers exceeds.
func (e *engine) ctrMountSnapshot(target, id string) error {
	command, err := e.ctr("snapshots", "mounts", target, id)
	if err != nil {
		return err
	}
	if _, err := e.output("env", "LIBMOUNT_FORCE_MOUNT2=always", "sh", "-c", command); err != nil {
		return fmt.Errorf("mounting the root file system of %s: %w", id, err)
	}
	return nil
}
