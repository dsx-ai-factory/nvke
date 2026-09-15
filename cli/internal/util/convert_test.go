// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"strings"
	"testing"

	api "github.com/NVIDIA/nke/controller/api"
)

func TestYAMLToRequest_ValidRoundTrip(t *testing.T) {
	data := []byte(`
name: test-cluster
nico:
  orgID: my-org
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
`)
	req, err := YAMLToRequest[api.CreateClusterJSONRequestBody](data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "test-cluster" {
		t.Errorf("Name = %q, want %q", req.Name, "test-cluster")
	}
	if req.Nico == nil {
		t.Fatal("Nico = nil, want the nested nico payload")
	}
	if req.Nico.OrgID != "my-org" {
		t.Errorf("Nico.OrgID = %q, want %q", req.Nico.OrgID, "my-org")
	}
	if req.ControlPlaneKubernetesVersion != "1.30.0" {
		t.Errorf("ControlPlaneKubernetesVersion = %q, want %q", req.ControlPlaneKubernetesVersion, "1.30.0")
	}
}

func TestYAMLToRequest_InvalidYAML(t *testing.T) {
	_, err := YAMLToRequest[api.CreateClusterJSONRequestBody]([]byte(":\t:"))
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestYAMLToRequest_UnknownFieldRejected(t *testing.T) {
	data := []byte(`
name: test-cluster
unknownField: should-fail
`)
	_, err := YAMLToRequest[api.CreateClusterJSONRequestBody](data)
	if err == nil {
		t.Error("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal request") {
		t.Errorf("error should mention unmarshal: %v", err)
	}
}

func TestYAMLToRequest_EmptyDocument(t *testing.T) {
	_, err := YAMLToRequest[api.CreateClusterJSONRequestBody]([]byte(""))
	if err == nil {
		t.Error("expected error for empty spec, got nil")
	}
}

// TestYAMLToGenericMap_SecondDocumentRejected verifies that a "---"-separated
// second YAML document is rejected rather than silently discarded, which
// would otherwise let requested fields vanish with no error.
func TestYAMLToGenericMap_SecondDocumentRejected(t *testing.T) {
	data := []byte("teleportEnabled: true\n---\nenableCPNodeRecycling: true\n")
	_, err := YAMLToGenericMap(data)
	if err == nil {
		t.Fatal("expected an error for a second YAML document, got nil")
	}
}

// TestYAMLToGenericMap_NonStringKeyRejected verifies that a mapping with a
// non-string key is rejected with a clear error, both at the top level and
// nested, instead of "yaml document is not a mapping" (top level) or a raw
// json.Marshal "unsupported type" error (nested).
func TestYAMLToGenericMap_NonStringKeyRejected(t *testing.T) {
	if _, err := YAMLToGenericMap([]byte("1: x\n")); err == nil {
		t.Error("expected an error for a non-string top-level key, got nil")
	}
	if _, err := YAMLToGenericMap([]byte("controlPlaneConfig:\n  1: x\n")); err == nil {
		t.Error("expected an error for a non-string nested key, got nil")
	}
}
