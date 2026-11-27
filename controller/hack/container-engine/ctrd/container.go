// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func createFlagSet() flagSet {
	return flagSet{
		flagName: {Kind: stringFlag}, flagHostname: {Kind: stringFlag}, "network": {Kind: stringFlag}, "net": {Kind: stringFlag},
		flagRestart: {Kind: stringFlag}, "entrypoint": {Kind: stringFlag}, "gpus": {Kind: stringFlag},
		"label": {Kind: listFlag, Short: "l"}, "env": {Kind: listFlag, Short: "e"}, "volume": {Kind: listFlag, Short: "v"},
		"tmpfs": {Kind: listFlag}, "publish": {Kind: listFlag, Short: "p"}, "sysctl": {Kind: listFlag},
		"cap-add": {Kind: listFlag}, "security-opt": {Kind: listFlag},
		"privileged": {Kind: boolFlag}, "tty": {Kind: boolFlag, Short: "t"}, "detach": {Kind: boolFlag, Short: "d"}, "init": {Kind: boolFlag},
	}
}

// runContainer implements `run --detach`: create and start.
func (e *engine) runContainer(args []string) error {
	id, err := e.createContainer("run", args)
	if err != nil {
		return err
	}
	meta, err := e.loadContainer(id)
	if err != nil {
		return err
	}
	if err := e.startContainer(meta, false); err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, id)
	return err
}

func (e *engine) create(args []string) error {
	id, err := e.createContainer(cmdCreate, args)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, id)
	return err
}

// createContainer makes the container, as ctr create does, and records it. It
// returns the ID.
func (e *engine) createContainer(command string, args []string) (string, error) {
	flags, err := parseFlags(command, createFlagSet(), args, true)
	if err != nil {
		return "", err
	}
	if command == "run" && !flags.has("detach") {
		return "", notSupportedError("run without --detach")
	}
	if len(flags.args) == 0 {
		return "", fmt.Errorf("%s: an image is required", command)
	}
	meta, err := e.containerFromFlags(flags)
	if err != nil {
		return "", err
	}
	if err := e.ensureImage(meta.Image); err != nil {
		return "", err
	}
	if err := e.withLock(func() error { return e.registerContainer(meta) }); err != nil {
		return "", err
	}
	if err := e.makeContainer(meta); err != nil {
		_ = e.removeContainerFiles(meta)
		return "", err
	}
	return meta.ID, nil
}

