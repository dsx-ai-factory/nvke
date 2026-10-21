// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build boot

// Integration suite for the boot tier. It boots whatever bootable cloud image
// VALIDATE_BOOT_IMAGE points at — a stock Ubuntu cloud image suffices: the
// harness mechanics (seed, boot, SSH channel, probe semantics) are
// image-agnostic, and the NVKE-specific checks are asserted to FAIL correctly
// on a non-NVKE image, which exercises the implementations through the real
// channel. A real node image (when available) flips those to pass without any
// code change. Driven by hack/test-boot-container.sh and the nightly CI lane.
package qemu

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
	bootchecks "github.com/dsx-ai-factory/nvke/validate/pkg/checks/boot"
)

func bootImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("VALIDATE_BOOT_IMAGE")
	if img == "" {
		t.Skip("VALIDATE_BOOT_IMAGE not set; run hack/test-boot-container.sh")
	}
	return img
}

// One VM serves every test in the suite: the probes are read-only, and a
// second TCG boot would double both the wall time and the flake surface (a
// loaded host has been measured pushing a single boot past ten minutes).
var (
	sharedVM   *VM
	sharedErr  error
	sharedOnce sync.Once
)

func bootVM(t *testing.T) *VM {
	t.Helper()
	img := bootImage(t)
	sharedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nvke-boot-test-*")
		if err != nil {
			sharedErr = err
			return
		}
		keyPath, pubKey, err := GenerateSSHKey(dir)
		if err != nil {
			sharedErr = err
			return
		}
		seed, err := MakeSeed(dir, "nvkeprobe", pubKey)
		if err != nil {
			sharedErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		sharedVM, sharedErr = Boot(ctx, img, Options{SeedISO: seed, SSHKeyPath: keyPath, BootTimeout: 18 * time.Minute})
	})
	require.NoError(t, sharedErr)
	return sharedVM
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedVM != nil {
		sharedVM.Close()
	}
	os.Exit(code)
}

func TestBootAndProbe(t *testing.T) {
	vm := bootVM(t)

	res, err := vm.Probe(context.Background(), "echo probe-ok")
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "probe-ok", strings.TrimSpace(res.Stdout))

	// Positive is-enabled through the real channel. Noble cloud images
	// socket-activate sshd: ssh.service reports disabled while ssh.socket is
	// the enabled unit (measured — asserting ssh.service here fails).
	res, err = vm.Probe(context.Background(), "systemctl is-enabled ssh.socket")
	require.NoError(t, err)
	assert.Equal(t, "enabled", strings.TrimSpace(res.Stdout))

	// Non-zero remote exits are data, not transport errors.
	res, err = vm.Probe(context.Background(), "false")
	require.NoError(t, err)
	assert.Equal(t, 1, res.ExitCode)

	// -snapshot: writes land in the overlay; the image file's bytes must not
	// change. Asserted indirectly: the write succeeds in-guest.
	res, err = vm.Probe(context.Background(), "touch /tmp/scratch && echo wrote")
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)

	// restrict=on: guest egress to a public address must fail while the
	// loopback probe channel keeps working.
	res, err = vm.Probe(context.Background(), "timeout 5 bash -c 'echo > /dev/tcp/1.1.1.1/80'")
	require.NoError(t, err)
	assert.NotEqual(t, 0, res.ExitCode, "guest egress to 1.1.1.1:80 must be blocked by restrict=on")
}

// TestNVKEChecksFailHonestlyOnForeignImage runs the real check
// implementations through the real channel against an image that is not an
// NVKE node image: every unit check must FAIL (not error, not pass), naming
// the missing unit — the exact behavior a tampered or mis-built node image
// would produce.
func TestNVKEChecksFailHonestlyOnForeignImage(t *testing.T) {
	vm := bootVM(t)
	in := checks.Inputs{Boot: vm}
	target := checks.Target{Variant: checks.VariantFDE}

	want := map[string]checks.Status{
		"SPEC-001":   checks.StatusFail, // no node agent unit here, either name generation
		"SPEC-002":   checks.StatusFail,
		"SPEC-003":   checks.StatusFail,
		"FDE-002":    checks.StatusFail,
		"SPEC-005":   checks.StatusFail, // stock kernel default is 120, not 60
		"COMPAT-003": checks.StatusSkip, // no target BOM in these inputs
	}
	for id, impl := range bootchecks.Impls() {
		out, err := impl(context.Background(), target, in)
		require.NoError(t, err, "%s must fail, not error", id)
		assert.Equal(t, want[id], out.Status, "%s: %s", id, out.Evidence)
		assert.NotEmpty(t, out.Evidence, id)
	}
}
