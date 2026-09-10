// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	api "github.com/NVIDIA/nke/controller/api"
)

// applyPatch builds a patch from entries via BuildPatch and decodes it onto
// dst, merging onto whatever dst already holds.
func applyPatch[T any](dst *T, entries []string) error {
	patch, err := BuildPatch[T](entries)
	if err != nil {
		return err
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// applyPatchFresh is applyPatch against a zero-value T.
func applyPatchFresh[T any](entries []string) (T, error) {
	var dst T
	err := applyPatch(&dst, entries)
	return dst, err
}

func TestApplySetFlagsScalarField(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneKubernetesVersion=1.31.0"}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if req.ControlPlaneKubernetesVersion == nil || *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want 1.31.0", req.ControlPlaneKubernetesVersion)
	}
}

func TestApplySetFlagsBuildsNestedPath(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneConfig.desiredCount=3"}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.DesiredCount == nil || *req.ControlPlaneConfig.DesiredCount != 3 {
		t.Errorf("controlPlaneConfig.desiredCount = %v, want 3", req.ControlPlaneConfig)
	}
}

// TestApplySetFlagsArrayKeyWrapsSingleValue and the three below it check
// BuildPatch's array detection against the real UpdateClusterRequest: a
// *[]string field (nodeLabels), a plain []uuid.UUID field, and a []string
// field nested under a different provider struct (infra vs nico).