var (
	portSpec = regexp.MustCompile(`(?i)^(?:(.*):)?(\d*):(\d+)(?:/(tcp|udp|sctp))?$`)
	nameSpec = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

func (e *engine) containerFromFlags(flags *parsedFlags) (*containerMeta, error) {
	var err error
	if flags.has("init") {
		return nil, notSupportedError("--init")
	}
	meta := &containerMeta{
		ID:         randomID(),
		Created:    time.Now().UTC(),
		Image:      normalizeRef(flags.args[0]),
		Hostname:   flags.str(flagHostname),
		Name:       flags.str(flagName),
		Privileged: flags.has("privileged"),
		Tty:        flags.has("tty"),
		Restart:    flags.str(flagRestart),
		Labels:     map[string]string{},
		Sysctls:    map[string]string{},
	}
	if meta.Name == "" {
		meta.Name = "ctrd-" + shortID(meta.ID)
	}
	if !nameSpec.MatchString(meta.Name) {
		return nil, fmt.Errorf("invalid container name %q", meta.Name)
	}
	meta.Network = flags.str("network")
	if net, ok := flags.set("net"); ok {
		meta.Network = net
	}
	if meta.Network == "" {
		return nil, errors.New("this engine has no default network: pass --network NAME, or host")
	}
	if meta.Network != hostNetwork {
		if _, err := e.loadNetwork(meta.Network); err != nil {
			return nil, err
		}
		if meta.Hostname == "" {
			meta.Hostname = shortID(meta.ID)
		}
		if meta.ResolvConf, err = e.cfg.resolvConf(); err != nil {
			return nil, err
		}
	}

	if entrypoint := flags.str("entrypoint"); entrypoint != "" {
		meta.Entrypoint = []string{entrypoint}
		meta.Command = flags.args[1:]
	} else if len(flags.args) > 1 {
		return nil, notSupportedError("a command without --entrypoint (it would need the image's entrypoint)")
	}
	for _, label := range flags.list("label") {
		key, value, _ := strings.Cut(label, "=")
		meta.Labels[key] = value
	}
	for _, env := range flags.list("env") {
		if strings.Contains(env, "=") {
			meta.Env = append(meta.Env, env)
		} else if value, ok := os.LookupEnv(env); ok {
			meta.Env = append(meta.Env, env+"="+value)
		}
	}
	for _, option := range flags.list("security-opt") {
		if option != "seccomp=unconfined" && option != "apparmor=unconfined" {
			return nil, notSupportedError("--security-opt " + option)
		}
	}
	for _, capability := range flags.list("cap-add") {
		meta.CapAdd = append(meta.CapAdd, "CAP_"+strings.TrimPrefix(strings.ToUpper(capability), "CAP_"))
	}
	for _, sysctl := range flags.list("sysctl") {
		key, value, ok := strings.Cut(sysctl, "=")
		if !ok || !sysctlKey.MatchString(key) {
			return nil, notSupportedError("--sysctl " + sysctl + " (only net.* settings)")
		}
		meta.Sysctls[key] = value
	}
	if meta.GPUs, err = cdiDevices(flags.str("gpus")); err != nil {
		return nil, err
	}
	if meta.Publish, err = parsePublish(flags.list("publish")); err != nil {
		return nil, err
	}
	for i, spec := range flags.list("volume") {
		vol, err := e.parseVolume(meta.ID, i, spec)
		if err != nil {
			return nil, err
		}
		meta.Volumes = append(meta.Volumes, vol)
	}
	for _, spec := range flags.list("tmpfs") {
		path, options, _ := strings.Cut(spec, ":")
		meta.Tmpfs = append(meta.Tmpfs, volume{Destination: path, Options: tmpfsOptions(strings.Split(options, ","))})
	}
	meta.Volumes = append(meta.Volumes, e.runDirectory(meta)...)
	return meta, nil
}

// runDirectory is the volume that gives a container the /run its image has.
// ctr's default spec mounts an empty tmpfs there, and the image's /run is
// what daemons like FRR keep their pid files in. Docker mounts nothing, so a
// container that does not ask for its own /run gets a volume, filled from the image.
func (e *engine) runDirectory(meta *containerMeta) []volume {
	for _, mount := range append(slices.Clone(meta.Tmpfs), meta.Volumes...) {
		if mount.Destination == "/run" {
			return nil
		}
	}
	return []volume{{Source: filepath.Join(e.containerDir(meta.ID), "volumes", "run"), Destination: "/run", Options: []string{bindOption}, Anonymous: true}}
}

// parseVolume reads SRC:DST[:OPTIONS], or a bare DST for an anonymous volume.
func (e *engine) parseVolume(id string, index int, spec string) (volume, error) {
	parts := strings.Split(spec, ":")
	if len(parts) == 1 && strings.HasPrefix(spec, "/") {
		return volume{
			Source: filepath.Join(e.containerDir(id), "volumes", strconv.Itoa(index)), Destination: spec, Options: []string{bindOption}, Anonymous: true,
		}, nil
	}
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "/") || !strings.HasPrefix(parts[1], "/") {
		return volume{}, notSupportedError("--volume " + spec + " (only host paths and anonymous volumes)")
	}
	vol := volume{Source: parts[0], Destination: parts[1], Options: []string{bindOption}}
	if len(parts) > 2 {
		for _, option := range strings.Split(parts[2], ",") {
			switch option {
			case "ro", "rw", "rshared", "rslave", "rprivate", "shared", "slave", "private":
				vol.Options = append(vol.Options, option)
			case "z", "Z":
			default:
				return volume{}, notSupportedError("--volume option " + option)
			}
		}
	}
	return vol, nil
}

