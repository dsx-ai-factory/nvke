// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package qemu implements the validator's boot tier: it boots a throwaway
// copy of a node image under QEMU and runs probes inside it over SSH.
//
// The channel is deliberate: the node images keep cloud-init active across
// boots (SPEC.md "Cloud-init input"), so a NoCloud seed carrying a temporary
// user and SSH key works identically for real images and test fixtures, and
// SSH over user-mode networking needs no root, no bridges and no guest
// agents. The image itself is never written: the drive is attached with
// -snapshot, so every boot starts from the artifact's exact bytes.
package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Options configure Boot.
type Options struct {
	// Arch is the guest architecture: amd64 | arm64. Defaults to the host's.
	Arch string
	// MemMB defaults to 2048.
	MemMB int
	// SMP defaults to 2.
	SMP int
	// SeedISO is a NoCloud seed image (see MakeSeed) carrying the probe
	// user and its SSH public key. Required: it is the probe channel.
	SeedISO string
	// SSHKeyPath is the private key matching the seed's public key.
	SSHKeyPath string
	// SSHUser defaults to "nvkeprobe".
	SSHUser string
	// FirmwarePath overrides UEFI firmware autodetection.
	FirmwarePath string
	// BootTimeout bounds the wait for SSH readiness. Defaults to 10 minutes
	// (generous enough for TCG; see the S1 measurements).
	BootTimeout time.Duration
	// QEMUSystem overrides the qemu binary, for tests.
	QEMUSystem string
}

// VM is one booted throwaway instance. Always Close it.
type VM struct {
	cmd     *exec.Cmd
	sshPort int
	sshKey  string
	sshUser string
	done    chan struct{}
}

// firmwareCandidates lists where UEFI firmware lives per architecture across
// the environments the harness runs in (Debian/Ubuntu packages, Homebrew).
var firmwareCandidates = map[string][]string{
	"arm64": {
		"/usr/share/AAVMF/AAVMF_CODE.fd",
		"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
		"/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
	},
	"amd64": {
		"/usr/share/OVMF/OVMF_CODE.fd",
		"/opt/homebrew/share/qemu/edk2-x86_64-code.fd",
	},
}

func defaults(opts Options) (Options, error) {
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.MemMB == 0 {
		opts.MemMB = 2048
	}
	if opts.SMP == 0 {
		opts.SMP = 2
	}
	if opts.SSHUser == "" {
		opts.SSHUser = "nvkeprobe"
	}
	if opts.BootTimeout == 0 {
		opts.BootTimeout = 10 * time.Minute
	}
	if opts.QEMUSystem == "" {
		bin, ok := map[string]string{"arm64": "qemu-system-aarch64", "amd64": "qemu-system-x86_64"}[opts.Arch]
		if !ok {
			return opts, fmt.Errorf("unsupported architecture %q", opts.Arch)
		}
		opts.QEMUSystem = bin
	}
	if opts.FirmwarePath == "" {
		for _, c := range firmwareCandidates[opts.Arch] {
			if _, err := os.Stat(c); err == nil {
				opts.FirmwarePath = c
				break
			}
		}
		if opts.FirmwarePath == "" {
			return opts, fmt.Errorf("no UEFI firmware found for %s; set Options.FirmwarePath", opts.Arch)
		}
	}
	if opts.SeedISO == "" || opts.SSHKeyPath == "" {
		return opts, fmt.Errorf("SeedISO and SSHKeyPath are required: the NoCloud seed is the probe channel")
	}
	return opts, nil
}

// accel picks the fastest available accelerator for a guest of this
// architecture: KVM on a matching Linux host, HVF on a matching macOS host,
// TCG otherwise (viable: boot is wait-dominated, not CPU-dominated).
func accel(arch string) string {
	if arch == runtime.GOARCH {
		if runtime.GOOS == "linux" {
			if _, err := os.Stat("/dev/kvm"); err == nil {
				return "kvm"
			}
		}
		if runtime.GOOS == "darwin" {
			return "hvf"
		}
	}
	return "tcg"
}

// freePort asks the kernel for an unused TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Boot starts a throwaway copy of the image and waits until the probe channel
// answers. The image file is opened read-only via -snapshot; all writes land
// in a transient overlay that dies with the VM.
func Boot(ctx context.Context, imagePath string, opts Options) (*VM, error) {
	opts, err := defaults(opts)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("allocating ssh forward port: %w", err)
	}

	machine := "q35"
	cpu := "max"
	if opts.Arch == "arm64" {
		machine = "virt"
	}
	a := accel(opts.Arch)
	if a == "kvm" || a == "hvf" {
		cpu = "host"
	}
	args := []string{
		"-machine", machine, "-accel", a, "-cpu", cpu,
		"-m", strconv.Itoa(opts.MemMB), "-smp", strconv.Itoa(opts.SMP),
		"-snapshot",
		"-bios", opts.FirmwarePath,
		"-drive", "if=virtio,format=qcow2,file=" + imagePath,
		"-drive", "if=virtio,format=raw,readonly=on,file=" + opts.SeedISO,
		// restrict=on: no guest egress even on a connected host; slirp DHCP
		// and the host-initiated hostfwd probe channel keep working.
		"-netdev", fmt.Sprintf("user,id=n0,restrict=on,hostfwd=tcp:127.0.0.1:%d-:22", port),
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none", "-serial", "none", "-monitor", "none",
	}
	cmd := exec.Command(opts.QEMUSystem, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", opts.QEMUSystem, err)
	}
	vm := &VM{cmd: cmd, sshPort: port, sshKey: opts.SSHKeyPath, sshUser: opts.SSHUser, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(vm.done) }()

	deadline := time.Now().Add(opts.BootTimeout)
	for {
		select {
		case <-vm.done:
			return nil, fmt.Errorf("qemu exited during boot: %s", strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			vm.Close()
			return nil, ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			vm.Close()
			return nil, fmt.Errorf("image did not become probeable within %s: %s", opts.BootTimeout, strings.TrimSpace(stderr.String()))
		}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		res, err := vm.Probe(probeCtx, "true")
		cancel()
		if err == nil && res.ExitCode == 0 {
			return vm, nil
		}
		time.Sleep(3 * time.Second)
	}
}

// Close terminates the VM. The -snapshot overlay evaporates with it.
func (v *VM) Close() {
	if v.cmd.Process != nil {
		_ = v.cmd.Process.Kill()
	}
	select {
	case <-v.done:
	case <-time.After(10 * time.Second):
	}
}

// sshArgs builds the non-interactive ssh invocation for one command.
func (v *VM) sshArgs(command string) []string {
	return []string{
		"-p", strconv.Itoa(v.sshPort),
		"-i", v.sshKey,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + os.DevNull,
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		v.sshUser + "@127.0.0.1",
		"--", command,
	}
}
