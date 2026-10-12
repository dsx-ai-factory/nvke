//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/test/integration/testutil"
)

var (
	suiteOnce    sync.Once
	suiteEnv     *testutil.TestEnv
	suiteProfile testutil.RuntimeProfile
	suiteErr     error
)

// TestMain routes test execution on macOS through a Lima VM (where libvirt is
// available). On Linux or inside Lima it runs tests directly via m.Run().
// The -test.run filter is forwarded transparently into the Lima inner run.
func TestMain(m *testing.M) {
	if runtime.GOOS == "darwin" && !testutil.IsRunningInsideLima() {
		m := newMViaLima()
		os.Exit(m.run())
	}

	stopCleanup := installLimaSignalCleanupHandler()
	code := m.Run()
	stopCleanup()
	closeSuite()
	os.Exit(code)
}

func sharedSuite(t *testing.T) (*testutil.TestEnv, testutil.RuntimeProfile) {
	t.Helper()

	suiteOnce.Do(func() {
		profile, err := testutil.BuildRealVMProfile(testutil.VMBackendLibvirt)
		if err != nil {
			suiteErr = err
			return
		}
		suiteProfile = profile
		env, err := testutil.NewDetachedTestEnv(testutil.DefaultSeedDoc(), suiteProfile)
		if err != nil {
			suiteErr = fmt.Errorf("initialize shared libvirt suite: %w", err)
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

// installLimaSignalCleanupHandler ensures libvirt domains created by tests inside
// a Lima VM are cleaned up if the process is interrupted.
func installLimaSignalCleanupHandler() func() {
	if !shouldAttemptLimaCleanup() {
		return func() {}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	var cleanupOnce sync.Once
	runCleanup := func(reason string) {
		cleanupOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "nico-vms integration [libvirt]: running Lima cleanup (%s)\n", reason)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cleanupCancel()
			if err := runLimaArtifactsCleanup(cleanupCtx); err != nil {
				fmt.Fprintf(os.Stderr, "nico-vms integration [libvirt]: Lima cleanup failed (%s): %v\n", reason, err)
				return
			}
			fmt.Fprintf(os.Stderr, "nico-vms integration [libvirt]: Lima cleanup complete (%s)\n", reason)
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sig, ok := <-sigCh
		if !ok {
			return
		}
		runCleanup(fmt.Sprintf("signal=%s", sig))
		closeSuite()
		os.Exit(exitCodeForSignal(sig))
	}()

	return func() {
		signal.Stop(sigCh)
		close(sigCh)
		runCleanup("normal exit")
		<-done
	}
}

func shouldAttemptLimaCleanup() bool {
	if runtime.GOOS != "darwin" || testutil.IsRunningInsideLima() {
		return false
	}
	_, err := exec.LookPath("limactl")
	return err == nil
}

func exitCodeForSignal(sig os.Signal) int {
	signalValue, ok := sig.(syscall.Signal)
	if !ok {
		return 1
	}
	return 128 + int(signalValue)
}

// ── Direct libvirt tests (run on Linux or inside Lima) ───────────────────────

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

func TestIPXEProductionCloudInitFlow(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunIPXEProductionCloudInitFlow(t, env.WithT(t), profile)
}

func TestBGPECMPFlow(t *testing.T) {
	t.Parallel()
	env, profile := sharedSuite(t)
	testutil.RunBGPECMPFlow(t, env.WithT(t), profile)
}