func TestApplySetFlagsArrayKeyWrapsSingleValue(t *testing.T) {
	id := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneConfig.nico.subnetIDs=" + id}); err != nil {
		t.Fatalf("applyPatch: %v", err)
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
	if err := applyPatch(&req, []string{
		"controlPlaneConfig.nico.osProfile=gpu-profile",
		"controlPlaneConfig.nico.subnetIDs=" + id1.String() + "," + id2.String(),
	}); err != nil {
		t.Fatalf("applyPatch: %v", err)
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
	if err := applyPatch(&req, []string{
		"controlPlaneConfig.infra.preKubeadmCommands=echo one,echo two",
	}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	infra := req.ControlPlaneConfig.Infra
	if infra == nil || len(infra.PreKubeadmCommands) != 2 || infra.PreKubeadmCommands[1] != "echo two" {
		t.Errorf("preKubeadmCommands = %v, want [echo one, echo two]", infra)
	}
}

func TestApplySetFlagsInfraBootstrapFilesYAMLSequence(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applyPatch(&req, []string{
		`controlPlaneConfig.infra.bootstrapFiles=[{path: /etc/foo, content: bar}, {path: /etc/baz, content: "qux, quux"}]`,
	})
	if err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	files := req.ControlPlaneConfig.Infra.BootstrapFiles
	if len(files) != 2 {
		t.Fatalf("bootstrapFiles = %v, want 2 entries", files)
	}
	if files[0].Path != "/etc/foo" || files[0].Content != "bar" {
		t.Errorf("bootstrapFiles[0] = %+v, want path=/etc/foo content=bar", files[0])
	}
	// The comma inside the quoted content parses correctly only because
	// the whole value is tried as one YAML sequence first.
	if files[1].Path != "/etc/baz" || files[1].Content != "qux, quux" {
		t.Errorf("bootstrapFiles[1] = %+v, want path=/etc/baz content=%q", files[1], "qux, quux")
	}
}

func TestApplySetFlagsPointerToSliceField(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneConfig.nodeLabels=gpu=true"}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if req.ControlPlaneConfig == nil || req.ControlPlaneConfig.NodeLabels == nil || len(*req.ControlPlaneConfig.NodeLabels) != 1 {
		t.Fatalf("nodeLabels = %v, want a one-element list", req.ControlPlaneConfig)
	}
}

// TestApplySetFlagsControlPlaneConfigScalarFields covers the
// controlPlaneConfig.* scalar fields not already exercised by another test
// in this file: the two provider-agnostic numeric/string/bool fields, one
// nico.* string field, and one infra.* enum field.
func TestApplySetFlagsControlPlaneConfigScalarFields(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		check func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig)
	}{
		{
			name:  "maxSurge",
			entry: "controlPlaneConfig.maxSurge=1",
			check: func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig) {
				if cfg.MaxSurge == nil || *cfg.MaxSurge != 1 {
					t.Errorf("maxSurge = %v, want 1", cfg.MaxSurge)
				}
			},
		},
		{
			name:  "systemReservedMemory",
			entry: "controlPlaneConfig.systemReservedMemory=16Gi",
			check: func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig) {
				if cfg.SystemReservedMemory == nil || *cfg.SystemReservedMemory != "16Gi" {
					t.Errorf("systemReservedMemory = %v, want 16Gi", cfg.SystemReservedMemory)
				}
			},
		},
		{
			name:  "provisioningMonitorDisabled",
			entry: "controlPlaneConfig.provisioningMonitorDisabled=true",
			check: func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig) {
				if cfg.ProvisioningMonitorDisabled == nil || !*cfg.ProvisioningMonitorDisabled {
					t.Errorf("provisioningMonitorDisabled = %v, want true", cfg.ProvisioningMonitorDisabled)
				}
			},
		},
		{
			// Distinct from the top-level teleportEnabled field, which
			// TestApplySetFlagsMergesOntoExistingValue already covers.
			name:  "controlPlaneConfig-scoped teleportEnabled",
			entry: "controlPlaneConfig.teleportEnabled=false",
			check: func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig) {
				if cfg.TeleportEnabled == nil || *cfg.TeleportEnabled {
					t.Errorf("controlPlaneConfig.teleportEnabled = %v, want false", cfg.TeleportEnabled)
				}
			},
		},
		{
			name:  "nico.configProfile",
			entry: "controlPlaneConfig.nico.configProfile=default",
			check: func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig) {
				if cfg.Nico == nil || cfg.Nico.ConfigProfile == nil || *cfg.Nico.ConfigProfile != "default" {
					t.Errorf("nico.configProfile = %v, want default", cfg.Nico)
				}
			},
		},
		{
			name:  "infra.spireAttestationMethod",
			entry: "controlPlaneConfig.infra.spireAttestationMethod=tpm",
			check: func(t *testing.T, cfg *api.UpdateClusterControlPlaneConfig) {
				if cfg.Infra == nil || cfg.Infra.SpireAttestationMethod == nil ||
					*cfg.Infra.SpireAttestationMethod != api.SpireAttestationMethodTpm {
					t.Errorf("infra.spireAttestationMethod = %v, want tpm", cfg.Infra)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req api.UpdateClusterRequest
			if err := applyPatch(&req, []string{tt.entry}); err != nil {
				t.Fatalf("applyPatch(%q): %v", tt.entry, err)
			}
			if req.ControlPlaneConfig == nil {
				t.Fatal("controlPlaneConfig is nil")
			}
			tt.check(t, req.ControlPlaneConfig)
		})
	}
}

func TestApplySetFlagsNicoRemainingUUIDListFields(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	var req api.UpdateClusterRequest
	err := applyPatch(&req, []string{
		"controlPlaneConfig.nico.instanceTypePreference=" + id1.String(),
		"controlPlaneConfig.nico.sshKeyGroupIDs=" + id1.String() + "," + id2.String(),
		"controlPlaneConfig.nico.vpcPrefixIDs=" + id2.String(),
	})
	if err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	nico := req.ControlPlaneConfig.Nico
	if len(nico.InstanceTypePreference) != 1 || nico.InstanceTypePreference[0] != id1 {
		t.Errorf("instanceTypePreference = %v, want [%s]", nico.InstanceTypePreference, id1)
	}
	if len(nico.SSHKeyGroupIDs) != 2 || nico.SSHKeyGroupIDs[0] != id1 || nico.SSHKeyGroupIDs[1] != id2 {
		t.Errorf("sshKeyGroupIDs = %v, want [%s %s]", nico.SSHKeyGroupIDs, id1, id2)
	}
	if len(nico.VpcPrefixIDs) != 1 || nico.VpcPrefixIDs[0] != id2 {
		t.Errorf("vpcPrefixIDs = %v, want [%s]", nico.VpcPrefixIDs, id2)
	}
}

