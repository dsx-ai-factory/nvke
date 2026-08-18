// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	fvmconfig "github.com/NVIDIA/nke/controller/forge-vms/internal/config"
)

// Config is an alias for the forge-vms config type so test packages don't
// need to import the internal config package directly.
type Config = fvmconfig.Config

const (
	VMBackendMock       = fvmconfig.VMBackendMock
	VMBackendLibvirt    = fvmconfig.VMBackendLibvirt
	VMBackendDarwinQEMU = fvmconfig.VMBackendDarwinQEMU
)

// ErrUnsupportedPlatform is returned by BuildRealVMProfile when the requested
// backend cannot run on the current OS. Callers should t.Skip rather than t.Fatal.
var ErrUnsupportedPlatform = errors.New("unsupported platform")

// NewMockRuntimeProfile returns a profile for the mock VM backend (fast, no real VMs).
func NewMockRuntimeProfile(t *testing.T) RuntimeProfile {
	t.Helper()

	t.Setenv("FORGE_VMS_VM_BACKEND", fvmconfig.VMBackendMock)
	t.Setenv("FORGE_VMS_LIBVIRT_URI", "mock://local")

	SetDefaultEnv(t, "FORGE_VMS_LOG_FORMAT", fvmconfig.LogFormatText)
	SetDefaultEnv(t, "FORGE_VMS_READINESS_PROBE_INTERVAL", "25ms")
	SetDefaultEnv(t, "FORGE_VMS_READINESS_TIMEOUT", "2s")
	SetDefaultEnv(t, "FORGE_VMS_VM_VCPUS", "2")
	SetDefaultEnv(t, "FORGE_VMS_VM_MEMORY_MIB", "2048")
	SetDefaultEnv(t, "FORGE_VMS_VM_DISK_GIB", "20")
	SetDefaultEnv(t, "FORGE_VMS_RECONCILE_WORKERS", "2")
	SetDefaultEnv(t, "FORGE_VMS_RECONCILE_INTERVAL", "20ms")
	SetDefaultEnv(t, "FORGE_VMS_FLOW_TIMEOUT", "3s")
	SetDefaultEnv(t, "FORGE_VMS_FLOW_POLL_INTERVAL", "50ms")

	cfg := MustParseConfig(t)
	cfg.ListenAddr = ":0"
	cfg.DBPath = filepath.Join(t.TempDir(), "forge-vms.db")
	cfg.SeedPath = ""
	cfg.BaseImage = ""
	cfg.ConsoleSSHPort = 0 // auto-assign for tests

	return RuntimeProfile{
		Name:         cfg.EffectiveVMBackend(),
		Config:       cfg,
		ReadyTimeout: DurationEnvOrDefault(t, "FORGE_VMS_FLOW_TIMEOUT", 3*time.Second),
		PollInterval: DurationEnvOrDefault(t, "FORGE_VMS_FLOW_POLL_INTERVAL", 50*time.Millisecond),
	}
}

// NewRealVMProfile is a convenience wrapper around BuildRealVMProfile that skips
// or fatals on the calling test. Do NOT call this inside sync.Once.Do — use
// BuildRealVMProfile instead to avoid sealing the Once on t.Skip/t.Fatal.
func NewRealVMProfile(t *testing.T, backend string) RuntimeProfile {
	t.Helper()
	profile, err := BuildRealVMProfile(backend)
	if err != nil {
		if errors.Is(err, ErrUnsupportedPlatform) {
			t.Skipf("%v", err)
		}
		t.Fatalf("%v", err)
	}
	return profile
}

