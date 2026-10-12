// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"testing"

	"github.com/spf13/cobra"
)

// withParent registers cmd under a throwaway parent so cobra resolves names and
// aliases exactly as it does at runtime, then calls parent with args.
func withParent(cmd *cobra.Command, args []string) error {
	parent := &cobra.Command{Use: "nvkectl"}
	parent.AddCommand(cmd)
	parent.SetArgs(args)
	return parent.Execute()
}

// TestCmdAliases verifies that "cluster" and "clusters" are interchangeable
// for get, create, patch, and delete.
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
		{"patch clusters", func() *cobra.Command { return NewPatchCmd(clientFnErr) }, []string{"clusters", "my-cluster", "--patch", "teleportEnabled: true"}},
		{"patch cluster", func() *cobra.Command { return NewPatchCmd(clientFnErr) }, []string{"cluster", "my-cluster", "--patch", "teleportEnabled: true"}},
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