func TestApplySetFlagsInfraMachineTemplateYAMLMapping(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applyPatch(&req, []string{
		`controlPlaneConfig.infra.machineTemplate={kind: NicoMachineTemplate, apiVersion: "infrastructure.cluster.x-k8s.io/v1alpha1"}`,
	})
	if err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	mt := req.ControlPlaneConfig.Infra.MachineTemplate
	if mt["kind"] != "NicoMachineTemplate" {
		t.Errorf("machineTemplate[kind] = %v, want NicoMachineTemplate", mt["kind"])
	}
	if mt["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1alpha1" {
		t.Errorf("machineTemplate[apiVersion] = %v, want infrastructure.cluster.x-k8s.io/v1alpha1", mt["apiVersion"])
	}
}

func TestApplySetFlagsRejectsUnknownKey(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"bogusField=x"}); err == nil {
		t.Fatal("applyPatch accepted an unknown key")
	}
}

func TestApplySetFlagsRejectsMissingEquals(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneKubernetesVersion"}); err == nil {
		t.Fatal("applyPatch accepted an entry without '='")
	}
}

func TestApplySetFlagsRejectsTypeMismatch(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneConfig.desiredCount=not-a-number"}); err == nil {
		t.Fatal("applyPatch accepted a non-integer desiredCount")
	}
}

func TestApplySetFlagsRejectsInvalidUUID(t *testing.T) {
	var req api.UpdateClusterRequest
	if err := applyPatch(&req, []string{"controlPlaneConfig.nico.subnetIDs=not-a-uuid"}); err == nil {
		t.Fatal("applyPatch accepted a non-UUID subnet id")
	}
}

func TestApplySetFlagsLaterEntryOverridesEarlier(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applyPatch(&req, []string{
		"controlPlaneKubernetesVersion=1.30.0",
		"controlPlaneKubernetesVersion=1.31.0",
	})
	if err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if *req.ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("controlPlaneKubernetesVersion = %v, want the later value 1.31.0", *req.ControlPlaneKubernetesVersion)
	}
}

