//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/nke/controller/nico-sim/test/integration/testutil"
)

var (
	suiteOnce    sync.Once
	suiteEnv     *testutil.TestEnv
	suiteProfile testutil.RuntimeProfile
	suiteErr     error
)

func TestMain(m *testing.M) {
	code := m.Run()
	closeSuite()
	os.Exit(code)
}

func sharedSuite(t *testing.T) (*testutil.TestEnv, testutil.RuntimeProfile) {
	t.Helper()

	suiteOnce.Do(func() {
		profile, err := testutil.BuildRealVMProfile(testutil.VMBackendDarwinQEMU)
		if err != nil {
			suiteErr = err
			return
		}
		suiteProfile = profile
		env, err := testutil.NewDetachedTestEnv(testutil.DefaultSeedDoc(), suiteProfile)
		if err != nil {
			suiteErr = fmt.Errorf("initialize shared darwin-qemu suite: %w", err)
			return
		}
		suiteEnv = env
	})

	if suiteErr != nil {
		if errors.Is(suiteErr, testutil.ErrUnsupportedPlatform) {
			t.Skipf("%v", suiteErr)
		}
		t.Fatalf("%v", suiteErr)
	}
	return suiteEnv, suiteProfile
}

func closeSuite() {
	if suiteEnv != nil {
		suiteEnv.Close()
		if dbPath := strings.TrimSpace(suiteProfile.Config.DBPath); dbPath != "" {
			_ = os.RemoveAll(filepath.Dir(dbPath))
		}
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestCreatePollReadyAndDeleteFlow(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunCreatePollReadyAndDeleteFlow(t, env.WithT(t), profile)
}

func TestPatchLabelsAndRebootFlow(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunPatchLabelsAndRebootFlow(t, env.WithT(t), profile)
}

func TestSSHVerification(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunSSHVerificationFlow(t, env.WithT(t), profile)
}

func TestVMToVMConnectivity(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunVMToVMConnectivityFlow(t, env.WithT(t), profile)
}

func TestIPXEBootFlow(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunIPXEBootFlow(t, env.WithT(t), profile)
}

func TestIPXEProductionImageFlow(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunIPXEProductionImageFlow(t, env.WithT(t), profile)
}
