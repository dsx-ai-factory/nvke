// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package darwinqemu implements a vm.Runtime backed by QEMU on macOS
// using vmnet-client for networking and QMP for machine control.
package darwinqemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

// New returns a vm.Runtime backed by QEMU on macOS with vmnet-client networking.
func New(qemuSystemPath, instancesDir string) vm.Runtime {
	return &darwinQEMU{
		qemuSystemPath:  qemuSystemPath,
		vmnetClientPath: findVMNetClient(),
		instancesDir:    instancesDir,
	}
}

// darwinQEMU manages QEMU VMs directly on macOS using vmnet-client for
// networking. All runtime state (PIDs, launch args) is persisted to disk
// so that VMs survive process restarts.
type darwinQEMU struct {
	qemuSystemPath  string
	vmnetClientPath string
	instancesDir    string
}

// darwinInstancePaths holds all file paths for a darwin-qemu VM instance,
// derived deterministically from the instance ID and the base instances
// directory. No in-memory state is needed to locate a running VM.
type darwinInstancePaths struct {
	Dir                string
	PIDFile            string
	VMNetClientPIDFile string
	QMPSock            string
	ManifestFile       string
	VMNetClientLog     string
}

func (r *darwinQEMU) pathsForInstance(instanceID string) darwinInstancePaths {
	dir := filepath.Join(r.instancesDir, instanceID)
	return darwinInstancePaths{
		Dir:                dir,
		PIDFile:            filepath.Join(dir, "qemu.pid"),
		VMNetClientPIDFile: filepath.Join(dir, "vmnet-client.pid"),
		QMPSock:            filepath.Join(os.TempDir(), qmpSocketName(instanceID)),
		ManifestFile:       filepath.Join(dir, "runtime.json"),
		VMNetClientLog:     filepath.Join(dir, "vmnet-client.log"),
	}
}

// darwinRuntimeManifest is persisted alongside each VM so that Reboot can
// re-launch vmnet-client with the original arguments after a process restart.
type darwinRuntimeManifest struct {
	LaunchPath string   `json:"launch_path"`
	LaunchArgs []string `json:"launch_args"`
}

func (r *darwinQEMU) EnsureBridge(ctx context.Context, _, _ string) error {
	if goruntime.GOOS != "darwin" {
		return fmt.Errorf("darwin-qemu backend requires darwin host, found %s", goruntime.GOOS)
	}
	return r.preflightVMNetClient(ctx)
}

func (r *darwinQEMU) preflightVMNetClient(_ context.Context) error {
	_, err := r.requireVMNetClient()
	return err
}

func (r *darwinQEMU) requireVMNetClient() (string, error) {
	vmnetClientPath := strings.TrimSpace(r.vmnetClientPath)
	if vmnetClientPath == "" {
		return "", fmt.Errorf(
			"darwin-qemu requires vmnet-client (nirs/vmnet-helper); run `make install-deps-homebrew` or install vmnet-helper so vmnet-client is available",
		)
	}
	info, err := os.Stat(vmnetClientPath)
	if err != nil {
		return "", fmt.Errorf("darwin-qemu vmnet-client check failed for %q: %w", vmnetClientPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("darwin-qemu vmnet-client path %q is a directory", vmnetClientPath)
	}
	return vmnetClientPath, nil
}

func (r *darwinQEMU) CreateAndStart(ctx context.Context, spec vm.InstanceSpec) (string, error) {
	if goruntime.GOOS != "darwin" {
		return "", fmt.Errorf("darwin-qemu backend requires darwin host, found %s", goruntime.GOOS)
	}
	vmnetClientPath, err := r.requireVMNetClient()
	if err != nil {
		return "", err
	}

	paths := r.pathsForInstance(spec.InstanceID)
	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.VMNetClientPIDFile)
	_ = os.Remove(paths.QMPSock)

	machine, cpu, firmwareArg := platformArgs(r.qemuSystemPath)

	baseArgs := []string{
		"-name", spec.DomainName,
		"-machine", machine,
		"-cpu", cpu,
		"-smp", strconv.Itoa(spec.VCPUs),
		"-m", strconv.Itoa(spec.MemoryMiB),
		"-drive", fmt.Sprintf("if=virtio,format=qcow2,file=%s", spec.DiskPath),
		"-drive", fmt.Sprintf("if=virtio,media=cdrom,format=raw,file=%s", spec.SeedISOPath),
		"-display", "none",
		"-serial", fmt.Sprintf("file:%s", filepath.Join(paths.Dir, "console.log")),
		"-pidfile", paths.PIDFile,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", paths.QMPSock),
		"-no-shutdown",
	}
	baseArgs = append(baseArgs, firmwareArg...)
	deviceArg := "virtio-net-pci,netdev=net0"
	if strings.TrimSpace(spec.NICMAC) != "" {
		deviceArg = fmt.Sprintf("%s,mac=%s", deviceArg, spec.NICMAC)
	}

	qemuArgs := append([]string{}, baseArgs...)
	qemuArgs = append(qemuArgs,
		"-netdev", "dgram,id=net0,local.type=fd,local.str=4",
		"-device", deviceArg,
	)
	clientArgs := []string{"--unprivileged", "--operation-mode", "shared"}
	clientArgs = append(clientArgs, "--")
	clientArgs = append(clientArgs, r.qemuSystemPath)
	clientArgs = append(clientArgs, qemuArgs...)

	vmnetPID, err := launchVMNetClient(vmnetClientPath, clientArgs, paths.VMNetClientLog)
	if err != nil {
		return "", err
	}

	if err := writePIDFile(paths.VMNetClientPIDFile, vmnetPID); err != nil {
		_ = stopProcessGroup(vmnetPID)
		return "", fmt.Errorf("failed to write vmnet-client pid file: %w", err)
	}

	if err := writeRuntimeManifest(paths.ManifestFile, darwinRuntimeManifest{
		LaunchPath: vmnetClientPath,
		LaunchArgs: append([]string{}, clientArgs...),
	}); err != nil {
		_ = stopProcessGroup(vmnetPID)
		return "", fmt.Errorf("failed to write runtime manifest: %w", err)
	}

	if err := waitForPIDFile(paths.PIDFile, 10*time.Second); err != nil {
		_ = stopProcessGroup(vmnetPID)
		logBytes, _ := os.ReadFile(paths.VMNetClientLog)
		return "", fmt.Errorf("qemu did not start (pidfile %s): %w; log: %s", paths.PIDFile, err, truncateForDebug(string(logBytes), 2000))
	}

	return spec.InstanceID, nil
}

