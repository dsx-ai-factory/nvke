//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/test/integration/testutil"
)

const (
	limaVMName                   = "nico-vms"
	limaForceCleanupOnStartupEnv = "NICO_VMS_LIMA_FORCE_CLEAN_ON_STARTUP"
	limaArtifactsCleanupCmd      = `set -euo pipefail
if command -v virsh >/dev/null 2>&1; then
  while IFS= read -r domain; do
    [ -z "$domain" ] && continue
    case "$domain" in
      nico-vm-*)
        virsh -c qemu:///system destroy "$domain" >/dev/null 2>&1 || true
        virsh -c qemu:///system undefine "$domain" --nvram >/dev/null 2>&1 || \
          virsh -c qemu:///system undefine "$domain" >/dev/null 2>&1 || true
        ;;
    esac
  done < <(virsh -c qemu:///system list --all --name || true)
fi
rm -rf /tmp/nico-vms-integration /tmp/nico-vms-realvm* || true
`
)

// mViaLima is a harness for running the libvirt integration tests via Lima.
type mViaLima struct {
	runFilter string
}

func newMViaLima() *mViaLima {
	runFilter := ""
	flag.Parse()
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		runFilter = f.Value.String()
	}
	return &mViaLima{runFilter: runFilter}
}

// runViaLima creates a MainTB, recovers panics from it, and delegates to
// doRunViaLima for the actual Lima orchestration. Called from TestMain on
// macOS when not already inside Lima.
func (m *mViaLima) run() int {
	tb := testutil.NewMainTB("lima-proxy")
	defer tb.RunCleanups()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(testutil.FatalError); ok {
				return
			}
			if _, ok := r.(testutil.SkipError); ok {
				return
			}
			panic(r)
		}
	}()

	runViaLima(tb, m.runFilter)

	if tb.Failed() {
		return 1
	}
	return 0
}

// runViaLima boots a Lima VM, cross-compiles the test binary for Linux, and
// re-executes it inside the VM. runFilter is the -test.run pattern to forward
// (empty means run all tests).
func runViaLima(tb testing.TB, runFilter string) {
	repoRoot := mustNicoVMSRepoRoot(tb)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	baseImagePath, err := testutil.EnsureBaseImage(ctx, filepath.Join(repoRoot, testutil.DefaultBaseImageRelPath))
	if err != nil {
		tb.Fatalf("ensure host base image: %v", err)
	}
	ensureLimaVMReady(tb, ctx, repoRoot, baseImagePath)
	if shouldForceLimaCleanupOnStartup() {
		tb.Logf("Force-cleaning libvirt artifacts inside Lima during test initialization")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := runLimaArtifactsCleanup(cleanupCtx); err != nil {
			tb.Fatalf("startup lima cleanup failed: %v", err)
		}
	} else {
		tb.Logf("Skipping startup Lima cleanup because %s is false", limaForceCleanupOnStartupEnv)
	}
	tb.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		cleanupLimaArtifacts(tb, cleanupCtx)
	})

	testBinaryPath := buildIntegrationTestBinaryForLima(tb, ctx, repoRoot)
	runLibvirtSuiteInLima(tb, ctx, repoRoot, baseImagePath, testBinaryPath, runFilter)
}

func mustNicoVMSRepoRoot(tb testing.TB) string {
	tb.Helper()
	root, err := testutil.FindRepoRoot()
	if err != nil {
		tb.Fatalf("resolve nico-vms repo root: %v", err)
	}
	return root
}

func ensureLimaVMReady(tb testing.TB, ctx context.Context, repoRoot, baseImagePath string) {
	tb.Helper()

	if _, err := exec.LookPath("limactl"); err != nil {
		tb.Fatalf("limactl is required for Lima real VM tests: %v", err)
	}

	created := false
	if !limaVMExists(tb, ctx, limaVMName) {
		tb.Logf("Lima VM %s does not exist", limaVMName)
		created = true
		cfgFile, err := os.CreateTemp("", "nico-vms-lima-*.yaml")
		if err != nil {
			tb.Fatalf("create lima config temp file failed: %v", err)
		}
		cfgPath := cfgFile.Name()
		tb.Logf("Creating config file %s for lima VM", cfgPath)
		if _, err := cfgFile.WriteString(limaConfigYAML(repoRoot)); err != nil {
			_ = cfgFile.Close()
			_ = os.Remove(cfgPath)
			tb.Fatalf("write lima config failed: %v", err)
		}
		if err := cfgFile.Close(); err != nil {
			_ = os.Remove(cfgPath)
			tb.Fatalf("close lima config failed: %v", err)
		}
		tb.Cleanup(func() {
			_ = os.Remove(cfgPath)
		})

		tb.Logf("Creating Lima VM %s with config file %s", limaVMName, cfgPath)
		runCommandOrFail(tb, ctx, "limactl", "create", "--name", limaVMName, cfgPath)
		tb.Logf("Lima VM %s created", limaVMName)
	}

	tb.Logf("Starting Lima VM %s", limaVMName)
	runCommandOrFail(tb, ctx, "limactl", "start", limaVMName)
	tb.Logf("Lima VM %s started", limaVMName)

	if !created {
		return
	}

	readyCheck := fmt.Sprintf(
		"set -euo pipefail; test -f %q; systemctl is-active --quiet libvirtd",
		baseImagePath,
	)
	waitForLimaProvisioning(tb, ctx, readyCheck)
}

