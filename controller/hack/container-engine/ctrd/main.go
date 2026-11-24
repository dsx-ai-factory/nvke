// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command ctrd is a container engine CLI for the local development flow on a host that has containerd
// and nothing else: no Docker daemon and no nerdctl. It speaks the subset of
// docker's command line that the flow and kind's nerdctl provider use, and
// implements it with ctr, the CNI plugins and buildctl. Anything else it
// refuses. It needs root; the flow runs it through sudo (hack/container-engine/shim).
//
// See controller/README.md, "Running the local flow on containerd alone".
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const (
	cmdCreate     = "create"
	cmdExec       = "exec"
	cmdInspect    = "inspect"
	cmdImage      = "image"
	flagFormat    = "format"
	flagPlatform  = "platform"
	flagQuiet     = "quiet"
	flagName      = "name"
	flagHostname  = "hostname"
	flagRestart   = "restart"
	fileHosts     = "hosts"
	dockerHub     = "docker.io"
	bridgeDriver  = "bridge"
	statusRunning = "running"
	setenvFlag    = "--setenv"
	nameKey       = "Name"
	typeKey       = "type"
	bindOption    = "rbind"
	fileResolv    = "resolv.conf"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := run(ctx)
	stop()
	os.Exit(status)
}

func run(ctx context.Context) int {
	engine := &engine{ctx: ctx, cfg: settingsFromEnv(os.Getenv), stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	engine.exe, _ = os.Executable()
	return engine.run(os.Args[1:])
}

// run executes one command line and returns the exit status.
func (e *engine) run(args []string) int {
	err := e.dispatch(args)
	var exit exitError
	var overlap poolOverlapError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return int(exit)
	case errors.As(err, &overlap):
		// Docker's wording, which kind's network creation recognizes: print it as it is.
		_, _ = fmt.Fprintln(e.stderr, err)
	default:
		_, _ = fmt.Fprintln(e.stderr, "ctrd: "+err.Error())
	}
	return 1
}

func (e *engine) dispatch(args []string) error {
	if len(args) == 0 {
		return errors.New("a command is required; see controller/README.md")
	}
	command, rest := args[0], args[1:]
	commands := map[string]func([]string) error{
		cmdCreate: e.create, "run": e.runContainer, "start": e.start, "stop": e.stop, "restart": e.restart,
		"rm": e.remove, "wait": e.wait, "update": e.update, "ps": e.ps, cmdInspect: e.inspect, "logs": e.logs,
		cmdExec: e.exec, "cp": e.cp, "info": e.info, "version": e.version, "pull": e.pull, "build": e.build,
		"save": e.save, "load": e.load, "network": e.network, cmdImage: e.image,
		"_supervise": e.superviseCommand,
	}
	fn, ok := commands[command]
	if !ok {
		return notSupportedError(command)
	}
	return fn(rest)
}

func (e *engine) image(args []string) error {
	if len(args) == 0 || args[0] != cmdInspect {
		return notSupportedError("image " + strings.Join(args, " "))
	}
	return e.inspect(append([]string{"--type=" + cmdImage}, args[1:]...))
}

func (e *engine) network(args []string) error {
	if len(args) == 0 {
		return notSupportedError("network")
	}
	switch args[0] {
	case cmdCreate:
		return e.networkCreate(args[1:])
	case cmdInspect:
		return e.networkInspect(args[1:])
	case "rm":
		return e.networkRemove(args[1:])
	}
	return notSupportedError("network " + args[0])
}

func (e *engine) networkCreate(args []string) error {
	flags, err := parseFlags("network create", flagSet{
		"driver": {Kind: stringFlag, Short: "d"}, "opt": {Kind: listFlag, Short: "o"}, "subnet": {Kind: listFlag}, "ipv6": {Kind: boolFlag},
	}, args, false)
	if err != nil {
		return err
	}
	if driver := flags.str("driver"); driver != "" && driver != bridgeDriver {
		return notSupportedError("network create -d " + driver)
	}
	if len(flags.args) != 1 {
		return errors.New("network create: one name is required")
	}
	options := map[string]string{}
	for _, option := range flags.list("opt") {
		key, value, _ := strings.Cut(option, "=")
		options[key] = value
	}
	id, err := e.createNetwork(flags.args[0], flags.list("subnet"), options)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, id)
	return err
}

func (e *engine) networkInspect(args []string) error {
	flags, err := parseFlags("network inspect", flagSet{flagFormat: {Kind: stringFlag, Short: "f"}}, args, false)
	if err != nil {
		return err
	}
	var failures []error
	var documents []any
	for _, name := range flags.args {
		network, err := e.loadNetwork(name)
		switch {
		case err != nil:
			failures = append(failures, err)
		case flags.str(flagFormat) != "":
			failures = append(failures, render(e.stdout, flags.str(flagFormat), e.networkDocument(network)))
		default:
			documents = append(documents, e.networkDocument(network))
		}
	}
	if len(documents) > 0 {
		failures = append(failures, e.printDocument("", documents))
	}
	return errors.Join(failures...)
}

func (e *engine) networkRemove(args []string) error {
	flags, err := parseFlags("network rm", flagSet{}, args, false)
	if err != nil {
		return err
	}
	var failures []error
	for _, name := range flags.args {
		if err := e.removeNetwork(name); err != nil {
			failures = append(failures, err)
		} else {
			_, _ = fmt.Fprintln(e.stdout, name)
		}
	}
	return errors.Join(failures...)
}

// superviseCommand is `_supervise ID`, which systemd runs for each started container.
func (e *engine) superviseCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("_supervise: one container ID is required")
	}
	return e.supervise(args[0])
}