// tmpfsOptions adds the options docker gives a tmpfs mount unless asked otherwise.
func tmpfsOptions(given []string) []string {
	options := slices.DeleteFunc(slices.Clone(given), func(option string) bool { return option == "" })
	for _, pair := range [][2]string{{"exec", "noexec"}, {"suid", "nosuid"}, {"dev", "nodev"}} {
		if !slices.Contains(options, pair[0]) && !slices.Contains(options, pair[1]) {
			options = append(options, pair[1])
		}
	}
	return options
}

// parsePublish reads [IP:]HOSTPORT:CONTAINERPORT[/PROTOCOL].
func parsePublish(specs []string) ([]portMapping, error) {
	var ports []portMapping
	for _, spec := range specs {
		match := portSpec.FindStringSubmatch(spec)
		if len(match) == 0 || match[2] == "" {
			return nil, notSupportedError("--publish " + spec)
		}
		hostPort, _ := strconv.Atoi(match[2])
		containerPort, err := strconv.Atoi(match[3])
		if err != nil || containerPort < 1 || containerPort > 65535 || hostPort > 65535 {
			return nil, fmt.Errorf("invalid port in --publish %s", spec)
		}
		// Kind writes the protocol in capitals.
		protocol := strings.ToLower(match[4])
		if protocol == "" {
			protocol = "tcp"
		}
		ports = append(ports, portMapping{HostIP: strings.Trim(match[1], "[]"), HostPort: hostPort, ContainerPort: containerPort, Protocol: protocol})
	}
	return ports, nil
}

// cdiDevices maps docker's --gpus to the CDI devices the NVIDIA container
// toolkit's generated spec names: all, a count, or device=0,1.
func cdiDevices(gpus string) ([]string, error) {
	gpus = strings.Trim(strings.TrimSpace(gpus), `"'`)
	const prefix = "nvidia.com/gpu="
	switch {
	case gpus == "":
		return nil, nil
	case gpus == "all" || gpus == "-1":
		return []string{prefix + "all"}, nil
	case strings.HasPrefix(gpus, "device="):
		var devices []string
		for _, device := range strings.Split(strings.TrimPrefix(gpus, "device="), ",") {
			devices = append(devices, prefix+device)
		}
		return devices, nil
	}
	count, err := strconv.Atoi(gpus)
	if err != nil || count < 1 {
		return nil, notSupportedError("--gpus " + gpus)
	}
	var devices []string
	for i := range count {
		devices = append(devices, prefix+strconv.Itoa(i))
	}
	return devices, nil
}

// registerContainer reserves the name and makes the directory. The state lock is held.
func (e *engine) registerContainer(meta *containerMeta) error {
	all, err := e.listContainers()
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.Name == meta.Name {
			return fmt.Errorf("the container name %q is already in use by container %s", meta.Name, shortID(other.ID))
		}
	}
	return os.MkdirAll(filepath.Join(e.containerDir(meta.ID), "volumes"), 0o755)
}

func (e *engine) removeContainerFiles(meta *containerMeta) error {
	return os.RemoveAll(e.containerDir(meta.ID))
}

