// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package darwinqemu implements a vm.Runtime backed by QEMU on macOS
// using vmnet-helper's launcher for networking and QMP for machine control.
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

// New returns a vm.Runtime backed by QEMU on macOS with vmnet-helper networking.
func New(qemuSystemPath, instancesDir, accelerator string) vm.Runtime {
	return &darwinQEMU{
		qemuSystemPath:  qemuSystemPath,
		vmnetClientPath: findVMNetLauncher(),
		instancesDir:    instancesDir,
		accelerator:     accelerator,
	}
}

// darwinQEMU manages QEMU VMs directly on macOS using vmnet-client for
// networking. Live process handles own status and termination. Saved PIDs and
// launch arguments are descriptions and do not grant ownership after a restart.
type darwinQEMU struct {
	qemuSystemPath  string
	vmnetClientPath string
	instancesDir    string
	accelerator     string
	networkMu       sync.Mutex
	network         *vmnetNetwork
	machines        map[string]*ownedProcess
	tpms            map[string]*nativeTPM
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

// darwinRuntimeManifest records diagnostic launch arguments. It does not
// grant authority to reconstruct ownership after a process restart.
type darwinRuntimeManifest struct {
	LaunchPath string   `json:"launch_path"`
	LaunchArgs []string `json:"launch_args"`
}

func (r *darwinQEMU) EnsureBridge(ctx context.Context, _, bridgeCIDR string) error {
	if goruntime.GOOS != "darwin" {
		return fmt.Errorf("darwin-qemu backend requires darwin host, found %s", goruntime.GOOS)
	}
	if _, err := vm.ResolveQEMUAccelerator(r.accelerator); err != nil {
		return err
	}
	if err := r.preflightVMNetClient(ctx); err != nil {
		return err
	}
	return r.ensureNetwork(ctx, bridgeCIDR)
}

func (r *darwinQEMU) preflightVMNetClient(_ context.Context) error {
	_, err := r.requireVMNetClient()
	return err
}

func (r *darwinQEMU) requireVMNetClient() (string, error) {
	vmnetClientPath := strings.TrimSpace(r.vmnetClientPath)
	if vmnetClientPath == "" {
		return "", fmt.Errorf(
			"darwin-qemu requires vmnet-run or legacy vmnet-client (nirs/vmnet-helper); run `make install-deps-homebrew` or install vmnet-helper",
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
	machine, cpu, firmwareArg, err := platformArgs(r.qemuSystemPath, r.accelerator)
	if err != nil {
		return "", err
	}
	vmnetClientPath, err := r.requireVMNetClient()
	if err != nil {
		return "", err
	}
	clientArgs, err := vmnetLauncherArgs(vmnetClientPath)
	if err != nil {
		return "", err
	}
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	if r.network == nil {
		return "", fmt.Errorf("native VM creation requires an owned shared network; call EnsureBridge first")
	}
	select {
	case <-r.network.done:
		return "", fmt.Errorf("owned native shared network exited")
	default:
	}
	if _, exists := r.machines[spec.InstanceID]; exists {
		return "", fmt.Errorf("native VM %s already has an owned process", spec.InstanceID)
	}
	clientArgs = append(clientArgs, r.network.args...)

	paths := r.pathsForInstance(spec.InstanceID)
	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.VMNetClientPIDFile)
	_ = os.Remove(paths.QMPSock)

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
	tpm, err := prepareNativeTPM(ctx, paths.Dir)
	if err != nil && !errors.Is(err, errNativeTPMUnavailable) {
		return "", err
	}
	if tpm != nil {
		if r.tpms == nil {
			r.tpms = make(map[string]*nativeTPM)
		}
		r.tpms[spec.InstanceID] = tpm
		baseArgs = append(baseArgs, tpm.qemuArgs(r.qemuSystemPath)...)
	}
	deviceArg := "virtio-net-pci,netdev=net0"
	if strings.TrimSpace(spec.NICMAC) != "" {
		deviceArg = fmt.Sprintf("%s,mac=%s", deviceArg, spec.NICMAC)
	}

	qemuArgs := append([]string{}, baseArgs...)
	qemuArgs = append(qemuArgs,
		"-netdev", "dgram,id=net0,local.type=fd,local.str=4",
		"-device", deviceArg,
	)
	clientArgs = append(clientArgs, "--")
	clientArgs = append(clientArgs, r.qemuSystemPath)
	clientArgs = append(clientArgs, qemuArgs...)

	process, err := launchVMNetClient(vmnetClientPath, clientArgs, paths.VMNetClientLog)
	if err != nil {
		if tpm != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = tpm.process.stop(cleanup)
		}
		return "", err
	}
	if r.machines == nil {
		r.machines = make(map[string]*ownedProcess)
	}
	r.machines[spec.InstanceID] = process
	cleanupStarted := func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = process.stop(cleanup)
		delete(r.machines, spec.InstanceID)
		if tpm != nil {
			_ = tpm.process.stop(cleanup)
			delete(r.tpms, spec.InstanceID)
		}
	}
	vmnetPID := process.command.Process.Pid

	if err := writePIDFile(paths.VMNetClientPIDFile, vmnetPID); err != nil {
		cleanupStarted()
		return "", fmt.Errorf("failed to write vmnet-client pid file: %w", err)
	}

	if err := writeRuntimeManifest(paths.ManifestFile, darwinRuntimeManifest{
		LaunchPath: vmnetClientPath,
		LaunchArgs: append([]string{}, clientArgs...),
	}); err != nil {
		cleanupStarted()
		return "", fmt.Errorf("failed to write runtime manifest: %w", err)
	}

	if err := waitForPIDFile(paths.PIDFile, 10*time.Second); err != nil {
		cleanupStarted()
		logBytes, _ := os.ReadFile(paths.VMNetClientLog)
		return "", fmt.Errorf("qemu did not start (pidfile %s): %w; log: %s", paths.PIDFile, err, truncateForDebug(string(logBytes), 2000))
	}

	return spec.InstanceID, nil
}

