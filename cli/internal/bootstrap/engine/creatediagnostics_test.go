// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createFailureRunner scripts the two calls every failing Create makes first:
// the existence check, then a create that fails.
func createFailureRunner() *fakeRunner {
	return &fakeRunner{
		outputs: []fakeOutput{
			{output: "No kind clusters found.\n", err: errors.New("exit status 1")},
			{output: "failed to init node with kubeadm\n", err: errors.New("exit status 1")},
		},
	}
}

// TestCreate_RetainsTheNodeOnFailure pins the flag the collection depends on.
// kind deletes the node container as part of its own cleanup, so without
// --retain there is nothing left to read by the time Create returns.
func TestCreate_RetainsTheNodeOnFailure(t *testing.T) {
	runner := createFailureRunner()

	_, err := New(runner, nil).Create(context.Background(), Config{
		ClusterName:    "test-cluster",
		DiagnosticsDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected an error from a failing create")
	}

	createCall := runner.calls[1]
	if !strings.Contains(joinArgs(createCall.args), "--retain") {
		t.Errorf("create call lacks --retain: %s", joinArgs(createCall.args))
	}
}

// TestCreate_CollectsFromTheNodeOnFailure is the point of the change: the
// report exists, and it was gathered through the container runtime rather
// than kubectl, which cannot work when the API server is what failed.
func TestCreate_CollectsFromTheNodeOnFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cluster-diagnostics")
	runner := createFailureRunner()

	if _, err := New(runner, nil).Create(context.Background(), Config{
		ClusterName:    "test-cluster",
		DiagnosticsDir: dir,
	}); err == nil {
		t.Fatal("expected an error from a failing create")
	}

	report, err := os.ReadFile(filepath.Join(dir, createFailureReport))
	if err != nil {
		t.Fatalf("no create-failure report: %v", err)
	}
	for _, want := range []string{"kubelet journal", "kube-apiserver logs", "etcd logs"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report has no %q section:\n%s", want, report)
		}
	}

	var probed bool
	for _, call := range runner.calls {
		if call.name != RuntimeCLI {
			continue
		}
		probed = true
		if call.args[0] == "exec" && call.args[1] != controlPlaneNode("test-cluster") {
			t.Errorf("probe targets %q, want the control-plane node", call.args[1])
		}
	}
	if !probed {
		t.Errorf("nothing was collected through %s; calls were %+v", RuntimeCLI, runner.calls)
	}
}

// TestCreate_DeletesTheRetainedNodeOnFailure: --retain makes the failure path
// responsible for the cleanup kind used to do, so a caller that did not ask
// to keep the cluster must not be left with one.
func TestCreate_DeletesTheRetainedNodeOnFailure(t *testing.T) {
	runner := createFailureRunner()

	if _, err := New(runner, nil).Create(context.Background(), Config{
		ClusterName:    "test-cluster",
		DiagnosticsDir: t.TempDir(),
	}); err == nil {
		t.Fatal("expected an error from a failing create")
	}

	var deleted bool
	for _, call := range runner.calls {
		if call.name == "kind" && strings.HasPrefix(joinArgs(call.args), "delete cluster") {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the retained cluster was not deleted; calls were %+v", runner.calls)
	}
}

func TestCreate_KeepFailedClusterSkipsTheDelete(t *testing.T) {
	runner := createFailureRunner()

	if _, err := New(runner, nil).Create(context.Background(), Config{
		ClusterName:       "test-cluster",
		DiagnosticsDir:    t.TempDir(),
		KeepFailedCluster: true,
	}); err == nil {
		t.Fatal("expected an error from a failing create")
	}

	for _, call := range runner.calls {
		if call.name == "kind" && strings.HasPrefix(joinArgs(call.args), "delete cluster") {
			t.Errorf("KeepFailedCluster still deleted the cluster: %+v", runner.calls)
		}
	}
}

// TestDumpCreateFailure_SurvivesAnAbsentNode is the property that matters on
// the path this runs on: a create can fail before the container exists, and
// the caller already has a real error that must not be lost to this.
func TestDumpCreateFailure_SurvivesAnAbsentNode(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeRunner{
		outputs: []fakeOutput{{output: "no such container", err: errors.New("exit status 1")}},
	}

	New(runner, nil).dumpCreateFailure(context.Background(), Config{
		ClusterName:    "test-cluster",
		DiagnosticsDir: dir,
	})

	report, err := os.ReadFile(filepath.Join(dir, createFailureReport))
	if err != nil {
		t.Fatalf("no report written when every probe failed: %v", err)
	}
	if !strings.Contains(string(report), "no such container") {
		t.Errorf("the report did not record the probe failures:\n%s", report)
	}
}

// An empty DiagnosticsDir means the caller does not want a bundle, and must
// not get one in the working directory.
func TestDumpCreateFailure_WithoutADirCollectsNothing(t *testing.T) {
	runner := &fakeRunner{}
	New(runner, nil).dumpCreateFailure(context.Background(), Config{ClusterName: "test-cluster"})
	if len(runner.calls) != 0 {
		t.Errorf("collected with no DiagnosticsDir set: %+v", runner.calls)
	}
}