// makeContainer asks containerd for the container and prepares the files and
// volumes it mounts. The state lock is not held.
func (e *engine) makeContainer(meta *containerMeta) error {
	dir := e.containerDir(meta.ID)
	spec := ctrCreateSpec{
		ID: meta.ID, Image: meta.Image, Command: append(append([]string{}, meta.Entrypoint...), meta.Command...),
		Env: meta.Env, CapAdd: meta.CapAdd, Devices: meta.GPUs, Privileged: meta.Privileged, Tty: meta.Tty,
		WithNS: []string{"cgroup:"},
	}
	for key, value := range meta.Labels {
		spec.Labels = append(spec.Labels, key+"="+value)
	}
	bind := func(source, destination string, options ...string) {
		spec.Mounts = append(spec.Mounts, "type=bind,src="+source+",dst="+destination+",options="+strings.Join(options, ":"))
	}
	if meta.Network == hostNetwork {
		spec.HostNet = true
		for _, name := range []string{fileHosts, fileResolv} {
			if _, err := os.Stat("/etc/" + name); err == nil {
				bind("/etc/"+name, "/etc/"+name, "rbind", "ro")
			}
		}
	} else {
		spec.Hostname = meta.Hostname
		spec.WithNS = append(spec.WithNS, "network:"+netnsPath(meta.ID))
		if err := e.writeContainerFiles(meta); err != nil {
			return err
		}
		for _, name := range []string{fileHosts, fileResolv, flagHostname} {
			bind(filepath.Join(dir, name), "/etc/"+name, "rbind", "rw")
		}
	}
	if meta.Privileged {
		// ctr's default spec has no cgroup mount, and a systemd inside the container needs one (docker adds it).
		spec.Mounts = append(spec.Mounts, "type=cgroup,src=cgroup,dst=/sys/fs/cgroup,options=rw:nosuid:noexec:nodev:relatime")
	}
	for _, vol := range meta.Volumes {
		if err := os.MkdirAll(vol.Source, 0o755); err != nil {
			return err
		}
		options := vol.Options
		if !contains(options, "ro") && !contains(options, "rw") {
			options = append(slices.Clone(options), "rw")
		}
		bind(vol.Source, vol.Destination, options...)
	}
	for _, tmpfs := range meta.Tmpfs {
		spec.Mounts = append(spec.Mounts, "type=tmpfs,src=tmpfs,dst="+tmpfs.Destination+",options="+strings.Join(tmpfs.Options, ":"))
	}
	if err := e.ctrCreate(spec); err != nil {
		return err
	}
	err := e.copyUp(meta)
	if err == nil {
		err = e.saveContainer(meta)
	}
	if err != nil {
		if derr := e.ctrDeleteContainer(meta.ID); derr != nil {
			err = errors.Join(err, derr)
		}
	}
	return err
}

// writeContainerFiles writes what /etc/hostname, /etc/hosts and /etc/resolv.conf
// of the container start from. The hosts file is rewritten when the container
// is attached to its network, and resolv.conf at every start, from what was
// decided at create. A container created before that was recorded has no
// record, and gets what the environment of this start says.
func (e *engine) writeContainerFiles(meta *containerMeta) error {
	dir := e.containerDir(meta.ID)
	resolv := meta.ResolvConf
	if resolv == "" {
		var err error
		if resolv, err = e.cfg.resolvConf(); err != nil {
			return err
		}
	}
	for name, content := range map[string]string{
		flagHostname: meta.Hostname + "\n",
		fileHosts:    "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n",
		fileResolv:   resolv,
	} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); name == fileHosts && err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // read by the container's processes
			return err
		}
	}
	return nil
}