func (r *darwinQEMU) Reboot(_ context.Context, machineID string) error {
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	process, owned := r.machines[machineID]
	if !owned {
		return fmt.Errorf("native VM %s has no live process ownership", machineID)
	}
	select {
	case <-process.done:
		return fmt.Errorf("owned native VM %s exited", machineID)
	default:
	}
	paths := r.pathsForInstance(machineID)
	return qmpReboot(paths.QMPSock)
}

func (r *darwinQEMU) TPMEKCert(_ context.Context, machineID string) (string, error) {
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	tpm := r.tpms[machineID]
	if tpm == nil {
		return "", nil
	}
	if _, owned := r.machines[machineID]; !owned {
		return "", fmt.Errorf("TPM certificate has no owned native VM")
	}
	select {
	case <-tpm.process.done:
		return "", fmt.Errorf("owned TPM exited: %v", tpm.process.err)
	default:
		return tpm.cert, nil
	}
}

func (r *darwinQEMU) Delete(ctx context.Context, machineID string) error {
	paths := r.pathsForInstance(machineID)
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	process, owned := r.machines[machineID]
	if !owned {
		if _, err := os.Stat(paths.PIDFile); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("native VM %s has no live process ownership; refusing PID-file cleanup", machineID)
	}
	if err := process.stop(ctx); err != nil {
		return fmt.Errorf("join owned native VM: %w", err)
	}
	if tpm := r.tpms[machineID]; tpm != nil {
		if err := tpm.process.stop(ctx); err != nil {
			return err
		}
		delete(r.tpms, machineID)
	}
	delete(r.machines, machineID)

	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.VMNetClientPIDFile)
	_ = os.Remove(paths.QMPSock)
	_ = os.Remove(paths.ManifestFile)
	return nil
}

func (r *darwinQEMU) GetState(_ context.Context, machineID string) (vm.MachineState, error) {
	paths := r.pathsForInstance(machineID)
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	process, owned := r.machines[machineID]
	if !owned {
		if _, err := os.Stat(paths.PIDFile); errors.Is(err, os.ErrNotExist) {
			return vm.MachineStateShutOff, nil
		}
		return vm.MachineStateNotFound, fmt.Errorf("native VM %s has no live process ownership", machineID)
	}
	select {
	case <-process.done:
		return vm.MachineStateCrashed, nil
	default:
		return vm.MachineStateRunning, nil
	}
}