// BuildRealVMProfile builds a RuntimeProfile for a real VM backend without
// calling any testing.T methods, making it safe to use inside sync.Once.Do.
// Returns ErrUnsupportedPlatform (wrapped) when the backend cannot run on the
// current OS so callers can distinguish skip vs hard failure.
func BuildRealVMProfile(backend string) (RuntimeProfile, error) {
	switch backend {
	case fvmconfig.VMBackendDarwinQEMU:
		if runtime.GOOS != "darwin" {
			return RuntimeProfile{}, fmt.Errorf("%w: darwin-qemu backend requires macOS (got %s)", ErrUnsupportedPlatform, runtime.GOOS)
		}
	case fvmconfig.VMBackendLibvirt:
		if runtime.GOOS != "linux" && !IsRunningInsideLima() {
			return RuntimeProfile{}, fmt.Errorf("%w: libvirt backend requires Linux or Lima (got %s)", ErrUnsupportedPlatform, runtime.GOOS)
		}
	default:
		return RuntimeProfile{}, fmt.Errorf("unsupported real VM backend: %q", backend)
	}

	if os.Getenv("FORGE_VMS_BASE_IMAGE") == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		imagePath, err := EnsureDefaultBaseImage(ctx)
		if err != nil {
			return RuntimeProfile{}, fmt.Errorf("auto-download base image: %w", err)
		}
		os.Setenv("FORGE_VMS_BASE_IMAGE", imagePath)
	}

	if os.Getenv("FORGE_VMS_LOG_FORMAT") == "" {
		os.Setenv("FORGE_VMS_LOG_FORMAT", fvmconfig.LogFormatText)
	}

	cfg, err := fvmconfig.Parse(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return RuntimeProfile{}, fmt.Errorf("config parse failed: %w", err)
	}
	cfg.VMBackend = backend
	cfg.ConsoleSSHPort = 0 // auto-assign for tests

	if os.Getenv("FORGE_VMS_READINESS_PROBE_INTERVAL") == "" {
		cfg.ReadinessProbeInterval = 3 * time.Second
	}
	if os.Getenv("FORGE_VMS_READINESS_TIMEOUT") == "" {
		cfg.ReadinessTimeout = 15 * time.Minute
	}
	if os.Getenv("FORGE_VMS_RECONCILE_WORKERS") == "" {
		cfg.ReconcileWorkers = 1
	}
	if os.Getenv("FORGE_VMS_RECONCILE_INTERVAL") == "" {
		cfg.ReconcileInterval = 2 * time.Second
	}
	if os.Getenv("FORGE_VMS_CLEANUP_ORPHANED_VMS") == "" {
		cfg.CleanupOrphanedVMs = true
	}
	if IsRunningInsideLima() {
		if os.Getenv("FORGE_VMS_VM_VCPUS") == "" {
			cfg.VMVCPUs = 1
		}
		if os.Getenv("FORGE_VMS_VM_MEMORY_MIB") == "" {
			cfg.VMMemoryMiB = 1536
		}
	}

	scratchDir, err := buildScratchDir(backend)
	if err != nil {
		return RuntimeProfile{}, err
	}
	cfg.ListenAddr = ":0"
	cfg.DBPath = filepath.Join(scratchDir, "forge-vms-realvm.db")
	cfg.SeedPath = ""

	if err := cfg.Validate(); err != nil {
		return RuntimeProfile{}, fmt.Errorf("real VM config validation failed: %w", err)
	}

	effectiveBackend := cfg.EffectiveVMBackend()
	if effectiveBackend == fvmconfig.VMBackendMock {
		return RuntimeProfile{}, fmt.Errorf("effective backend resolved to %q; not supported for real VM tests", fvmconfig.VMBackendMock)
	}

	readyTimeout := durationEnvOr("FORGE_VMS_FLOW_TIMEOUT", 2*time.Minute)
	pollInterval := durationEnvOr("FORGE_VMS_FLOW_POLL_INTERVAL", 2*time.Second)

	return RuntimeProfile{
		Name:         effectiveBackend,
		Config:       cfg,
		ReadyTimeout: readyTimeout,
		PollInterval: pollInterval,
	}, nil
}

func buildScratchDir(name string) (string, error) {
	rootDir := filepath.Join(os.TempDir(), "forge-vms-integration")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return "", fmt.Errorf("create real VM scratch root failed: %w", err)
	}
	testDir := filepath.Join(
		rootDir,
		fmt.Sprintf("%s-%d", sanitizePathComponent(name), time.Now().UnixNano()),
	)
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		return "", fmt.Errorf("create real VM scratch dir failed: %w", err)
	}
	return testDir, nil
}

func durationEnvOr(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return v
}