// copyUp fills each anonymous volume with what the image has at its path, as
// docker does: kind's /var is a volume, and an empty one has no package database.
func (e *engine) copyUp(meta *containerMeta) error {
	var anonymous []volume
	for _, vol := range meta.Volumes {
		if vol.Anonymous {
			anonymous = append(anonymous, vol)
		}
	}
	if len(anonymous) == 0 {
		return nil
	}
	return e.withSnapshot(meta.ID, func(mnt string) error {
		root, err := os.OpenRoot(mnt)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, vol := range anonymous {
			// The path is looked up inside the image: a symlink that leads out of it
			// is not followed, so the copy cannot be made to read the host.
			inImage := strings.TrimPrefix(filepath.Clean("/"+vol.Destination), "/")
			if inImage == "" {
				inImage = "."
			}
			if info, err := root.Stat(inImage); err == nil && info.IsDir() {
				if _, err := e.output("cp", "-a", filepath.Join(mnt, inImage)+"/.", vol.Source); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (e *engine) exitCodeFile(id string) string { return filepath.Join(e.containerDir(id), "exitcode") }

// isRunning reports whether the container has a running task.
func (e *engine) isRunning(id string) (bool, error) {
	tasks, err := e.ctrTasks()
	return tasks[id] == "RUNNING", err
}

// startContainer attaches the container to its network and starts its task under a supervisor.
// A start the restart policy asks for (automatic) gives way to a stop or to another
// start that happened meanwhile; any other start ends the stop and the policy's count.
func (e *engine) startContainer(meta *containerMeta, automatic bool) error {
	if running, err := e.isRunning(meta.ID); err != nil || running {
		return err
	}
	e.ctrDeleteTask(meta.ID)
	err := e.withLock(func() error {
		current, err := e.loadContainer(meta.ID)
		if err != nil {
			return err
		}
		took := automatic && current.Generation != meta.Generation
		*meta = *current
		switch {
		case automatic && (took || e.stoppedByUser(meta.ID)):
			return errStoppedByUser
		case !automatic:
			e.clearStopped(meta.ID)
		}
		meta.Generation++
		if meta.Network == hostNetwork {
			return e.saveContainer(meta)
		}
		if err := e.writeContainerFiles(meta); err != nil {
			return err
		}
		return e.attach(meta)
	})
	if err != nil {
		return err
	}
	if err := e.superviseStart(meta); err != nil {
		// A container that cannot be started stays stopped, whatever its policy.
		e.markStopped(meta.ID)
		_ = e.withLock(func() error { return e.detach(meta) })
		return err
	}
	return e.withLock(func() error {
		current, err := e.loadContainer(meta.ID)
		if err != nil {
			return err
		}
		current.StartedAt, current.FinishedAt, current.ExitCode = time.Now().UTC(), time.Time{}, 0
		*meta = *current
		return e.saveContainer(meta)
	})
}

func (e *engine) start(args []string) error {
	flags, err := parseFlags("start", flagSet{}, args, false)
	if err != nil {
		return err
	}
	return e.eachContainer(flags.args, "", func(meta *containerMeta) error { return e.startContainer(meta, false) })
}

// eachContainer applies fn to every named container and prints the name given;
// it goes on after a failure and reports them all.
func (e *engine) eachContainer(refs []string, ignoreMissing string, fn func(*containerMeta) error) error {
	if len(refs) == 0 {
		return errors.New("at least one container is required")
	}
	var failures []error
	for _, ref := range refs {
		meta, err := e.findContainer(ref)
		if err == nil {
			err = fn(meta)
		}
		switch {
		case err == nil:
			_, _ = fmt.Fprintln(e.stdout, ref)
		case ignoreMissing != "" && strings.HasPrefix(err.Error(), "no such container"):
		default:
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// stopContainer stops the task: the image's stop signal, then SIGKILL after the
// grace period. A zero period kills at once. It then detaches the container from
// its network. Stopping a container that is not running is not an error.
func (e *engine) stopContainer(meta *containerMeta, grace time.Duration) error {
	e.markStopped(meta.ID)
	tasks, err := e.ctrTasks()
	if err != nil {
		return err
	}
	if _, ok := tasks[meta.ID]; ok {
		signal := "SIGKILL"
		if grace > 0 {
			signal = stopSignalNumber(e.ctrStopSignal(meta.ID))
		}
		_ = e.ctrKill(meta.ID, signal)
		if !e.waitForExit(meta.ID, grace) {
			_ = e.ctrKill(meta.ID, "SIGKILL")
			if !e.waitForExit(meta.ID, 30*time.Second) {
				return fmt.Errorf("container %s did not stop", meta.Name)
			}
		}
		e.ctrDeleteTask(meta.ID)
	}
	return e.withLock(func() error {
		current, err := e.loadContainer(meta.ID)
		if err != nil {
			return err
		}
		*meta = *current
		if _, ok := tasks[meta.ID]; ok {
			meta.ExitCode, meta.FinishedAt = e.recordedExitCode(meta.ID), time.Now().UTC()
		}
		return e.detach(meta)
	})
}

// waitForExit waits for the task to be gone, up to the grace period.
func (e *engine) waitForExit(id string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		tasks, err := e.ctrTasks()
		if err == nil && (tasks[id] == "" || tasks[id] == "STOPPED") {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// recordedExitCode reads what the supervisor wrote when the task ended, for up to a few seconds.
func (e *engine) recordedExitCode(id string) int {
	for range 30 {
		if data, err := os.ReadFile(e.exitCodeFile(id)); err == nil {
			if code, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return code
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return -1
}

// stopSignalNumber turns SIGRTMIN+3, the signal systemd images stop with, into a number.
func stopSignalNumber(signal string) string {
	const sigrtmin = 34
	if n, ok := strings.CutPrefix(strings.TrimPrefix(signal, "SIG"), "RTMIN+"); ok {
		if offset, err := strconv.Atoi(n); err == nil {
			return strconv.Itoa(sigrtmin + offset)
		}
	}
	return signal
}

func (e *engine) stop(args []string) error {
	flags, err := parseFlags("stop", flagSet{"time": {Kind: stringFlag, Short: "t"}}, args, false)
	if err != nil {
		return err
	}
	grace, err := graceFromFlag(flags)
	if err != nil {
		return err
	}
	return e.eachContainer(flags.args, "", func(meta *containerMeta) error { return e.stopContainer(meta, grace) })
}

func graceFromFlag(flags *parsedFlags) (time.Duration, error) {
	value, ok := flags.set("time")
	if !ok {
		return 10 * time.Second, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("invalid --time %q", value)
	}
	return time.Duration(seconds) * time.Second, nil
}

// restart stops and starts the container: its snapshot and volumes stay, which is a reboot.
func (e *engine) restart(args []string) error {
	flags, err := parseFlags(flagRestart, flagSet{"time": {Kind: stringFlag, Short: "t"}}, args, false)
	if err != nil {
		return err
	}
	grace, err := graceFromFlag(flags)
	if err != nil {
		return err
	}
	return e.eachContainer(flags.args, "", func(meta *containerMeta) error {
		if err := e.stopContainer(meta, grace); err != nil {
			return err
		}
		return e.startContainer(meta, false)
	})
}

func (e *engine) remove(args []string) error {
	flags, err := parseFlags("rm", flagSet{
		"force": {Kind: boolFlag, Short: "f"}, "volumes": {Kind: boolFlag, Short: "v"},
	}, args, false)
	if err != nil {
		return err
	}
	ignore := ""
	if flags.has("force") {
		ignore = "no such container"
	}
	// The container's volumes are in its directory and go with it, with or without -v.
	return e.eachContainer(flags.args, ignore, func(meta *containerMeta) error {
		running, err := e.isRunning(meta.ID)
		if err != nil {
			return err
		}
		if running && !flags.has("force") {
			return fmt.Errorf("cannot remove container %s: it is running; stop it first or use -f", meta.Name)
		}
		if err := e.stopContainer(meta, 0); err != nil {
			if !flags.has("force") {
				return err
			}
			_, _ = fmt.Fprintln(e.stderr, "ctrd: removing anyway:", err)
		}
		if err := e.ctrDeleteContainer(meta.ID); err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}
		return e.removeContainerFiles(meta)
	})
}

// wait blocks until the containers have stopped and prints their exit codes.
func (e *engine) wait(args []string) error {
	var failures []error
	for _, ref := range args {
		meta, err := e.findContainer(ref)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		waited := false
		for !waited {
			waited = e.waitForExit(meta.ID, time.Second)
		}
		if meta, err = e.loadFinished(meta.ID); err != nil {
			failures = append(failures, err)
			continue
		}
		_, _ = fmt.Fprintln(e.stdout, meta.ExitCode)
	}
	return errors.Join(failures...)
}

// loadFinished loads a container whose task is gone. The supervisor records how
// the task ended a moment after that, so a started container is given a few seconds to be finished.
func (e *engine) loadFinished(id string) (*containerMeta, error) {
	for range 30 {
		meta, err := e.loadContainer(id)
		if err != nil || meta.StartedAt.IsZero() || !meta.FinishedAt.IsZero() {
			return meta, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return e.loadContainer(id)
}

// update implements `update --restart=POLICY`: the policy is recorded, and the supervisor reads it when the task ends.
func (e *engine) update(args []string) error {
	flags, err := parseFlags("update", flagSet{flagRestart: {Kind: stringFlag}}, args, false)
	if err != nil {
		return err
	}
	return e.eachContainer(flags.args, "", func(meta *containerMeta) error {
		return e.withLock(func() error {
			current, err := e.loadContainer(meta.ID)
			if err != nil {
				return err
			}
			current.Restart = flags.str(flagRestart)
			return e.saveContainer(current)
		})
	})
}
