// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultRoot         = "/var/lib/ctrd"
	defaultNamespace    = "ctrd"
	defaultAddress      = "/run/containerd/containerd.sock"
	defaultBuildkitHost = "unix:///run/buildkit/buildkitd.sock"
)

// settings is everything the engine reads from its environment. Each field has
// one variable; controller/README.md lists them.
type settings struct {
	// Root is the state directory (CTRD_ROOT).
	Root string
	// Namespace is the containerd namespace every container and image lives in (CTRD_NAMESPACE).
	Namespace string
	// Address is containerd's socket (CONTAINERD_ADDRESS, which ctr itself also reads).
	Address string
	// CNIDirs are searched for the CNI plugin binaries (CNI_PATH, the variable nerdctl reads).
	CNIDirs []string
	// BuildkitHost is buildkitd's address (BUILDKIT_HOST).
	BuildkitHost string
	// DNS overrides the name servers of every container the engine creates (CTRD_DNS). It is read at create:
	// the resolv.conf it gives is recorded in the container and does not follow this setting afterwards.
	DNS []string
	// SupervisorCgroups is the cgroup the console supervisors each get a child of.
	SupervisorCgroups string
	// RestartDelay is the wait before the first automatic restart of a container.
	RestartDelay time.Duration
	// ResolvConfs are the files the host's own upstream resolvers are read from, first that exists.
	ResolvConfs []string
}

func settingsFromEnv(getenv func(string) string) settings {
	orDefault := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}
	cfg := settings{
		Root:         orDefault("CTRD_ROOT", defaultRoot),
		Namespace:    orDefault("CTRD_NAMESPACE", defaultNamespace),
		Address:      orDefault("CONTAINERD_ADDRESS", defaultAddress),
		BuildkitHost: orDefault("BUILDKIT_HOST", defaultBuildkitHost),
		DNS:          splitList(getenv("CTRD_DNS")),
		// On a systemd-resolved host /etc/resolv.conf names the 127.0.0.53 stub,
		// which does not exist inside a container; the upstream servers are in the
		// first file.
		ResolvConfs:       []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"},
		SupervisorCgroups: "/sys/fs/cgroup/ctrd-supervisors",
		RestartDelay:      time.Second,
	}
	cfg.CNIDirs = splitList(strings.ReplaceAll(getenv("CNI_PATH"), ":", ","))
	if len(cfg.CNIDirs) == 0 {
		cfg.CNIDirs = []string{"/opt/cni/bin", "/usr/local/libexec/cni", "/usr/local/lib/cni", "/usr/libexec/cni", "/usr/lib/cni"}
	}
	return cfg
}

// splitList splits on commas and white space.
func splitList(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

// engine carries the settings and the streams of one invocation.
type engine struct {
	ctx    context.Context //nolint:containedctx // the engine is one command invocation; the context is its lifetime
	cfg    settings
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// exe is this binary, which the supervisor of a started container re-executes.
	exe string
}

// command builds the one kind of child process the engine starts: a named tool
// found on PATH. Everything it runs goes through here.
func (e *engine) command(name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(e.ctx, name, args...) //nolint:gosec // the engine's job is to run these tools; the arguments are built by its own code
	cmd.Stderr = e.stderr
	return cmd
}

// output runs the command and returns its standard output; a failure carries
// the standard error.
func (e *engine) output(name string, args ...string) (string, error) {
	cmd := e.command(name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// resolvConf returns the resolv.conf of a container: CTRD_DNS when it is set, else
// the host's upstream IPv4 resolvers, without loopback stubs, and the
// search and options lines of the same file.
func (cfg settings) resolvConf() (string, error) {
	if len(cfg.DNS) > 0 {
		return nameserverLines(cfg.DNS), nil
	}
	for _, path := range cfg.ResolvConfs {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var servers, rest []string
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			switch {
			case len(fields) >= 2 && fields[0] == "nameserver":
				if ip := net.ParseIP(fields[1]); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
					servers = append(servers, fields[1])
				}
			case len(fields) >= 2 && (fields[0] == "search" || fields[0] == "options"):
				rest = append(rest, strings.TrimSpace(line))
			}
		}
		if len(servers) == 0 {
			continue
		}
		return nameserverLines(servers) + strings.Join(append(rest, ""), "\n"), nil
	}
	return "", fmt.Errorf("no upstream IPv4 resolver found in %s (only loopback stubs?): set CTRD_DNS to the servers containers should use", strings.Join(cfg.ResolvConfs, " or "))
}

func nameserverLines(servers []string) string {
	var b strings.Builder
	for _, server := range servers {
		b.WriteString("nameserver " + server + "\n")
	}
	return b.String()
}
