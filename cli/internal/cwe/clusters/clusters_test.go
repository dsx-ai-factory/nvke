// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// for get, create, and delete.
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

func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}
