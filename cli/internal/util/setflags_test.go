// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"testing"

	"github.com/google/uuid"

	api "github.com/NVIDIA/nke/controller/api"
)

func TestApplySetFlagsScalarField(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneKubernetesVersion=1.31.0"}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneKubernetesVersion == nil || *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want 1.31.0", req.ControlPlaneKubernetesVersion)
	}
}

func TestApplySetFlagsBuildsNestedPath(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.desiredCount=3"}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.DesiredCount == nil || *req.ControlPlaneConfig.DesiredCount != 3 {
		t.Errorf("controlPlaneConfig.desiredCount = %v, want 3", req.ControlPlaneConfig)
	}
}

// TestApplySetFlagsArrayKeyWrapsSingleValue and the three below it prove
// that ApplySetFlags's reflection-based array detection (isArrayField)
// works against the real UpdateClusterRequest, including a *[]string field
// (nodeLabels), a plain []uuid.UUID field, and a []string field nested
// under a different provider struct (infra vs nico).

func TestApplySetFlagsArrayKeyWrapsSingleValue(t *testing.T) {
	id := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.nico.subnetIDs=" + id}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.Nico == nil || len(req.ControlPlaneConfig.Nico.SubnetIDs) != 1 {
		t.Fatalf("subnetIDs = %v, want a one-element list", req.ControlPlaneConfig)
	}
	if req.ControlPlaneConfig.Nico.SubnetIDs[0].String() != id {
		t.Errorf("subnetIDs[0] = %v, want %s", req.ControlPlaneConfig.Nico.SubnetIDs[0], id)
	}
}

func TestApplySetFlagsNicoSubnetIDsMultiValue(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{
		"controlPlaneConfig.nico.osProfile=gpu-profile",
		"controlPlaneConfig.nico.subnetIDs=" + id1.String() + "," + id2.String(),
	}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	nico := req.ControlPlaneConfig.Nico
	if nico.OSProfile == nil || *nico.OSProfile != "gpu-profile" {
		t.Errorf("osProfile = %v, want gpu-profile", nico.OSProfile)
	}
	if len(nico.SubnetIDs) != 2 || nico.SubnetIDs[0] != id1 || nico.SubnetIDs[1] != id2 {
		t.Errorf("subnetIDs = %v, want [%s %s]", nico.SubnetIDs, id1, id2)
	}
}

func TestApplySetFlagsInfraPreKubeadmCommands(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{
		"controlPlaneConfig.infra.preKubeadmCommands=echo one,echo two",
	}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	infra := req.ControlPlaneConfig.Infra
	if infra == nil || len(infra.PreKubeadmCommands) != 2 || infra.PreKubeadmCommands[1] != "echo two" {
		t.Errorf("preKubeadmCommands = %v, want [echo one, echo two]", infra)
	}
}

func TestApplySetFlagsInfraBootstrapFilesYAMLSequence(t *testing.T) {
	var req api.UpdateClusterRequest
	err := ApplySetFlags(&req, []string{
		`controlPlaneConfig.infra.bootstrapFiles=[{path: /etc/foo, content: bar}, {path: /etc/baz, content: "qux, quux"}]`,
	})
	if err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	files := req.ControlPlaneConfig.Infra.BootstrapFiles
	if len(files) != 2 {
		t.Fatalf("bootstrapFiles = %v, want 2 entries", files)
	}
	if files[0].Path != "/etc/foo" || files[0].Content != "bar" {
		t.Errorf("bootstrapFiles[0] = %+v, want path=/etc/foo content=bar", files[0])
	}
	// The comma inside the quoted content only works because the whole
	// value parses as one YAML sequence up front — the naive
	// comma-split fallback would have cut this file's content in half.
	if files[1].Path != "/etc/baz" || files[1].Content != "qux, quux" {
		t.Errorf("bootstrapFiles[1] = %+v, want path=/etc/baz content=%q", files[1], "qux, quux")
	}
}

func TestApplySetFlagsPointerToSliceField(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.nodeLabels=gpu=true"}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.NodeLabels == nil || len(*req.ControlPlaneConfig.NodeLabels) != 1 {
		t.Fatalf("nodeLabels = %v, want a one-element list", req.ControlPlaneConfig)
	}
}

func TestApplySetFlagsRejectsUnknownKey(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"bogusField=x"}); err == nil {
		t.Fatal("ApplySetFlags accepted an unknown key")
	}
}

func TestApplySetFlagsRejectsMissingEquals(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneKubernetesVersion"}); err == nil {
		t.Fatal("ApplySetFlags accepted an entry without '='")
	}
}

func TestApplySetFlagsRejectsTypeMismatch(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.desiredCount=not-a-number"}); err == nil {
		t.Fatal("ApplySetFlags accepted a non-integer desiredCount")
	}
}

func TestApplySetFlagsRejectsInvalidUUID(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.nico.subnetIDs=not-a-uuid"}); err == nil {
		t.Fatal("ApplySetFlags accepted a non-UUID subnet id")
	}
}

func TestApplySetFlagsLaterEntryOverridesEarlier(t *testing.T) {
	var req api.UpdateClusterRequest
	err := ApplySetFlags(&req, []string{
		"controlPlaneKubernetesVersion=1.30.0",
		"controlPlaneKubernetesVersion=1.31.0",
	})
	if err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want the later value 1.31.0", *req.ControlPlaneKubernetesVersion)
	}
}

func TestApplySetFlagsMergesOntoExistingValue(t *testing.T) {
	version := "1.30.0"
	req := api.UpdateClusterRequest{ControlPlaneKubernetesVersion: &version}
	if err := ApplySetFlags(&req, []string{"teleportEnabled=true"}); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneKubernetesVersion == nil || *req.ControlPlaneKubernetesVersion != "1.30.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want the pre-existing value preserved", req.ControlPlaneKubernetesVersion)
	}
	if req.TeleportEnabled == nil || !*req.TeleportEnabled {
		t.Errorf("teleportEnabled = %v, want true", req.TeleportEnabled)
	}
}

func TestApplySetFlagsNoEntriesIsNoop(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, nil); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneKubernetesVersion != nil {
		t.Errorf("controlPlaneKubernetesVersion = %v, want nil", req.ControlPlaneKubernetesVersion)
	}
}

func TestIsArrayField(t *testing.T) {
	tests := []struct {
		path []string
		want bool
	}{
		{[]string{"controlPlaneConfig", "nico", "osProfile"}, false},
		{[]string{"controlPlaneConfig", "nico", "subnetIDs"}, true},
		{[]string{"controlPlaneConfig", "nodeLabels"}, true},
		{[]string{"controlPlaneConfig", "infra", "preKubeadmCommands"}, true},
		{[]string{"controlPlaneConfig", "infra", "machineTemplate"}, false},
		{[]string{"enableCPNodeRecycling"}, false},
		{[]string{"bogusField"}, false},
	}
	for _, tt := range tests {
		if got := isArrayField[api.UpdateClusterRequest](tt.path); got != tt.want {
			t.Errorf("isArrayField(%v) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
