// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"strings"
	"testing"
)

// TestPatchCmdRequiresPatchOrFile verifies that omitting both --patch and
// --patch-file fails before the API is called.
func TestPatchCmdRequiresPatchOrFile(t *testing.T) {
	cmd := NewPatchCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when neither --patch nor --patch-file is given, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("missing --patch/--patch-file should fail before reaching the API, got: %v", err)
	}
}

// TestPatchCmdRejectsBothPatchAndFile verifies that passing both --patch and
// --patch-file is rejected before the API is called.
func TestPatchCmdRejectsBothPatchAndFile(t *testing.T) {
	path := writeSpec(t, "teleportEnabled: true")
	cmd := NewPatchCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster", "--patch", "teleportEnabled: true", "--patch-file", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when both --patch and --patch-file are given, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("--patch and --patch-file together should fail before reaching the API, got: %v", err)
	}
}

// TestPatchCmdUnknownFieldRejected verifies that a patch with an unknown
// field fails before the API is called.
func TestPatchCmdUnknownFieldRejected(t *testing.T) {
	cmd := NewPatchCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster", "--patch", "notAField: x"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for an unknown field, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("unknown field should fail before reaching the API, got: %v", err)
	}
}

// TestPatchCmdMalformedYAMLRejected verifies that invalid YAML in
// --patch-file fails before the API is called.
func TestPatchCmdMalformedYAMLRejected(t *testing.T) {
	path := writeSpec(t, ":\t:")
	cmd := NewPatchCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster", "--patch-file", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("malformed YAML should fail before reaching the API, got: %v", err)
	}
}

// TestPatchCmdEmptyPatchRejected verifies that an empty --patch document
// fails before the API is called.
func TestPatchCmdEmptyPatchRejected(t *testing.T) {
	cmd := NewPatchCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster", "--patch", "{}"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for an empty patch document, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("an empty patch document should fail before reaching the API, got: %v", err)
	}
}

func TestPatchClusterSendsThePatchAndReportsSuccess(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	out, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch", "controlPlaneKubernetesVersion: 1.31.0")
	if err != nil {
		t.Fatalf("patch cluster: %v", err)
	}

	want := []string{
		"GET /api/clusters?name=my-cluster",
		"PATCH /api/clusters/" + seed.ID.String(),
	}
	seen := fake.seen()
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
	if !strings.Contains(out, `"my-cluster" patched`) {
		t.Errorf("output does not confirm the patch:\n%s", out)
	}
}

// TestPatchClusterMultipleFieldsInOnePatch verifies that a single --patch
// document combining a top-level scalar, a top-level bool, and nested
// object fields sends all of them together, and that the ones the fake
// tracks are actually applied to the stored cluster.
func TestPatchClusterMultipleFieldsInOnePatch(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	patch := `
controlPlaneKubernetesVersion: "1.31.0"
teleportEnabled: true
controlPlaneConfig:
  desiredCount: 3
  maxSurge: 1
`
	_, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch", patch)
	if err != nil {
		t.Fatalf("patch cluster: %v", err)
	}

	body := fake.lastRequestBody()
	for _, want := range []string{
		`"controlPlaneKubernetesVersion":"1.31.0"`,
		`"teleportEnabled":true`,
		`"desiredCount":3`,
		`"maxSurge":1`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("PATCH body = %s, want it to contain %s", body, want)
		}
	}

	stored := fake.stored()
	if len(stored) != 1 {
		t.Fatalf("fake holds %d clusters, want 1", len(stored))
	}
	if stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("stored controlPlaneKubernetesVersion = %q, want 1.31.0", stored[0].ControlPlaneKubernetesVersion)
	}
	if stored[0].TeleportEnabled == nil || !*stored[0].TeleportEnabled {
		t.Errorf("stored teleportEnabled = %v, want true", stored[0].TeleportEnabled)
	}
}

// TestPatchClusterFileLoadsThePatch verifies --patch-file reads a real file
// path (not just stdin) and its fields reach the wire.
func TestPatchClusterFileLoadsThePatch(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	path := writeSpec(t, `controlPlaneKubernetesVersion: "1.31.0"`)
	_, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch-file", path)
	if err != nil {
		t.Fatalf("patch cluster --patch-file %s: %v", path, err)
	}

	body := fake.lastRequestBody()
	if !bytes.Contains(body, []byte(`"controlPlaneKubernetesVersion":"1.31.0"`)) {
		t.Errorf("PATCH body = %s, want controlPlaneKubernetesVersion 1.31.0", body)
	}
}

// TestPatchClusterStdin verifies --patch-file - reads the patch from stdin.
func TestPatchClusterStdin(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewPatchCmd(clientFn)
	cmd.SetIn(strings.NewReader("teleportEnabled: true"))
	_, err := run(t, cmd, "my-cluster", "--patch-file", "-")
	if err != nil {
		t.Fatalf("patch cluster --patch-file -: %v", err)
	}

	body := fake.lastRequestBody()
	if !bytes.Contains(body, []byte(`"teleportEnabled":true`)) {
		t.Errorf("PATCH body = %s, want teleportEnabled true", body)
	}
}

// TestPatchClusterEmptyValueReachesTheWireAsALiteralEmptyArray verifies that
// an empty array literal in the patch document sends a literal
// "preKubeadmCommands":[] in the PATCH body, not silently dropped by
// omitempty during a re-marshal.
func TestPatchClusterEmptyValueReachesTheWireAsALiteralEmptyArray(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	_, err := run(t, NewPatchCmd(clientFn), "my-cluster",
		"--patch", "controlPlaneConfig: {infra: {preKubeadmCommands: []}}",
	)
	if err != nil {
		t.Fatalf("patch cluster: %v", err)
	}

	body := fake.lastRequestBody()
	if !bytes.Contains(body, []byte(`"preKubeadmCommands":[]`)) {
		t.Errorf("PATCH body = %s, want a literal \"preKubeadmCommands\":[]", body)
	}
}

// TestPatchClusterSurfacesServerRejection verifies that a PATCH the server
// rejects surfaces the server's error, after the name resolves successfully
// — GET/list aren't blocked by an in-progress mutation on CWE, only a new
// PATCH racing it is.
func TestPatchClusterSurfacesServerRejection(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)
	fake.failPatch(409, "another operation is already in progress")

	_, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch", "teleportEnabled: true")
	if err == nil {
		t.Fatal("expected the server rejection to surface, got nil")
	}
	if !strings.Contains(err.Error(), "another operation is already in progress") {
		t.Errorf("error = %v, want it to carry the server's message", err)
	}
}

// TestPatchUnknownClusterReportsNotFound verifies that patching a cluster
// name that does not exist fails with a not-found error.
func TestPatchUnknownClusterReportsNotFound(t *testing.T) {
	_, clientFn := newFakeCWE(t)

	_, err := run(t, NewPatchCmd(clientFn), "nope", "--patch", "teleportEnabled: true")
	if err == nil {
		t.Fatal("expected a not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to mention not found", err)
	}
}
