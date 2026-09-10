// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"testing"

	api "github.com/NVIDIA/nke/controller/api"
)

// arrayKeys mirrors the subset of clusters.arrayKeys this file's tests
// exercise — ApplySetFlags is generic, but a caller must always supply
// which dot-paths are list-valued (see ApplySetFlags's doc comment for why).
var testArrayKeys = map[string]bool{
	"controlPlaneConfig.nico.subnetIDs": true,
}

func TestApplySetFlagsScalarField(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneKubernetesVersion=1.31.0"}, nil); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneKubernetesVersion == nil || *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want 1.31.0", req.ControlPlaneKubernetesVersion)
	}
}

func TestApplySetFlagsBuildsNestedPath(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.desiredCount=3"}, nil); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.DesiredCount == nil || *req.ControlPlaneConfig.DesiredCount != 3 {
		t.Errorf("controlPlaneConfig.desiredCount = %v, want 3", req.ControlPlaneConfig)
	}
}

func TestApplySetFlagsArrayKeyWrapsSingleValue(t *testing.T) {
	id := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.nico.subnetIDs=" + id}, testArrayKeys); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.Nico == nil || len(req.ControlPlaneConfig.Nico.SubnetIDs) != 1 {
		t.Fatalf("subnetIDs = %v, want a one-element list", req.ControlPlaneConfig)
	}
	if req.ControlPlaneConfig.Nico.SubnetIDs[0].String() != id {
		t.Errorf("subnetIDs[0] = %v, want %s", req.ControlPlaneConfig.Nico.SubnetIDs[0], id)
	}
}

func TestApplySetFlagsRejectsUnknownKey(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"bogusField=x"}, nil); err == nil {
		t.Fatal("ApplySetFlags accepted an unknown key")
	}
}

func TestApplySetFlagsRejectsMissingEquals(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneKubernetesVersion"}, nil); err == nil {
		t.Fatal("ApplySetFlags accepted an entry without '='")
	}
}

func TestApplySetFlagsRejectsTypeMismatch(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := ApplySetFlags(&req, []string{"controlPlaneConfig.desiredCount=not-a-number"}, nil); err == nil {
		t.Fatal("ApplySetFlags accepted a non-integer desiredCount")
	}
}

func TestApplySetFlagsLaterEntryOverridesEarlier(t *testing.T) {
	var req api.UpdateClusterRequest
	err := ApplySetFlags(&req, []string{
		"controlPlaneKubernetesVersion=1.30.0",
		"controlPlaneKubernetesVersion=1.31.0",
	}, nil)
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
	if err := ApplySetFlags(&req, []string{"teleportEnabled=true"}, nil); err != nil {
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
	if err := ApplySetFlags(&req, nil, nil); err != nil {
		t.Fatalf("ApplySetFlags: %v", err)
	}
	if req.ControlPlaneKubernetesVersion != nil {
		t.Errorf("controlPlaneKubernetesVersion = %v, want nil", req.ControlPlaneKubernetesVersion)
	}
}