func (r *darwinQEMU) IsRunning(ctx context.Context, machineID string) (bool, error) {
	s, err := r.GetState(ctx, machineID)
	return s == vm.MachineStateRunning, err
}

func (r *darwinQEMU) SupportsTCPProbe() bool {
	return true
}

func (r *darwinQEMU) ListMachines(_ context.Context) ([]string, error) {
	r.networkMu.Lock()
	defer r.networkMu.Unlock()
	var machines []string
	for id, process := range r.machines {
		select {
		case <-process.done:
		default:
			machines = append(machines, id)
		}
	}
	slices.Sort(machines)
	return machines, nil
}

// LookupDHCPLease is unsupported on the darwin/vmnet backend (it relies on
// vmnet's external DHCP, not a libvirt network). libvirt-network mode is
// Linux-only, so this is never called here.
func (r *darwinQEMU) LookupDHCPLease(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// --- process helpers ---

// launchVMNetClient starts vmnet-client as a long-running foreground process.
// vmnet-client manages both vmnet-helper and QEMU as children; QEMU must NOT
// use -daemonize so that vmnet-helper stays alive for the VM's lifetime.
//
// The process is started in its own process group (Setpgid) so that
// the launcher can terminate and join its children on shutdown. Returns
// the package-issued live process handle.
func launchVMNetClient(vmnetClientPath string, clientArgs []string, logPath string) (*ownedProcess, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create vmnet launcher log %s: %w", logPath, err)
	}

	cmd := exec.Command(vmnetClientPath, clientArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return startOwnedProcess(cmd, logFile)
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

// --- platform helpers ---

func platformArgs(qemuSystemPath, accelerator string) (machine, cpu string, firmwareArgs []string, err error) {
	selected, err := vm.ResolveQEMUAccelerator(accelerator)
	if err != nil {
		return "", "", nil, err
	}
	cpu = "host"
	if selected == vm.QEMUAcceleratorTCG {
		cpu = "max"
	}
	base := strings.ToLower(filepath.Base(qemuSystemPath))
	if strings.Contains(base, "aarch64") || strings.Contains(base, "arm64") {
		firmware := firstExistingFile(
			"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
			"/usr/local/share/qemu/edk2-aarch64-code.fd",
		)
		if firmware != "" {
			return "virt,accel=" + string(selected), cpu, []string{"-bios", firmware}, nil
		}
		return "virt,accel=" + string(selected), cpu, nil, nil
	}
	// x86_64: try OVMF for UEFI boot (required for production qcow2 images)
	firmware := firstExistingFile(
		"/opt/homebrew/share/qemu/edk2-x86_64-code.fd",
		"/usr/local/share/qemu/edk2-x86_64-code.fd",
	)
	if firmware != "" {
		return "q35,accel=" + string(selected), cpu, []string{"-bios", firmware}, nil
	}
	return "q35,accel=" + string(selected), cpu, nil, nil
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

// vmnetLauncherArgs uses the source-owned command interface for each launcher.
// vmnet-run replaced vmnet-client in vmnet-helper v0.11.0 and runs unprivileged
// on macOS 26 without the legacy --unprivileged option.
func vmnetLauncherArgs(path string) ([]string, error) {
	switch filepath.Base(path) {
	case "vmnet-run":
		return []string{"--operation-mode", "shared"}, nil
	case "vmnet-client":
		return []string{"--unprivileged", "--operation-mode", "shared"}, nil
	default:
		return nil, fmt.Errorf("unsupported vmnet launcher %q", path)
	}
}

// findVMNetLauncher prefers the current vmnet-run from nirs/vmnet-helper and
// supports legacy vmnet-client installations without modifying either tool.
func findVMNetLauncher() string {
	candidates := []string{
		"/opt/vmnet-helper/bin/vmnet-run",
		"/opt/homebrew/opt/vmnet-helper/libexec/vmnet-run",
		"/usr/local/opt/vmnet-helper/libexec/vmnet-run",
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
