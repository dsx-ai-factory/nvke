// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// run executes cmd with args and returns everything it wrote.
func run(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// writePatch writes a patch file and returns its path.
func writePatch(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "patch.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing patch: %v", err)
	}
	return path
}

func TestPatchNodePoolScales(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})

	out, err := run(t, NewPatchCmd(clientFn),
		"gpu-workers", "--cluster", "alpha", "--patch", "desiredNodeCount: 8")
	if err != nil {
		t.Fatalf("patch nodepool: %v", err)
	}

	if got := fake.stored()[0].DesiredNodeCount; got != 8 {
		t.Errorf("stored desired node count = %d, want 8", got)
	}
	for _, want := range []string{`"gpu-workers"`, "patched", "async"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The hint must point at the command that shows the result. The cluster
	// table has no node pool columns, so it must not name that one.
	if strings.Contains(out, "get clusters") {
		t.Errorf("output points at a command that cannot show the change:\n%s", out)
	}
	if !strings.Contains(out, "get nodepools --cluster alpha") {
		t.Errorf("output does not point at the command that shows the change:\n%s", out)
	}

	seen := fake.seen()
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want a list then a patch", seen)
	}
	// The pool is resolved with both filters, so a name that is only unique
	// within its cluster cannot select a pool from another one.
	if !strings.Contains(seen[0], "clusterName=alpha") || !strings.Contains(seen[0], "name=gpu-workers") {
		t.Errorf("list request = %q, want a GET filtered by cluster name and pool name", seen[0])
	}
	if !strings.HasPrefix(seen[1], "PATCH /api/nodePools/"+fake.stored()[0].ID.String()) {
		t.Errorf("patch request = %q, want a PATCH of the resolved pool", seen[1])
	}
}

// TestPatchNodePoolSendsOnlyTheGivenFields pins the request body. A field the
// operator did not name must stay absent, because a field present in the body
// is a field CWE overwrites.
func TestPatchNodePoolSendsOnlyTheGivenFields(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})

	if _, err := run(t, NewPatchCmd(clientFn),
		"gpu-workers", "--cluster", "alpha", "--patch", "desiredNodeCount: 8"); err != nil {
		t.Fatalf("patch nodepool: %v", err)
	}

	bodies := fake.patchBodies()
	if len(bodies) != 1 {
		t.Fatalf("patch bodies = %d, want 1", len(bodies))
	}
	if len(bodies[0]) != 1 {
		t.Fatalf("patch body = %v, want desiredNodeCount alone", bodies[0])
	}
	// A number, not the string "8": CWE rejects a string for an integer field.
	if got, ok := bodies[0]["desiredNodeCount"].(float64); !ok || int(got) != 8 {
		t.Errorf("patch body desiredNodeCount = %#v, want the number 8", bodies[0]["desiredNodeCount"])
	}
}

func TestPatchNodePoolAcceptsJSON(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})

	if _, err := run(t, NewPatchCmd(clientFn),
		"gpu-workers", "--cluster", "alpha", "--patch", `{"desiredNodeCount": 6}`); err != nil {
		t.Fatalf("patch nodepool: %v", err)
	}
	if got := fake.stored()[0].DesiredNodeCount; got != 6 {
		t.Errorf("stored desired node count = %d, want 6", got)
	}
}

func TestPatchNodePoolReadsAPatchFile(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})
	path := writePatch(t, "desiredNodeCount: 5\n")

	if _, err := run(t, NewPatchCmd(clientFn),
		"gpu-workers", "--cluster", "alpha", "--patch-file", path); err != nil {
		t.Fatalf("patch nodepool: %v", err)
	}
	if got := fake.stored()[0].DesiredNodeCount; got != 5 {
		t.Errorf("stored desired node count = %d, want 5", got)
	}
}

func TestPatchNodePoolReadsStdin(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})

	cmd := NewPatchCmd(clientFn)
	cmd.SetIn(strings.NewReader("desiredNodeCount: 4\n"))
	if _, err := run(t, cmd, "gpu-workers", "--cluster", "alpha", "--patch-file", "-"); err != nil {
		t.Fatalf("patch nodepool: %v", err)
	}
	if got := fake.stored()[0].DesiredNodeCount; got != 4 {
		t.Errorf("stored desired node count = %d, want 4", got)
	}
}

