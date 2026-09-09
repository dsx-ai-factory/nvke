// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"testing"

	"github.com/google/uuid"

	api "github.com/NVIDIA/nke/controller/api"
)

func TestApplySetScalarFields(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applySets(&req, []string{
		"controlPlaneKubernetesVersion=1.31.0",
		"enableCPNodeRecycling=true",
		"teleportEnabled=false",
	}); err != nil {
		t.Fatalf("applySets: %v", err)
	}
	if req.ControlPlaneKubernetesVersion == nil || *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want 1.31.0", req.ControlPlaneKubernetesVersion)
	}
	if req.EnableCPNodeRecycling == nil || *req.EnableCPNodeRecycling != true {
		t.Errorf("enableCPNodeRecycling = %v, want true", req.EnableCPNodeRecycling)
	}
	if req.TeleportEnabled == nil || *req.TeleportEnabled != false {
		t.Errorf("teleportEnabled = %v, want false", req.TeleportEnabled)
	}
}

func TestApplySetControlPlaneConfigScalars(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applySets(&req, []string{
		"controlPlaneConfig.desiredCount=3",
		"controlPlaneConfig.maxSurge=1",
		"controlPlaneConfig.systemReservedMemory=16Gi",
		"controlPlaneConfig.nodeLabels=gpu=true,zone=us-east",
	}); err != nil {
		t.Fatalf("applySets: %v", err)
	}
	cfg := req.ControlPlaneConfig
	if cfg == nil {
		t.Fatal("controlPlaneConfig is nil")
	}
	if cfg.DesiredCount == nil || *cfg.DesiredCount != 3 {
		t.Errorf("desiredCount = %v, want 3", cfg.DesiredCount)
	}
	if cfg.MaxSurge == nil || *cfg.MaxSurge != 1 {
		t.Errorf("maxSurge = %v, want 1", cfg.MaxSurge)
	}
	if cfg.SystemReservedMemory == nil || *cfg.SystemReservedMemory != "16Gi" {
		t.Errorf("systemReservedMemory = %v, want 16Gi", cfg.SystemReservedMemory)
	}
	if cfg.NodeLabels == nil || len(*cfg.NodeLabels) != 2 || (*cfg.NodeLabels)[0] != "gpu=true" {
		t.Errorf("nodeLabels = %v, want [gpu=true zone=us-east]", cfg.NodeLabels)
	}
}

func TestApplySetNicoFields(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	var req api.UpdateClusterRequest
	if err := applySets(&req, []string{
		"controlPlaneConfig.nico.osProfile=gpu-profile",
		"controlPlaneConfig.nico.configProfile=default",
		"controlPlaneConfig.nico.subnetIDs=" + id1.String() + "," + id2.String(),
	}); err != nil {
		t.Fatalf("applySets: %v", err)
	}
	nico := req.ControlPlaneConfig.Nico
	if nico == nil {
		t.Fatal("controlPlaneConfig.nico is nil")
	}
	if nico.OSProfile == nil || *nico.OSProfile != "gpu-profile" {
		t.Errorf("osProfile = %v, want gpu-profile", nico.OSProfile)
	}
	if len(nico.SubnetIDs) != 2 || nico.SubnetIDs[0] != id1 || nico.SubnetIDs[1] != id2 {
		t.Errorf("subnetIDs = %v, want [%s %s]", nico.SubnetIDs, id1, id2)
	}
}

func TestApplySetInfraFields(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applySets(&req, []string{
		"controlPlaneConfig.infra.spireAttestationMethod=tpm",
		"controlPlaneConfig.infra.preKubeadmCommands=echo one,echo two",
	}); err != nil {
		t.Fatalf("applySets: %v", err)
	}
	infra := req.ControlPlaneConfig.Infra
	if infra == nil {
		t.Fatal("controlPlaneConfig.infra is nil")
	}
	if infra.SpireAttestationMethod == nil || *infra.SpireAttestationMethod != api.SpireAttestationMethodTpm {
		t.Errorf("spireAttestationMethod = %v, want tpm", infra.SpireAttestationMethod)
	}
	if len(infra.PreKubeadmCommands) != 2 || infra.PreKubeadmCommands[1] != "echo two" {
		t.Errorf("preKubeadmCommands = %v, want [echo one, echo two]", infra.PreKubeadmCommands)
	}
}

func TestApplySetRejectsUnknownKey(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applySets(&req, []string{"controlPlaneConfig.bogusField=x"})
	if err == nil {
		t.Fatal("applySets accepted an unknown key")
	}
}

func TestApplySetRejectsMissingEquals(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applySets(&req, []string{"controlPlaneKubernetesVersion"})
	if err == nil {
		t.Fatal("applySets accepted an entry without '='")
	}
}

func TestApplySetRejectsInvalidBool(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applySets(&req, []string{"teleportEnabled=maybe"})
	if err == nil {
		t.Fatal("applySets accepted an invalid bool")
	}
}

func TestApplySetRejectsInvalidInt(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applySets(&req, []string{"controlPlaneConfig.desiredCount=three"})
	if err == nil {
		t.Fatal("applySets accepted a non-integer desiredCount")
	}
}

func TestApplySetRejectsInvalidUUID(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applySets(&req, []string{"controlPlaneConfig.nico.subnetIDs=not-a-uuid"})
	if err == nil {
		t.Fatal("applySets accepted a non-UUID subnet id")
	}
}

func TestApplySetLaterEntryOverridesEarlier(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applySets(&req, []string{
		"controlPlaneKubernetesVersion=1.30.0",
		"controlPlaneKubernetesVersion=1.31.0",
	}); err != nil {
		t.Fatalf("applySets: %v", err)
	}
	if *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want the later value 1.31.0", *req.ControlPlaneKubernetesVersion)
	}
}
