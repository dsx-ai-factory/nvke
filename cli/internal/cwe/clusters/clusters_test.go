// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/nke/cli/internal/clients"
	"github.com/NVIDIA/nke/cli/internal/cwe/clusters"
)

// clientFnErr is a clientFn that always returns an error. Commands that pass
// arg/flag validation will reach the API call and return this error — used to
// confirm a command is wired correctly without a real CWE deployment.
func clientFnErr() (*clients.CWEClient, error) {
	return nil, &notConfiguredError{}
}

type notConfiguredError struct{}

func (e *notConfiguredError) Error() string { return "no site configured" }

// TestGetCmdClusterFilterFlag verifies that --cluster is registered on get clusters.
func TestGetCmdClusterFilterFlag(t *testing.T) {
	cmd := clusters.NewGetCmd(clientFnErr)
	if cmd.Flags().Lookup("cluster") == nil {
		t.Error("--cluster flag missing from get clusters")
	}
}

// TestCreateCmdRequiresFilenameFlag verifies that -f / --filename is required.
func TestCreateCmdRequiresFilenameFlag(t *testing.T) {
	cmd := clusters.NewCreateCmd(clientFnErr)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when -f is omitted, got nil")
	}
}

// TestCreateCmdFilenameShortAndLongForms verifies both -f and --filename are accepted.
func TestCreateCmdFilenameShortAndLongForms(t *testing.T) {
	spec := writeSpec(t, `
name: my-cluster
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
`)
	for _, flag := range []string{"-f", "--filename"} {
		cmd := clusters.NewCreateCmd(clientFnErr)
		cmd.SetArgs([]string{flag, spec})
		err := cmd.Execute()
		// The only expected error is "no site configured" — the command reached
		// the API call, meaning file parsing and flag validation succeeded.
		if err == nil || !strings.Contains(err.Error(), "no site configured") {
			t.Errorf("flag %s: expected no-site error, got: %v", flag, err)
		}
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

	cmd := clusters.NewCreateCmd(clientFnErr)
	cmd.SetIn(r)
	cmd.SetArgs([]string{"-f", "-"})
	execErr := cmd.Execute()
	if execErr == nil || !strings.Contains(execErr.Error(), "no site configured") {
		t.Errorf("stdin: expected no-site error, got: %v", execErr)
	}
}

// TestCreateCmdUnknownFieldRejected verifies that a spec with an unknown field returns
// a parse error before the API is ever called.
func TestCreateCmdUnknownFieldRejected(t *testing.T) {
	spec := writeSpec(t, `
name: my-cluster
unknownField: should-fail
`)
	cmd := clusters.NewCreateCmd(clientFnErr)
	cmd.SetArgs([]string{"-f", spec})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown field in spec, got nil")
	}
	if strings.Contains(err.Error(), "no site configured") {
		t.Errorf("unknown field should fail before reaching the API, got: %v", err)
	}
}

// TestCreateCmdMalformedYAMLRejected verifies that invalid YAML returns a parse
// error before the API is ever called.
func TestCreateCmdMalformedYAMLRejected(t *testing.T) {
	spec := writeSpec(t, ":\t:")
	cmd := clusters.NewCreateCmd(clientFnErr)
	cmd.SetArgs([]string{"-f", spec})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if strings.Contains(err.Error(), "no site configured") {
		t.Errorf("malformed YAML should fail before reaching the API, got: %v", err)
	}
}

// TestDeleteCmdRequiresName verifies that delete cluster requires exactly one positional arg.
func TestDeleteCmdRequiresName(t *testing.T) {
	cmd := clusters.NewDeleteCmd(clientFnErr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when cluster name is omitted")
	}
}

// TestDeleteCmdReachesClient verifies that delete cluster with a name reaches
// the API call (returns "no site configured", not an arg/flag error).
func TestDeleteCmdReachesClient(t *testing.T) {
	cmd := clusters.NewDeleteCmd(clientFnErr)
	cmd.SetArgs([]string{"my-cluster"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no site configured") {
		t.Errorf("expected no-site error, got: %v", err)
	}
}

// writeSpec writes a YAML spec to a temp file and returns its path.
func writeSpec(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}