func TestPatchNodePoolResolvesWithinTheNamedCluster(t *testing.T) {
	fake, clientFn := newFakeCWE(t,
		seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3},
		seed{clusterName: "beta", name: "gpu-workers", desiredNodeCount: 5},
	)

	if _, err := run(t, NewPatchCmd(clientFn),
		"gpu-workers", "--cluster", "beta", "--patch", "desiredNodeCount: 9"); err != nil {
		t.Fatalf("patch nodepool: %v", err)
	}

	stored := fake.stored()
	if stored[0].DesiredNodeCount != 3 {
		t.Errorf("alpha's pool = %d, want it left at 3", stored[0].DesiredNodeCount)
	}
	if stored[1].DesiredNodeCount != 9 {
		t.Errorf("beta's pool = %d, want 9", stored[1].DesiredNodeCount)
	}
}

func TestPatchNodePoolErrors(t *testing.T) {
	tests := []struct {
		name     string
		seeds    []seed
		args     []string
		clientFn func(*testing.T) CWEClientFn
		wantErr  string
	}{
		{
			name:    "node pool not found",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"cpu-workers", "--cluster", "alpha", "--patch", "desiredNodeCount: 8"},
			wantErr: `node pool "cpu-workers" not found in cluster "alpha"`,
		},
		{
			name:    "node pool is in another cluster",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "--cluster", "beta", "--patch", "desiredNodeCount: 8"},
			wantErr: `node pool "gpu-workers" not found in cluster "beta"`,
		},
		{
			name: "node pool name is ambiguous within the cluster",
			seeds: []seed{
				{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3},
				{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 5},
			},
			args:    []string{"gpu-workers", "--cluster", "alpha", "--patch", "desiredNodeCount: 8"},
			wantErr: "is ambiguous",
		},
		{
			name:    "no patch given",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "--cluster", "alpha"},
			wantErr: "nothing to patch: pass --patch or --patch-file",
		},
		{
			name:    "both patch inputs given",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "-c", "alpha", "--patch", "desiredNodeCount: 8", "--patch-file", "p.yaml"},
			wantErr: "pass only one of --patch or --patch-file",
		},
		{
			name:    "cluster flag missing",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "--patch", "desiredNodeCount: 8"},
			wantErr: `required flag(s) "cluster" not set`,
		},
		{
			name:    "node pool name missing",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"--cluster", "alpha", "--patch", "desiredNodeCount: 8"},
			wantErr: "accepts 1 arg(s)",
		},
		{
			// The field is real and CWE would take it; nkectl does not offer
			// it yet, and says which fields it does.
			name:    "patch carries an unsupported field",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "-c", "alpha", "--patch", "maxSurge: 2"},
			wantErr: `unknown field "maxSurge"; nkectl can set desiredNodeCount`,
		},
		{
			// Left to reach CWE, a null comes back as a validator message
			// naming a JSON pointer. Refused locally instead.
			name:    "patch sets a null",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "-c", "alpha", "--patch", "desiredNodeCount:"},
			wantErr: "null is not a value; omit the field to leave it unchanged",
		},
		{
			name:    "patch document is empty",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "-c", "alpha", "--patch", "{}"},
			wantErr: "nothing to patch: the patch document is empty",
		},
		{
			name:    "patch file does not exist",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "-c", "alpha", "--patch-file", "/nonexistent/patch.yaml"},
			wantErr: "opening",
		},
		{
			// A rejected update, such as a scale-up the cluster's pod CIDR has
			// no room for, must reach the operator with CWE's own explanation.
			name: "server rejects the patch",
			args: []string{"gpu-workers", "-c", "alpha", "--patch", "desiredNodeCount: 8"},
			clientFn: func(t *testing.T) CWEClientFn {
				t.Helper()
				fake, fn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})
				fake.failPatchWith(400, `{"message":"pod CIDR has no room for 8 nodes"}`)
				return fn
			},
			// The "patching node pool" prefix pins the failure to the PATCH. A
			// fake that rejected the lookup too would produce "finding node
			// pool: ..." and never reach the patch.
			wantErr: "patching node pool: 400 Bad Request: pod CIDR has no room for 8 nodes",
		},
		{
			name: "server rejects the lookup",
			args: []string{"gpu-workers", "-c", "alpha", "--patch", "desiredNodeCount: 8"},
			clientFn: func(t *testing.T) CWEClientFn {
				t.Helper()
				return newBrokenCWE(t, 503, `{"message":"upstream unavailable"}`)
			},
			wantErr: "finding node pool: listing node pools: 503 Service Unavailable: upstream unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientFn := tt.clientFn
			if clientFn == nil {
				clientFn = func(t *testing.T) CWEClientFn {
					t.Helper()
					_, fn := newFakeCWE(t, tt.seeds...)
					return fn
				}
			}
			out, err := run(t, NewPatchCmd(clientFn(t)), tt.args...)
			if err == nil {
				t.Fatalf("want an error, got output:\n%s", out)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