func (r *darwinQEMU) Reboot(_ context.Context, machineID string) error {
	paths := r.pathsForInstance(machineID)
	return qmpReboot(paths.QMPSock)
}

func (r *darwinQEMU) TPMEKCert(context.Context, string) (string, error) { return "", nil }

func (r *darwinQEMU) Delete(_ context.Context, machineID string) error {
	paths := r.pathsForInstance(machineID)

	if vmnetPID, err := readPIDFile(paths.VMNetClientPIDFile); err == nil {
		if stopErr := stopProcessGroup(vmnetPID); stopErr != nil {
			if pidErr := stopPIDFileProcess(paths.PIDFile); pidErr != nil {
				return fmt.Errorf("failed stopping process group (pgid %d): %w; pidfile fallback also failed: %v",
					vmnetPID, stopErr, pidErr)
			}
		}
	} else {
		_ = stopPIDFileProcess(paths.PIDFile)
	}

	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.VMNetClientPIDFile)
	_ = os.Remove(paths.QMPSock)
	_ = os.Remove(paths.ManifestFile)
	return nil
}

func (r *darwinQEMU) GetState(_ context.Context, machineID string) (vm.MachineState, error) {
	paths := r.pathsForInstance(machineID)
	pid, err := readPIDFile(paths.PIDFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return vm.MachineStateShutOff, nil
		}
		return vm.MachineStateNotFound, err
	}
	alive, err := isPIDAlive(pid)
	if err != nil {
		return vm.MachineStateNotFound, err
	}
	if alive {
		return vm.MachineStateRunning, nil
	}
	return vm.MachineStateCrashed, nil
}

func (r *darwinQEMU) IsRunning(ctx context.Context, machineID string) (bool, error) {
	s, err := r.GetState(ctx, machineID)
	return s == vm.MachineStateRunning, err
}

func (r *darwinQEMU) SupportsTCPProbe() bool {
	return true
}

func (r *darwinQEMU) ListMachines(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(r.instancesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read instances dir: %w", err)
	}
	var machines []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		instanceID := entry.Name()
		paths := r.pathsForInstance(instanceID)
		pid, pidErr := readPIDFile(paths.PIDFile)
		if pidErr != nil {
			continue
		}
		alive, _ := isPIDAlive(pid)
		if alive {
			machines = append(machines, instanceID)
		}
	}
	return machines, nil
}

// LookupDHCPLease is unsupported on the darwin/vmnet backend (it relies on
// vmnet's external DHCP, not a libvirt network). libvirt-network mode is
// Linux-only, so this is never called here.
func (r *darwinQEMU) LookupDHCPLease(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// --- process helpers ---

// stopVMProcesses tears down the vmnet-client process group (which includes
// QEMU) and cleans up stale PID/socket files in preparation for a relaunch.
func stopVMProcesses(paths darwinInstancePaths) {
	if vmnetPID, err := readPIDFile(paths.VMNetClientPIDFile); err == nil {
		if stopErr := stopProcessGroup(vmnetPID); stopErr != nil {
			_ = stopPIDFileProcess(paths.PIDFile)
		}
	} else {
		_ = stopPIDFileProcess(paths.PIDFile)
	}
	_ = os.Remove(paths.QMPSock)
	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.VMNetClientPIDFile)
}

