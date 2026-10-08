// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"strings"
	"testing"
)

// ctxCheckingRunner records the calls that received an already-canceled
// context, which is what an exec.CommandContext-backed runner would refuse to
// run.
type ctxCheckingRunner struct {
	*fakeRunner
	canceledCalls []string
}

func (r *ctxCheckingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	if ctx.Err() != nil {
		r.canceledCalls = append(r.canceledCalls, name+" "+joinArgs(args))
	}
	return r.fakeRunner.Run(ctx, env, name, args...)
}

// TestCreate_CleansUpAfterACancelledCreate pins the SIGINT case. With --retain
// the node container survives a failed create, so the delete has to run on a
// context that outlives the canceled one or the next bootstrap is blocked.
func TestCreate_CleansUpAfterACancelledCreate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &ctxCheckingRunner{fakeRunner: createFailureRunner()}

	if _, err := New(runner, nil).Create(ctx, Config{
		ClusterName:    "test-cluster",
		DiagnosticsDir: t.TempDir(),
	}); err == nil {
		t.Fatal("expected an error from a failing create")
	}

	deleted := false
	for _, call := range runner.calls {
		if call.name == "kind" && strings.HasPrefix(joinArgs(call.args), "delete cluster") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("the retained cluster was not deleted; calls were %+v", runner.calls)
	}
	for _, c := range runner.canceledCalls {
		if strings.HasPrefix(c, "kind delete cluster") {
			t.Errorf("the delete ran on the canceled context: %v", runner.canceledCalls)
		}
	}
}
