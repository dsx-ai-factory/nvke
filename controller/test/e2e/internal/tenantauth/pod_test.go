// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"strings"
	"testing"
)

func TestBuildPodWaitsForExpectedUsername(t *testing.T) {
	const expectedUsername = "spiffe://test.nke.local/nke-service/nke-controller"
	pod := buildPod(
		Identity{Namespace: "nvke-controller", ServiceAccount: "nvke-controller"},
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

func TestBuildPodMountsOptionalExchangeCA(t *testing.T) {
	pod := buildPod(
		Identity{
			Namespace:         "nvke-controller",
			ServiceAccount:    "nvke-controller",
			CABundleConfigMap: "dsx-identity-exchange-ca",
		},
		"tenant-cluster",
		"spiffe://nke.capd.nvke-controller.nvid.id/bm/cwe",
		nil,
	)

	verify := pod.Spec.Containers[0]
	if got := verify.Env[len(verify.Env)-1]; got.Name != "SSL_CERT_FILE" || got.Value != exchangeCAPath {
		t.Fatalf("exchange CA env = %s=%q, want SSL_CERT_FILE=%q", got.Name, got.Value, exchangeCAPath)
	}
	if got := verify.VolumeMounts[len(verify.VolumeMounts)-1]; got.MountPath != exchangeCAPath || got.SubPath != "ca.crt" {
		t.Fatalf("exchange CA mount = %#v", got)
	}
	volume := pod.Spec.Volumes[len(pod.Spec.Volumes)-1]
	if volume.ConfigMap == nil || volume.ConfigMap.Name != "dsx-identity-exchange-ca" {
		t.Fatalf("exchange CA volume = %#v", volume)
	}
}
