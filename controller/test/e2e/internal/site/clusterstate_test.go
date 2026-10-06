// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The collector's whole value is that it separates "CWE is not ready" from
// "CWE is ready and the probe could not reach it", so the section that
// answers that has to be present and has to come first.
func TestClusterStateCollectsEndpointsFirst(t *testing.T) {
	script := clusterStateScript()
	endpoints := strings.Index(script, "get service,endpoints,endpointslice")
	if endpoints < 0 {
		t.Fatalf("the script does not read endpoints:\n%s", script)
	}
	for _, later := range []string{"get pods -o wide", "describe pods", "get events"} {
		if i := strings.Index(script, later); i >= 0 && i < endpoints {
			t.Errorf("%q is read before endpoints, which is the section that classifies the failure", later)
		}
	}
}

// Reads only. A collector that mutated the cluster it is reporting on would
// be rewriting the evidence the run is judged on.
func TestClusterStateOnlyReads(t *testing.T) {
	for _, verb := range []string{"delete", "apply", "patch", "edit", "scale", "create", "rollout", "cordon", "drain"} {
		if strings.Contains(clusterStateScript(), " "+verb+" ") {
			t.Errorf("the script runs kubectl %s; it must only read", verb)
		}
	}
}

// Every command is tolerated individually. A cluster with no CWE Deployment
// still has events worth reading, and the first failure must not stop them
// being collected.
func TestClusterStateToleratesEachCommand(t *testing.T) {
	script := clusterStateScript()
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		if strings.HasPrefix(line, "echo ") {
			continue
		}
		if !strings.HasSuffix(line, "|| true") {
			t.Errorf("command is not tolerated, so a failure before it hides the rest: %s", line)
		}
	}
}

// The script runs against the kubeconfig on the node, not a forward. If the
// forward is what broke, a collector that needed one would fail exactly where
// the probe did and report nothing.
func TestClusterStateRunsOnTheNode(t *testing.T) {
	script := clusterStateScript()
	if !strings.Contains(script, kubeconfigPath) {
		t.Errorf("the script does not use the node's kubeconfig %s:\n%s", kubeconfigPath, script)
	}
	if strings.Contains(script, "127.0.0.1") {
		t.Error("the script reaches the cluster through a forward, which is the thing that may have failed")
	}
}

func TestCollectClusterState(t *testing.T) {
	session := newFakeSession()
	session.fail = func(string) (string, error) { return "pod/nke-controller-0  0/1  CrashLoopBackOff", nil }

	dest := filepath.Join(t.TempDir(), "state.txt")
	if err := CollectClusterState(context.Background(), session, dest); err != nil {
		t.Fatalf("CollectClusterState: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "CrashLoopBackOff") {
		t.Errorf("the node's output was not written through: %q", got)
	}
	if session.ran("get service,endpoints,endpointslice") < 0 {
		t.Errorf("no command read endpoints:\n%s", strings.Join(session.runs, "\n"))
	}
}

// A failure to collect still writes what arrived. "kubectl: not found" is an
// answer, and losing it to a non-zero exit status would replace the finding
// with nothing.
func TestCollectClusterStateKeepsPartialOutput(t *testing.T) {
	session := newFakeSession()
	session.fail = func(string) (string, error) {
		return "sh: 1: kubectl: not found", os.ErrPermission
	}

	dest := filepath.Join(t.TempDir(), "state.txt")
	if err := CollectClusterState(context.Background(), session, dest); err != nil {
		t.Fatalf("CollectClusterState: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kubectl: not found", "collection failed"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("want %q in the bundle, got:\n%s", want, got)
		}
	}
}

// The bundle is uploaded as a CI artifact, so it leaves the scope the
// credentials were issued for. kubectl output carries Secrets base64-encoded,
// so both forms have to go.
func TestCollectClusterStateRedacts(t *testing.T) {
	// Not written out as a literal: a base64 blob beside the word "key" is
	// what a credential scanner is built to catch, and this fixture exists
	// precisely to look like one. Encoding it here keeps the test honest and
	// keeps the scanner off a value that was never a credential.
	const secret = "example-token-not-a-credential"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	session := newFakeSession()
	session.fail = func(string) (string, error) {
		return "token: " + secret + "\ndata:\n  key: " + encoded, nil
	}

	dest := filepath.Join(t.TempDir(), "state.txt")
	if err := CollectClusterState(context.Background(), session, dest, secret); err != nil {
		t.Fatalf("CollectClusterState: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) {
		t.Error("the secret survived in the bundle")
	}
	if strings.Contains(string(got), encoded) {
		t.Error("the base64 form of the secret survived in the bundle")
	}
}
