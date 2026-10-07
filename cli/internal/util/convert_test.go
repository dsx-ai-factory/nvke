// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"reflect"
	"strings"
	"testing"

	api "github.com/dsx-ai-factory/nke/controller/api"
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
// non-string key is rejected with a clear error at the top level or nested in
// another mapping or sequence, instead of "yaml document is not a mapping"
// (top level) or a raw json.Marshal "unsupported type" error (nested).
func TestYAMLToGenericMap_NonStringKeyRejected(t *testing.T) {
	if _, err := YAMLToGenericMap([]byte("1: x\n")); err == nil {
		t.Error("expected an error for a non-string top-level key, got nil")
	}
	if _, err := YAMLToGenericMap([]byte("controlPlaneConfig:\n  1: x\n")); err == nil {
		t.Error("expected an error for a non-string nested key, got nil")
	}
	if _, err := YAMLToGenericMap([]byte("items:\n  - 1: x\n")); err == nil {
		t.Error("expected an error for a non-string key in a sequence, got nil")
	}
}

// TestYAMLToGenericMap_MergeKeyAccepted verifies that a YAML merge key
// (<<: *anchor) is accepted rather than rejected as a non-string key, and
// that the merged fields land in the result alongside the mapping's own.
func TestYAMLToGenericMap_MergeKeyAccepted(t *testing.T) {
	data := []byte("defaults: &defaults\n  teleportEnabled: true\ncontrolPlaneConfig:\n  <<: *defaults\n  desiredCount: 3\n")
	m, err := YAMLToGenericMap(data)
	if err != nil {
		t.Fatalf("unexpected error for a merge key: %v", err)
	}
	cpConfig, ok := m["controlPlaneConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("controlPlaneConfig = %v, want a map", m["controlPlaneConfig"])
	}
	if cpConfig["teleportEnabled"] != true {
		t.Errorf("teleportEnabled = %v, want true (merged from defaults)", cpConfig["teleportEnabled"])
	}
	if cpConfig["desiredCount"] != 3 {
		t.Errorf("desiredCount = %v, want 3", cpConfig["desiredCount"])
	}
}

// TestYAMLToGenericMap_NullDocumentVariants verifies that every YAML
// spelling of "no document" (fully empty input, a bare "---" separator with
// nothing after it, and an explicit null) all report the same "empty
// document" error, not the generic "not a mapping" error a null scalar
// would otherwise fall into.
func TestYAMLToGenericMap_NullDocumentVariants(t *testing.T) {
	for _, data := range []string{"", "---\n", "null\n", "~\n"} {
		_, err := YAMLToGenericMap([]byte(data))
		if err == nil || err.Error() != "empty document" {
			t.Errorf("YAMLToGenericMap(%q) = %v, want \"empty document\"", data, err)
		}
	}
}

// JSON has no timestamps, so a scalar YAML would read as one stays the text it
// was written as, at any depth.
func TestYAMLToGenericMap_TimestampsStayText(t *testing.T) {
	t.Parallel()

	got, err := YAMLToGenericMap([]byte("date: 2026-03-23\nnested:\n  at: 2026-03-23T10:00:00Z\nlist:\n  - 2026-03-23\nquoted: \"2026-03-23\"\n"))
	if err != nil {
		t.Fatalf("YAMLToGenericMap: %v", err)
	}
	want := map[string]interface{}{
		"date":   "2026-03-23",
		"nested": map[string]interface{}{"at": "2026-03-23T10:00:00Z"},
		"list":   []interface{}{"2026-03-23"},
		"quoted": "2026-03-23",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}