func shouldForceLimaCleanupOnStartup() bool {
	if strings.TrimSpace(os.Getenv(limaForceCleanupOnStartupEnv)) == "" {
		return true
	}
	return testutil.IsTrueEnv(limaForceCleanupOnStartupEnv)
}

func limaVMExists(tb testing.TB, ctx context.Context, name string) bool {
	tb.Helper()

	tb.Logf("Checking if Lima VM %s exists", name)
	output := runCommandOrFail(tb, ctx, "limactl", "ls", "-q")
	for line := range strings.SplitSeq(output, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

func runLibvirtSuiteInLima(
	tb testing.TB,
	ctx context.Context,
	repoRoot, baseImagePath, testBinaryPath, runFilter string,
) {
	tb.Helper()

	runArg := ""
	if runFilter != "" {
		runArg = fmt.Sprintf("-test.run %q", runFilter)
	}

	cmd := fmt.Sprintf(
		"set -euo pipefail; "+
			"cd %q; "+
			"NICO_VMS_IN_LIMA=1 "+
			"NICO_VMS_VM_BACKEND=libvirt "+
			"NICO_VMS_BASE_IMAGE=%q "+
			"%q -test.v -test.timeout=60m %s",
		repoRoot,
		baseImagePath,
		testBinaryPath,
		runArg,
	)

	tb.Logf("Running libvirt suite in Lima VM %s (filter: %q)", limaVMName, runFilter)
	runCommandOrFail(tb, ctx, "limactl", "shell", limaVMName, "sudo", "bash", "-lc", cmd)
}

func cleanupLimaArtifacts(tb testing.TB, ctx context.Context) {
	tb.Helper()

	tb.Logf("Cleaning up Lima artifacts")
	if err := runLimaArtifactsCleanup(ctx); err != nil {
		tb.Errorf("lima cleanup failed: %v", err)
	}
}

func runLimaArtifactsCleanup(ctx context.Context) error {
	output, err := runCommand(ctx, "limactl", "shell", limaVMName, "sudo", "bash", "-lc", limaArtifactsCleanupCmd)
	if err != nil {
		return fmt.Errorf("lima cleanup command failed: %w\n%s", err, output)
	}
	return nil
}

func runCommandOrFail(tb testing.TB, ctx context.Context, name string, args ...string) string {
	tb.Helper()

	output, err := runCommand(ctx, name, args...)
	if err != nil {
		tb.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
	return output
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = io.MultiWriter(os.Stderr, &output)
	err := cmd.Run()
	return output.String(), err
}

func waitForLimaProvisioning(tb testing.TB, ctx context.Context, readyCheck string) {
	tb.Helper()

	tb.Logf("Waiting (20 minutes) for Lima VM %s to be provisioned", limaVMName)
	deadline := time.Now().Add(20 * time.Minute)
	for {
		output, err := runCommand(ctx, "limactl", "shell", limaVMName, "bash", "-lc", readyCheck)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("lima cloud-init provisioning did not complete in time: %v\n%s", err, output)
		}
		time.Sleep(5 * time.Second)
	}
}

func buildIntegrationTestBinaryForLima(tb testing.TB, ctx context.Context, repoRoot string) string {
	tb.Helper()

	targetArch := runtime.GOARCH
	if targetArch != "arm64" && targetArch != "amd64" {
		tb.Fatalf("unsupported GOARCH for Lima integration binary: %s", targetArch)
	}

	binDir := filepath.Join(repoRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		tb.Fatalf("create bin directory failed: %v", err)
	}

	testBinaryPath := filepath.Join(binDir, fmt.Sprintf("integration-libvirt-linux-%s.test", targetArch))
	tb.Logf("Building Linux integration test binary: %s", testBinaryPath)

	cmd := exec.CommandContext(
		ctx,
		"go",
		"test",
		"./test/integration/libvirt",
		"-tags=integration",
		"-c",
		"-o",
		testBinaryPath,
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+targetArch,
		"CGO_ENABLED=0",
	)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = io.MultiWriter(os.Stderr, &output)
	if err := cmd.Run(); err != nil {
		tb.Fatalf("build integration test binary failed: %v\n%s", err, output.String())
	}

	if err := os.Chmod(testBinaryPath, 0o755); err != nil {
		tb.Fatalf("chmod test binary failed: %v", err)
	}

	return testBinaryPath
}

func limaConfigYAML(projectDir string) string {
	vmTypeYAML := "vmType: qemu"
	if runtime.GOARCH == "arm64" {
		vmTypeYAML = "vmType: vz\nnestedVirtualization: true"
	}

	return fmt.Sprintf(`%s

images:
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
    arch: "x86_64"
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
    arch: "aarch64"

cpus: 4
memory: "8GiB"
disk: "40GiB"

containerd:
  system: false
  user: false

mounts:
  - location: %q
    writable: true

provision:
  - mode: system
    script: |
      #!/bin/bash
      set -eux
      export DEBIAN_FRONTEND=noninteractive

      apt-get update -qq
      apt-get install -y -qq \
        qemu-kvm \
        libvirt-daemon-system \
        libvirt-clients \
        cloud-image-utils \
        iproute2
      systemctl enable --now libvirtd
`, vmTypeYAML, projectDir)
}
