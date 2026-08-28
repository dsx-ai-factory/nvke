// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util_test

import (
	"strings"
	"testing"

	"github.com/NVIDIA/nke/cli/internal/util"
	api "github.com/NVIDIA/nke/controller/api"
)

func TestYAMLToRequest_ValidRoundTrip(t *testing.T) {
	data := []byte(`
name: test-cluster
orgID: my-org
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
`)
	req, err := util.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "test-cluster" {
		t.Errorf("Name = %q, want %q", req.Name, "test-cluster")
	}
	if req.OrgID != "my-org" {
		t.Errorf("OrgID = %q, want %q", req.OrgID, "my-org")
	}
	if req.ControlPlaneKubernetesVersion != "1.30.0" {
		t.Errorf("ControlPlaneKubernetesVersion = %q, want %q", req.ControlPlaneKubernetesVersion, "1.30.0")
	}
}

func TestYAMLToRequest_InvalidYAML(t *testing.T) {
	_, err := util.YAMLToRequest[api.CreateClusterJSONRequestBody]([]byte(":\t:"))
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestYAMLToRequest_UnknownFieldRejected(t *testing.T) {
	data := []byte(`
name: test-cluster
unknownField: should-fail
`)
	_, err := util.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
	if err == nil {
		t.Error("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal request") {
		t.Errorf("error should mention unmarshal: %v", err)
	}
}

func TestYAMLToRequest_EmptyDocument(t *testing.T) {
	req, err := util.YAMLToRequest[api.CreateClusterJSONRequestBody]([]byte(""))
	if err != nil {
		t.Fatalf("empty YAML should succeed with zero value, got: %v", err)
	}
	if req.Name != "" {
		t.Errorf("empty YAML should produce zero-value Name, got %q", req.Name)
	}
}