func TestApplySetFlagsMergesOntoExistingValue(t *testing.T) {
	version := "1.30.0"
	req := api.UpdateClusterRequest{ControlPlaneKubernetesVersion: &version}
	if err := applyPatch(&req, []string{"teleportEnabled=true"}); err != nil {
		t.Fatalf("applyPatch: %v", err)
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
	if err := applyPatch(&req, nil); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if req.ControlPlaneKubernetesVersion != nil {
		t.Errorf("controlPlaneKubernetesVersion = %v, want nil", req.ControlPlaneKubernetesVersion)
	}
}

// TestApplySetFlagsEmptyValueClearsScalarField proves an empty --set value
// on a pointer-typed scalar field decodes to a non-nil pointer to "" rather
// than nil (see UpdateClusterControlPlaneConfig and its nested provider
// structs' "omitted/value/empty" three-state doc comments).
func TestApplySetFlagsEmptyValueClearsScalarField(t *testing.T) {
	old := "old-profile"
	req := api.UpdateClusterRequest{
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{OSProfile: &old},
		},
	}
	if err := applyPatch(&req, []string{"controlPlaneConfig.nico.osProfile="}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	profile := req.ControlPlaneConfig.Nico.OSProfile
	if profile == nil || *profile != "" {
		t.Errorf("osProfile = %v, want a non-nil pointer to \"\"", profile)
	}
}

func TestApplySetFlagsEmptyValueClearsPointerToSliceField(t *testing.T) {
	labels := []string{"gpu=true"}
	req := api.UpdateClusterRequest{
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{NodeLabels: &labels},
	}
	if err := applyPatch(&req, []string{"controlPlaneConfig.nodeLabels="}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if req.ControlPlaneConfig.NodeLabels == nil || len(*req.ControlPlaneConfig.NodeLabels) != 0 {
		t.Errorf("nodeLabels = %v, want a non-nil pointer to an empty slice", req.ControlPlaneConfig.NodeLabels)
	}
}

func TestApplySetFlagsEmptyValueClearsPlainSliceField(t *testing.T) {
	req := api.UpdateClusterRequest{
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			Nico: &api.NicoControlPlaneProviderUpdate{SubnetIDs: []uuid.UUID{uuid.New()}},
		},
	}
	if err := applyPatch(&req, []string{"controlPlaneConfig.nico.subnetIDs="}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	// A JSON array decode replaces a slice's contents, unlike a JSON object
	// decoding onto a map (see the map test below). Starting from one
	// element proves the result is genuinely empty, not merely untouched.
	if len(req.ControlPlaneConfig.Nico.SubnetIDs) != 0 {
		t.Errorf("subnetIDs = %v, want empty", req.ControlPlaneConfig.Nico.SubnetIDs)
	}
}

// TestApplySetFlagsEmptyValueOnMapFieldIsANoOpOntoAnExistingMap documents
// that decoding an empty object onto an already-populated map field is a
// no-op: encoding/json merges a JSON object onto an existing map instead of
// replacing it. Compare with
// TestBuildPatchEmptyValueOnMapFieldReachesTheWireAsAGenuineClear.
func TestApplySetFlagsEmptyValueOnMapFieldIsANoOpOntoAnExistingMap(t *testing.T) {
	req := api.UpdateClusterRequest{
		ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{
			Infra: &api.InfraControlPlaneProviderUpdate{MachineTemplate: map[string]interface{}{"kind": "Foo"}},
		},
	}
	if err := applyPatch(&req, []string{"controlPlaneConfig.infra.machineTemplate="}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if req.ControlPlaneConfig.Infra.MachineTemplate["kind"] != "Foo" {
		t.Errorf("machineTemplate = %v, want the pre-existing key left untouched (merge, not replace)",
			req.ControlPlaneConfig.Infra.MachineTemplate)
	}
}

// TestBuildPatchEmptyValueOnMapFieldReachesTheWireAsAGenuineClear verifies
// that BuildPatch's result for an empty machineTemplate value marshals to a
// literal empty object, and decodes cleanly onto a fresh
// UpdateClusterRequest.
func TestBuildPatchEmptyValueOnMapFieldReachesTheWireAsAGenuineClear(t *testing.T) {
	patch, err := BuildPatch[api.UpdateClusterRequest]([]string{"controlPlaneConfig.infra.machineTemplate="})
	if err != nil {
		t.Fatalf("BuildPatch: %v", err)
	}
	data, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"machineTemplate":{}`)) {
		t.Errorf("wire body = %s, want a literal empty object for machineTemplate", data)
	}

	req, err := applyPatchFresh[api.UpdateClusterRequest]([]string{"controlPlaneConfig.infra.machineTemplate="})
	if err != nil {
		t.Fatalf("applyPatchFresh: %v", err)
	}
	if req.ControlPlaneConfig.Infra.MachineTemplate == nil || len(req.ControlPlaneConfig.Infra.MachineTemplate) != 0 {
		t.Errorf("machineTemplate = %v, want a non-nil empty map", req.ControlPlaneConfig.Infra.MachineTemplate)
	}
}

// TestApplySetFlagsRejectsScalarThenNestedConflict verifies that setting a
// path to a scalar, then a nested field under that same path in a later
// entry, errors instead of silently discarding the earlier entry.
func TestApplySetFlagsRejectsScalarThenNestedConflict(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applyPatch(&req, []string{
		"controlPlaneConfig=x",
		"controlPlaneConfig.nico.osProfile=y",
	})
	if err == nil {
		t.Fatal("applyPatch accepted a nested entry conflicting with an earlier scalar entry")
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
