// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func platformArgs(ctx context.Context, qemu, dir string) (string, string, []string, error) {
	machine, codeName, varsName := "q35,accel=hvf", "edk2-x86_64-code.fd", "edk2-i386-vars.fd"
	arm := strings.Contains(filepath.Base(qemu), "aarch64") || strings.Contains(filepath.Base(qemu), "arm64")
	if arm != (runtime.GOARCH == "arm64") {
		return "", "", nil, fmt.Errorf("QEMU %s does not match native %s architecture required by HVF; override with NICO_VMS_QEMU_SYSTEM_PATH", qemu, runtime.GOARCH)
	}
	if arm {
		machine, codeName, varsName = "virt,accel=hvf", "edk2-aarch64-code.fd", "edk2-arm-vars.fd"
	}
	code, vars, err := firmwarePair(ctx, qemu, codeName, varsName)
	if err != nil {
		return "", "", nil, err
	}
	privateVars := filepath.Join(dir, "efi-vars.fd")
	if err := copyVariables(vars, privateVars); err != nil {
		return "", "", nil, err
	}
	return machine, "host", []string{
		"-drive", "if=pflash,format=raw,readonly=on,file=" + code,
		"-drive", "if=pflash,format=raw,file=" + privateVars,
	}, nil
}

func firmwarePair(ctx context.Context, qemu, codeName, varsName string) (string, string, error) {
	code, vars := os.Getenv("NICO_VMS_EFI_CODE"), os.Getenv("NICO_VMS_EFI_VARS_TEMPLATE")
	if code != "" || vars != "" {
		if code == "" || vars == "" {
			return "", "", fmt.Errorf("set both NICO_VMS_EFI_CODE and NICO_VMS_EFI_VARS_TEMPLATE")
		}
		return code, vars, nil
	}
	// Ask the selected QEMU first. Its paths work for Flox, Nix and Homebrew alike.
	out, err := exec.CommandContext(ctx, qemu, "-L", "help").Output()
	if err != nil {
		return "", "", fmt.Errorf("query QEMU firmware directories: %w", err)
	}
	dirs := strings.Split(string(out), "\n")
	dirs = append(dirs, "/opt/homebrew/share/qemu", "/usr/local/share/qemu", "/usr/share/qemu")
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		c, v := filepath.Join(dir, codeName), filepath.Join(dir, varsName)
		ci, ce := os.Stat(c)
		vi, ve := os.Stat(v)
		if ce == nil && ve == nil && ci.Mode().IsRegular() && vi.Mode().IsRegular() {
			return c, v, nil
		}
	}
	return "", "", fmt.Errorf("no matching UEFI firmware pair found for %s; set NICO_VMS_EFI_CODE and NICO_VMS_EFI_VARS_TEMPLATE", qemu)
}

func copyVariables(src, dst string) error {
	// Preserve NVRAM when reconciling an existing instance.
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read EFI variables: %w", err)
	}
	// Write then rename, so a crash mid-write cannot leave a truncated dst that
	// the Stat above would keep forever.
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write EFI variables: %w", err)
	}
	return os.Rename(tmp, dst)
}
