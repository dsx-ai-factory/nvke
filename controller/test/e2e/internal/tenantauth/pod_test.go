// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"strings"
	"testing"
)

func TestBuildPodWaitsForExpectedUsername(t *testing.T) {
	const expectedUsername = "spiffe://test.nke.local/nke-service/cloud-workflow-engine"
	pod := buildPod(
		Identity{Namespace: "cloud-workflow-engine", ServiceAccount: "cloud-workflow-engine"},
		"tenant-cluster",
		expectedUsername,
		[]Check{{Verb: "list", Resource: "nodes"}},
	)

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	verify := pod.Spec.Containers[0]
	if len(verify.Command) < 4 {
		t.Fatalf("verify command length = %d, want at least 4", len(verify.Command))
	}
	if !strings.Contains(verify.Command[2], `[ "$username" = "$EXPECTED_USERNAME" ]`) {
		t.Fatalf("verify script does not wait for %s", expectedUsernameEnv)
	}
	if !strings.Contains(verify.Command[2], `while [ "$attempt" -lt 12 ]`) {
		t.Fatal("verify script does not retry identity discovery")
	}
	if len(verify.Env) != 2 {
		t.Fatalf("verify env length = %d, want 2", len(verify.Env))
	}
	if got := verify.Env[1]; got.Name != expectedUsernameEnv || got.Value != expectedUsername {
		t.Fatalf("verify expected-username env = %s=%q, want %s=%q", got.Name, got.Value, expectedUsernameEnv, expectedUsername)
	}
}