// launchVMNetClient starts vmnet-client as a long-running foreground process.
// vmnet-client manages both vmnet-helper and QEMU as children; QEMU must NOT
// use -daemonize so that vmnet-helper stays alive for the VM's lifetime.
//
// The process is started in its own process group (Setpgid) so that
// stopProcessGroup can tear down the entire tree (vmnet-client + vmnet-helper
// + qemu) in one shot.  Returns the vmnet-client PID (== PGID).
func launchVMNetClient(vmnetClientPath string, clientArgs []string, logPath string) (int, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create vmnet-client log %s: %w", logPath, err)
	}

	cmd := exec.Command(vmnetClientPath, clientArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, fmt.Errorf("vmnet-client start failed: %w", err)
	}

	pid := cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()
	return pid, nil
}

func waitForPIDFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := readPIDFile(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pidfile %s not created within %v", path, timeout)
}

func readPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file %s: %w", path, err)
	}
	return pid, nil
}

func writePIDFile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

func writeRuntimeManifest(path string, manifest darwinRuntimeManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readRuntimeManifest(path string) (darwinRuntimeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return darwinRuntimeManifest{}, err
	}
	var m darwinRuntimeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return darwinRuntimeManifest{}, err
	}
	return m, nil
}

func isPIDAlive(pid int) (bool, error) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	err = proc.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, nil
	}
}

// stopProcessGroup kills a process and all members of its process group.
// The target must have been started with Setpgid: true so its PGID equals
// its PID.  Sends SIGTERM to the group first, escalating to SIGKILL.
func stopProcessGroup(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	alive, err := isPIDAlive(pgid)
	if err != nil || !alive {
		return nil
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := isPIDAlive(pgid)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	for i := 0; i < 10; i++ {
		alive, err := isPIDAlive(pgid)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("process group %d still running after SIGKILL", pgid)
}

func stopProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive, aliveErr := isPIDAlive(pid)
		if aliveErr != nil {
			return aliveErr
		}
		if !alive {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	_ = proc.Signal(syscall.SIGKILL)
	for i := 0; i < 10; i++ {
		alive, aliveErr := isPIDAlive(pid)
		if aliveErr != nil {
			return aliveErr
		}
		if !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("process %d still running after SIGKILL", pid)
}

func stopPIDFileProcess(pidFile string) error {
	pid, err := readPIDFile(pidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return stopProcess(pid)
}

// --- platform helpers ---

func platformArgs(qemuSystemPath string) (machine, cpu string, firmwareArgs []string) {
	base := strings.ToLower(filepath.Base(qemuSystemPath))
	if strings.Contains(base, "aarch64") || strings.Contains(base, "arm64") {
		firmware := firstExistingFile(
			"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
			"/usr/local/share/qemu/edk2-aarch64-code.fd",
		)
		if firmware != "" {
			return "virt,accel=hvf", "host", []string{"-bios", firmware}
		}
		return "virt,accel=hvf", "host", nil
	}
	// x86_64: try OVMF for UEFI boot (required for production qcow2 images)
	firmware := firstExistingFile(
		"/opt/homebrew/share/qemu/edk2-x86_64-code.fd",
		"/usr/local/share/qemu/edk2-x86_64-code.fd",
	)
	if firmware != "" {
		return "q35,accel=hvf", "host", []string{"-bios", firmware}
	}
	return "q35,accel=hvf", "host", nil
}

func firstExistingFile(paths ...string) string {
	for _, p := range paths {
		info, err := os.Stat(p)
		if err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// qmpSocketName returns a deterministic socket name for the QMP
// control channel. The name is stable across process restarts so that
// the socket path can be derived from the instance ID alone.
func qmpSocketName(instanceID string) string {
	shortID := instanceID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("nvm-%s.sock", shortID)
}

func looksLikeVMNetPermissionIssue(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "vmnet") &&
		(strings.Contains(lower, "privilege") ||
			strings.Contains(lower, "not enough privileges") ||
			strings.Contains(lower, "cannot create vmnet interface"))
}

func truncateForDebug(value string, maxLen int) string {
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

func qmpReboot(socketPath string) error {
	c := NewQMPClient(socketPath)
	if err := c.Connect(); err != nil {
		return err
	}
	defer c.Close()
	return c.GracefulReboot(20 * time.Second)
}

// findVMNetClient returns the path to the vmnet-client binary from
// nirs/vmnet-helper if installed, or empty string if not found.
func findVMNetClient() string {
	candidates := []string{
		"/opt/vmnet-helper/bin/vmnet-client",
		"/opt/homebrew/opt/vmnet-helper/libexec/vmnet-client",
		"/usr/local/opt/vmnet-helper/libexec/vmnet-client",
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}
