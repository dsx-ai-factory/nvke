// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

// TestCreateCmdRequiresFilenameFlag verifies that omitting -f returns an error.
func TestCreateCmdRequiresFilenameFlag(t *testing.T) {
	cmd := NewCreateCmd(clientFnErr)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when -f is omitted, got nil")
	}
}

// TestCreateCmdStdin verifies that -f - reads the spec from stdin.
func TestCreateCmdStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(`
name: my-cluster
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
`)
	w.Close()

	cmd := NewCreateCmd(clientFnErr)
	cmd.SetIn(r)
	cmd.SetArgs([]string{"-f", "-"})
	if err := cmd.Execute(); !isNotConfigured(err) {
		t.Errorf("stdin: expected no-site error, got: %v", err)
	}
}

// TestCreateCmdUnknownFieldRejected verifies that a spec with an unknown field
// fails before the API is called.
func TestCreateCmdUnknownFieldRejected(t *testing.T) {
	spec := writeSpec(t, `
name: my-cluster
unknownField: should-fail
`)
	cmd := NewCreateCmd(clientFnErr)
	cmd.SetArgs([]string{"-f", spec})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("unknown field should fail before reaching the API, got: %v", err)
	}
}

// TestCreateCmdMalformedYAMLRejected verifies that invalid YAML fails before
// the API is called.
func TestCreateCmdMalformedYAMLRejected(t *testing.T) {
	spec := writeSpec(t, ":\t:")
	cmd := NewCreateCmd(clientFnErr)
	cmd.SetArgs([]string{"-f", spec})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("malformed YAML should fail before reaching the API, got: %v", err)
	}
}

// TestCreateCmdEmptySpecRejected verifies that an empty spec file fails before
// the API is called.
func TestCreateCmdEmptySpecRejected(t *testing.T) {
	spec := writeSpec(t, "")
	cmd := NewCreateCmd(clientFnErr)
	cmd.SetArgs([]string{"-f", spec})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty spec, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("empty spec should fail before reaching the API, got: %v", err)
	}
}

func TestCreateClusterPostsTheSpecAndReportsTheID(t *testing.T) {
	fake, clientFn := newFakeCWE(t)

	cmd := NewCreateCmd(clientFn)
	cmd.SetIn(strings.NewReader(`
name: my-cluster
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
controlPlaneConfig:
  desiredCount: 3
  nodeLabels:
    tier: control
nodePools:
  - name: gpu
    nodeNamePrefix: g
    desiredNodeCount: 1
    nodeLabels:
      nvidia.com/gpu.present: "true"
    nodeTaints:
      - key: nvidia.com/gpu
        value: "true"
        effect: NoSchedule
`))
	out, err := run(t, cmd, "-f", "-")
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	if seen := fake.seen(); len(seen) != 1 || seen[0] != "POST /api/clusters" {
		t.Fatalf("requests = %v, want one POST /api/clusters", seen)
	}

	// The fake builds its record from the body it was sent, so this asserts
	// the CLI serialised the spec rather than dropping fields.
	stored := fake.stored()
	if len(stored) != 1 {
		t.Fatalf("fake holds %d clusters, want 1", len(stored))
	}
	if stored[0].Name != "my-cluster" {
		t.Errorf("posted name = %q, want my-cluster", stored[0].Name)
	}
	if stored[0].ControlPlaneKubernetesVersion != "1.30.0" {
		t.Errorf("posted kubernetes version = %q, want 1.30.0", stored[0].ControlPlaneKubernetesVersion)
	}
	if stored[0].CniPlugin != "calico" {
		t.Errorf("posted cni plugin = %q, want calico", stored[0].CniPlugin)
	}

	// Labels and taints are not part of the stored cluster, so assert the wire body.
	var body struct {
		ControlPlaneConfig struct {
			NodeLabels map[string]any `json:"nodeLabels"`
		} `json:"controlPlaneConfig"`
		NodePools []struct {
			NodeLabels map[string]any `json:"nodeLabels"`
			NodeTaints []any          `json:"nodeTaints"`
		} `json:"nodePools"`
	}
	if err := json.Unmarshal(fake.lastRequestBody(), &body); err != nil {
		t.Fatalf("decoding the create body: %v", err)
	}
	if want := map[string]any{"tier": "control"}; !reflect.DeepEqual(body.ControlPlaneConfig.NodeLabels, want) {
		t.Errorf("controlPlaneConfig.nodeLabels = %v, want %v", body.ControlPlaneConfig.NodeLabels, want)
	}
	if len(body.NodePools) != 1 {
		t.Fatalf("nodePools = %d, want 1", len(body.NodePools))
	}
	if want := map[string]any{"nvidia.com/gpu.present": "true"}; !reflect.DeepEqual(body.NodePools[0].NodeLabels, want) {
		t.Errorf("nodePools[0].nodeLabels = %v, want %v", body.NodePools[0].NodeLabels, want)
	}
	wantTaints := []any{map[string]any{"key": "nvidia.com/gpu", "value": "true", "effect": "NoSchedule"}}
	if !reflect.DeepEqual(body.NodePools[0].NodeTaints, wantTaints) {
		t.Errorf("nodePools[0].nodeTaints = %v, want %v", body.NodePools[0].NodeTaints, wantTaints)
	}

	if !strings.Contains(out, `"my-cluster" created`) {
		t.Errorf("output does not confirm creation:\n%s", out)
	}
	if !strings.Contains(out, stored[0].ID.String()) {
		t.Errorf("output omits the new cluster id:\n%s", out)
	}
}
