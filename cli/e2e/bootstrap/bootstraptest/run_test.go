// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e_bootstrap

package bootstraptest_test

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/e2e/bootstrap/bootstraptest"
)

// TestE2E_BootstrapSite runs one `nvkectl bootstrap site` and reports what it
// exited with. It stands in for the command itself, in the place the bootstrap
// lanes used to invoke it: run.sh and run-nico.sh start this in the
// background, watch the log file it writes, bridge their networks, and then
// wait for it, exactly as they did around `setsid nvkectl bootstrap site`.
//
// The lane configures the run through the environment; see env.go. It is not a
// lane of its own and adds no coverage: everything asserted about the run is
// still asserted by the lane, over NVKECTL_E2E_BOOTSTRAP_LOG.
//
// The TestE2E_ prefix is how the lanes select it; see the package comment.
//
// -timeout 0 matters when invoking it: a bootstrap outlasts the 10 minute
// default, and a go test timeout panics the process rather than stopping the
// command.
func TestE2E_BootstrapSite(t *testing.T) {
	// The test's working directory is its own package directory, so cli/ is
	// three levels up. NVKECTL overrides the binary this resolves anyway.
	opts, err := bootstraptest.BootstrapOptionsFromEnv("../../..")
	if err != nil {
		t.Fatalf("reading the run configuration: %v", err)
	}

	b, err := bootstraptest.StartBootstrap(context.Background(), opts, t.Logf)
	if err != nil {
		t.Fatalf("starting bootstrap site: %v", err)
	}
	t.Cleanup(b.Stop)
	stopOnSignal(t, b)

	if err := b.Wait(); err != nil {
		t.Fatalf("nvkectl bootstrap site exited %d: %v", b.ExitCode(), err)
	}
	t.Logf("bootstraptest: nvkectl bootstrap site exited 0")
}

// stopOnSignal ends the bootstrap when this process is signalled.
//
// The lane aborts by signalling this process's group, and nvkectl is not in
// it: it is given a group of its own so that the kind, helm and nerdctl it
// drives can be signalled as one. Without this, an aborted lane would leave
// nvkectl running and then tear down the cluster underneath it -- which is
// what the shell's `kill -- -$bootstrap_pid` existed to prevent.
func stopOnSignal(t *testing.T, b *bootstraptest.Bootstrap) {
	t.Helper()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(signals) })

	go func() {
		sig, ok := <-signals
		if !ok {
			return
		}
		// Not t.Fatalf: this is not the test's goroutine, and the lane is
		// already failing. Stop the command, then go, so the lane's teardown
		// does not race a bootstrap that is still driving the cluster.
		b.Stop()
		os.Stderr.WriteString("bootstraptest: stopped bootstrap site on " + sig.String() + "\n")
		os.Exit(1)
	}()
}
