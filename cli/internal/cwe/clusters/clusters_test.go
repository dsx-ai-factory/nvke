// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/NVIDIA/nke/cli/internal/clients"
	api "github.com/NVIDIA/nke/controller/api"
)

// clientFnErr is a clientFn that always returns an error. Commands that pass
// arg/flag validation will reach the API call and return this error — used to
// confirm a command is wired correctly without a real CWE deployment.
func clientFnErr() (*clients.CWEClient, error) {
	return nil, &notConfiguredError{}
}

type notConfiguredError struct{}

func (e *notConfiguredError) Error() string { return "no site configured" }

func isNotConfigured(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no site configured")
}

// TestGetCmdClusterFilterReachesClient verifies that --cluster routes through
// the filtering path to the client call.
func TestGetCmdClusterFilterReachesClient(t *testing.T) {
	cmd := NewGetCmd(clientFnErr)
	cmd.SetArgs([]string{"--cluster", "foo"})
	if err := cmd.Execute(); !isNotConfigured(err) {
		t.Errorf("expected no-site error, got: %v", err)
	}
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

// TestUpdateCmdRequiresSetOrFile verifies that omitting both --set and -f
// fails before the API is called.
func TestUpdateCmdRequiresSetOrFile(t *testing.T) {
	cmd := NewUpdateCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when neither --set nor -f is given, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("missing --set/-f should fail before reaching the API, got: %v", err)
	}
}

// TestUpdateCmdBadSetEntryRejected verifies that an unknown --set key fails
// before the API is called.
func TestUpdateCmdBadSetEntryRejected(t *testing.T) {
	cmd := NewUpdateCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster", "--set", "notAField=x"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for an unknown --set key, got nil")
	}
	if isNotConfigured(err) {
		t.Errorf("unknown --set key should fail before reaching the API, got: %v", err)
	}
}

// TestDeleteCmdRequiresName verifies that delete cluster requires exactly one arg.
func TestDeleteCmdRequiresName(t *testing.T) {
	cmd := NewDeleteCmd(clientFnErr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when cluster name is omitted")
	}
}

// TestDeleteCmdReachesClient verifies that a name arg routes to the client call.
func TestDeleteCmdReachesClient(t *testing.T) {
	cmd := NewDeleteCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster"})
	if err := cmd.Execute(); !isNotConfigured(err) {
		t.Errorf("expected no-site error, got: %v", err)
	}
}

// withParent registers cmd under a throwaway parent so cobra resolves names and
// aliases exactly as it does at runtime, then calls parent with args.
func withParent(cmd *cobra.Command, args []string) error {
	parent := &cobra.Command{Use: "nkectl"}
	parent.AddCommand(cmd)
	parent.SetArgs(args)
	return parent.Execute()
}

// TestCmdAliases verifies that "cluster" and "clusters" are interchangeable
// for get, create, update, and delete.
func TestCmdAliases(t *testing.T) {
	validSpec := writeSpec(t, `
name: my-cluster
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
`)

	tests := []struct {
		label  string
		newCmd func() *cobra.Command
		args   []string
	}{
		{"get clusters", func() *cobra.Command { return NewGetCmd(clientFnErr) }, []string{"clusters"}},
		{"get cluster", func() *cobra.Command { return NewGetCmd(clientFnErr) }, []string{"cluster"}},
		{"create clusters", func() *cobra.Command { return NewCreateCmd(clientFnErr) }, []string{"clusters", "-f", validSpec}},
		{"create cluster", func() *cobra.Command { return NewCreateCmd(clientFnErr) }, []string{"cluster", "-f", validSpec}},
		{"update clusters", func() *cobra.Command { return NewUpdateCmd(clientFnErr) }, []string{"clusters", "my-cluster", "--set", "teleportEnabled=true"}},
		{"update cluster", func() *cobra.Command { return NewUpdateCmd(clientFnErr) }, []string{"cluster", "my-cluster", "--set", "teleportEnabled=true"}},
		{"delete clusters", func() *cobra.Command { return NewDeleteCmd(clientFnErr) }, []string{"clusters", "my-cluster"}},
		{"delete cluster", func() *cobra.Command { return NewDeleteCmd(clientFnErr) }, []string{"cluster", "my-cluster"}},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			err := withParent(tt.newCmd(), tt.args)
			if !isNotConfigured(err) {
				t.Errorf("expected no-site error, got: %v", err)
			}
		})
	}
}

func trueCondition() api.Condition {
	return api.Condition{Status: api.ConditionStatusTrue}
}

func trueConditionPtr() *api.Condition {
	c := trueCondition()
	return &c
}

// TestClusterStatus verifies the priority ordering of clusterStatus.
func TestClusterStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions api.ClusterConditions
		want       string
	}{
		{
			name:       "no conditions",
			conditions: api.ClusterConditions{},
			want:       "Unknown",
		},
		{
			name:       "Provisioning",
			conditions: api.ClusterConditions{Provisioning: trueCondition()},
			want:       "Provisioning",
		},
		{
			name:       "Provisioned",
			conditions: api.ClusterConditions{Provisioned: trueCondition()},
			want:       "Provisioned",
		},
		{
			name:       "Deleting",
			conditions: api.ClusterConditions{Deleting: trueCondition()},
			want:       "Deleting",
		},
		{
			name:       "UpdateFailed",
			conditions: api.ClusterConditions{UpdateFailed: trueConditionPtr()},
			want:       "UpdateFailed",
		},
		{
			name: "Updating",
			conditions: api.ClusterConditions{
				UpdateInProgress: &api.UpdateInProgress{Type: api.UpdateInProgressTypeK8SUpgrade},
			},
			want: "Updating",
		},
		{
			name:       "Upgrading",
			conditions: api.ClusterConditions{ControlPlaneUpgrading: trueConditionPtr()},
			want:       "Upgrading",
		},
		{
			name:       "Recycling",
			conditions: api.ClusterConditions{CpNodesRecycling: trueConditionPtr()},
			want:       "Recycling",
		},
		// Priority checks
		{
			name: "Deleting wins over Provisioned",
			conditions: api.ClusterConditions{
				Deleting:    trueCondition(),
				Provisioned: trueCondition(),
			},
			want: "Deleting",
		},
		{
			name: "Deleting wins over UpdateFailed",
			conditions: api.ClusterConditions{
				Deleting:     trueCondition(),
				UpdateFailed: trueConditionPtr(),
			},
			want: "Deleting",
		},
		{
			name: "UpdateFailed wins over Provisioning",
			conditions: api.ClusterConditions{
				UpdateFailed: trueConditionPtr(),
				Provisioning: trueCondition(),
			},
			want: "UpdateFailed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusterStatus(&api.Cluster{Conditions: tt.conditions})
			if got != tt.want {
				t.Errorf("clusterStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The generic value-parsing and merge-decode behavior of applySets is
// covered by util.ApplySetFlags's own tests. These four instead prove that
// arrayKeys — the one piece of schema-specific knowledge this package
// supplies (see arrayKeys's doc comment) — is correct against the real
// UpdateClusterRequest's nested nico/infra provider structs.

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

func TestApplySetRejectsInvalidUUID(t *testing.T) {
	var req api.UpdateClusterRequest
	err := applySets(&req, []string{"controlPlaneConfig.nico.subnetIDs=not-a-uuid"})
	if err == nil {
		t.Fatal("applySets accepted a non-UUID subnet id")
	}
}

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}
