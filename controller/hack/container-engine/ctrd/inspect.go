// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// What the callers read of `inspect`, `ps`, `info`, `version` and `network inspect`
// is docker's output, usually through a --format template, so these documents
// have docker's field names. They are maps where a name would not pass the
// linter's naming rules (HostIp) or where a template needs a missing key to be empty.

// render prints data once through the Go template, as docker does for --format.
func render(w io.Writer, format string, data any) error {
	//nolint:gosec // the template is the caller's --format argument
	tmpl, err := template.New(flagFormat).Funcs(template.FuncMap{
		"json": func(v any) (string, error) {
			out, err := json.Marshal(v)
			return string(out), err
		},
	}).Parse(format)
	if err != nil {
		return fmt.Errorf("invalid --format %q: %w", format, err)
	}
	if err := tmpl.Execute(w, data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// printDocument prints data through the template, or as indented JSON.
func (e *engine) printDocument(format string, data any) error {
	if format != "" {
		return render(e.stdout, format, data)
	}
	out, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, string(out))
	return err
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (e *engine) containerDocument(meta *containerMeta, status string) map[string]any {
	ports := map[string][]map[string]string{}
	for _, port := range meta.Publish {
		key := fmt.Sprintf("%d/%s", port.ContainerPort, port.Protocol)
		host := port.HostIP
		if host == "" {
			host = "0.0.0.0"
		}
		ports[key] = append(ports[key], map[string]string{"HostIp": host, "HostPort": strconv.Itoa(port.HostPort)})
	}
	networks := map[string]map[string]string{}
	if meta.Network != hostNetwork {
		networks[meta.Network] = map[string]string{
			"IPAddress": meta.IPv4, "GlobalIPv6Address": meta.IPv6, "MacAddress": meta.MAC,
		}
		if network, err := e.loadNetwork(meta.Network); err == nil {
			networks[meta.Network]["NetworkID"] = network.ID
			networks[meta.Network]["Gateway"] = network.Gateways[0]
		}
	}
	mounts := make([]map[string]any, 0, len(meta.Volumes))
	for _, vol := range meta.Volumes {
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": vol.Source, "Destination": vol.Destination, "RW": !contains(vol.Options, "ro")})
	}
	policy, retries, _ := strings.Cut(meta.Restart, ":")
	maxRetries, _ := strconv.Atoi(retries)
	return map[string]any{
		"Id":      meta.ID,
		nameKey:   "/" + meta.Name,
		"Created": timestamp(meta.Created),
		"Image":   meta.Image,
		"State": map[string]any{
			"Status": status, "Running": status == statusRunning, "Paused": false, "Restarting": false, "Dead": false,
			"ExitCode": meta.ExitCode, "StartedAt": timestamp(meta.StartedAt), "FinishedAt": timestamp(meta.FinishedAt),
		},
		"Config": map[string]any{
			"Hostname": meta.Hostname, "Image": meta.Image, "Env": meta.Env, "Tty": meta.Tty, "Labels": meta.Labels,
			"Entrypoint": meta.Entrypoint, "Cmd": meta.Command,
		},
		"HostConfig": map[string]any{
			"NetworkMode": meta.Network, "Privileged": meta.Privileged,
			"RestartPolicy": map[string]any{nameKey: policy, "MaximumRetryCount": maxRetries},
		},
		"NetworkSettings": map[string]any{"Networks": networks, "Ports": ports, "IPAddress": meta.IPv4},
		"Mounts":          mounts,
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// containerStatus is created, running or exited.
func (e *engine) containerStatus(meta *containerMeta, tasks map[string]string) string {
	switch {
	case tasks[meta.ID] == "RUNNING":
		return statusRunning
	case meta.StartedAt.IsZero():
		return "created"
	default:
		return "exited"
	}
}

func (e *engine) inspect(args []string) error {
	flags, err := parseFlags("inspect", flagSet{
		flagFormat: {Kind: stringFlag, Short: "f"}, typeKey: {Kind: stringFlag},
	}, args, false)
	if err != nil {
		return err
	}
	if kind := flags.str(typeKey); kind != "" && kind != cmdImage && kind != "container" {
		return notSupportedError("inspect --type=" + kind)
	}
	var failures []error
	var documents []any
	for _, ref := range flags.args {
		var document any
		meta, err := e.findContainer(ref)
		switch {
		case err == nil && flags.str(typeKey) != cmdImage:
			var tasks map[string]string
			if tasks, err = e.ctrTasks(); err == nil {
				document = e.containerDocument(meta, e.containerStatus(meta, tasks))
			}
		case flags.str(typeKey) == "container":
		default:
			document, err = e.imageDocument(ref)
		}
		switch {
		case err != nil:
			failures = append(failures, err)
		case flags.str(flagFormat) != "":
			failures = append(failures, render(e.stdout, flags.str(flagFormat), document))
		default:
			documents = append(documents, document)
		}
	}
	// Without --format docker prints the documents as one array.
	if len(documents) > 0 {
		failures = append(failures, e.printDocument("", documents))
	}
	return errors.Join(failures...)
}

// psRow is what `ps --format` templates read.
type psRow struct {
	ID        string
	Names     string
	Image     string
	Status    string
	State     string
	Command   string
	Ports     string
	CreatedAt string
	labels    map[string]string
}

// Label is the value of one label, as docker's `{{.Label "key"}}`.
func (r psRow) Label(key string) string { return r.labels[key] }

func (e *engine) ps(args []string) error {
	flags, err := parseFlags("ps", flagSet{
		"all": {Kind: boolFlag, Short: "a"}, flagQuiet: {Kind: boolFlag, Short: "q"},
		"filter": {Kind: listFlag}, flagFormat: {Kind: stringFlag},
	}, args, false)
	if err != nil {
		return err
	}
	wantLabels := map[string]string{}
	for _, filter := range flags.list("filter") {
		label, ok := strings.CutPrefix(filter, "label=")
		if !ok {
			return notSupportedError("ps --filter " + filter)
		}
		key, value, hasValue := strings.Cut(label, "=")
		wantLabels[key] = value
		if !hasValue {
			wantLabels[key] = "\x00"
		}
	}
	all, err := e.listContainers()
	if err != nil {
		return err
	}
	tasks, err := e.ctrTasks()
	if err != nil {
		return err
	}
	format := flags.str(flagFormat)
	if format == "" && flags.has(flagQuiet) {
		format = "{{.ID}}"
	}
	if format == "" {
		_, _ = fmt.Fprintln(e.stdout, "CONTAINER ID\tIMAGE\tSTATUS\tPORTS\tNAMES")
		format = "{{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}\t{{.Names}}"
	}
	for _, meta := range all {
		status := e.containerStatus(meta, tasks)
		if status != statusRunning && !flags.has("all") {
			continue
		}
		if !labelsMatch(meta.Labels, wantLabels) {
			continue
		}
		var ports []string
		for _, port := range meta.Publish {
			ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", port.HostIP, port.HostPort, port.ContainerPort, port.Protocol))
		}
		row := psRow{
			ID: shortID(meta.ID), Names: meta.Name, Image: meta.Image, State: status, Status: status,
			Command: strings.Join(append(append([]string{}, meta.Entrypoint...), meta.Command...), " "),
			Ports:   strings.Join(ports, ", "), CreatedAt: timestamp(meta.Created), labels: meta.Labels,
		}
		if status == "exited" {
			row.Status = fmt.Sprintf("Exited (%d)", meta.ExitCode)
		}
		if err := render(e.stdout, format, row); err != nil {
			return err
		}
	}
	return nil
}

func labelsMatch(labels, want map[string]string) bool {
	for key, value := range want {
		have, ok := labels[key]
		if !ok || (value != "\x00" && have != value) {
			return false
		}
	}
	return true
}

// info prints what kind reads to decide how to configure a node: the cgroup
// version and the controllers that are delegated.
func (e *engine) info(args []string) error {
	flags, err := parseFlags("info", flagSet{flagFormat: {Kind: stringFlag, Short: "f"}}, args, false)
	if err != nil {
		return err
	}
	version, err := e.ctrServerVersion()
	if err != nil {
		return err
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	cgroupVersion := "2"
	if err != nil {
		cgroupVersion = "1"
	}
	has := func(controller string) bool {
		return err != nil || contains(strings.Fields(string(controllers)), controller)
	}
	hostname, _ := os.Hostname()
	return e.printDocument(flags.str(flagFormat), map[string]any{
		"ServerVersion": version, nameKey: hostname, "Architecture": runtime.GOARCH, "OperatingSystem": runtime.GOOS,
		"CgroupDriver": "cgroupfs", "CgroupVersion": cgroupVersion,
		"MemoryLimit": has("memory"), "PidsLimit": has("pids"), "CPUShares": has("cpu"),
		"SecurityOptions": []string{},
	})
}

func (e *engine) version(args []string) error {
	flags, err := parseFlags("version", flagSet{flagFormat: {Kind: stringFlag, Short: "f"}}, args, false)
	if err != nil {
		return err
	}
	version, err := e.ctrServerVersion()
	if err != nil {
		return err
	}
	document := map[string]any{
		"Client": map[string]string{"Version": "ctrd"},
		"Server": map[string]string{"Version": version, "Os": runtime.GOOS, "Arch": runtime.GOARCH},
	}
	if flags.str(flagFormat) == "" {
		_, err = fmt.Fprintf(e.stdout, "Client:\n Version: ctrd\nServer: containerd\n Version: %s\n Arch: %s\n", version, runtime.GOARCH)
		return err
	}
	return render(e.stdout, flags.str(flagFormat), document)
}

func (e *engine) networkDocument(network *networkMeta) map[string]any {
	config := make([]map[string]string, 0, len(network.Subnets))
	for i, subnet := range network.Subnets {
		config = append(config, map[string]string{"Subnet": subnet, "Gateway": network.Gateways[i]})
	}
	options := map[string]string{}
	for key, value := range network.Options {
		options[key] = value
	}
	return map[string]any{
		nameKey: network.Name, "Id": network.ID, "Created": timestamp(network.Created), "Driver": bridgeDriver,
		"EnableIPv6": len(network.Subnets) > 1, "IPAM": map[string]any{"Driver": "default", "Config": config},
		"Options": options, "Labels": map[string]string{},
	}
}
