// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	api "github.com/dsx-ai-factory/nke/controller/api"
)

// writeSpec writes a spec file named name and returns its path.
func writeSpec(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing spec: %v", err)
	}
	return path
}

func TestCreateNodePoolFromFile(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
	clusterID := fake.clusterIDFor(t, "alpha")

	spec := fmt.Sprintf(`clusterID: %s
name: gpu-workers
nodeNamePrefix: gpu
desiredNodeCount: 2
nodeLabels:
  role: gpu
nodeTaints:
  - key: nvidia.com/gpu
    value: "true"
    effect: NoSchedule
  - key: dedicated
    effect: PreferNoSchedule
`, clusterID)

	out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pool.yaml", spec))
	if err != nil {
		t.Fatalf("create: %v (output %q)", err, out)
	}
	if !strings.Contains(out, `Node pool "gpu-workers" created`) {
		t.Errorf("output missing the created line: %q", out)
	}
	if !strings.Contains(out, "nkectl get nodepools --cluster") {
		t.Errorf("output missing the status hint: %q", out)
	}

	if got := fake.seen(); len(got) != 1 || got[0] != "POST /api/nodePools" {
		t.Fatalf("requests = %v, want one POST /api/nodePools", got)
	}

	bodies := fake.createBodies()
	if len(bodies) != 1 {
		t.Fatalf("create bodies = %v, want 1", bodies)
	}
	body := bodies[0]
	if body["clusterID"] != clusterID.String() {
		t.Errorf("clusterID = %v, want %s", body["clusterID"], clusterID)
	}
	if body["name"] != "gpu-workers" || body["nodeNamePrefix"] != "gpu" {
		t.Errorf("name/prefix = %v/%v", body["name"], body["nodeNamePrefix"])
	}
	if body["desiredNodeCount"] != float64(2) {
		t.Errorf("desiredNodeCount = %v, want 2", body["desiredNodeCount"])
	}
	labels, ok := body["nodeLabels"].(map[string]any)
	if !ok || labels["role"] != "gpu" {
		t.Errorf("nodeLabels = %v, want map[role:gpu]", body["nodeLabels"])
	}
	wantTaints := []any{
		map[string]any{"key": "nvidia.com/gpu", "value": "true", "effect": "NoSchedule"},
		map[string]any{"key": "dedicated", "effect": "PreferNoSchedule"},
	}
	if !reflect.DeepEqual(body["nodeTaints"], wantTaints) {
		t.Errorf("nodeTaints = %v, want %v", body["nodeTaints"], wantTaints)
	}
	if _, present := body["controlPlane"]; present {
		t.Errorf("controlPlane was sent though the spec omitted it: %v", body)
	}

	pools := fake.stored()
	if len(pools) != 2 {
		t.Fatalf("stored %d pools, want 2", len(pools))
	}
	// What the controller stored is what the pool reports back through get.
	created := pools[1]
	if want := map[string]string{"role": "gpu"}; !reflect.DeepEqual(created.NodeLabels, want) {
		t.Errorf("stored nodeLabels = %v, want %v", created.NodeLabels, want)
	}
	wantStored := []api.NodeTaint{
		{Key: "nvidia.com/gpu", Value: "true", Effect: api.NodeTaintEffectNoSchedule},
		{Key: "dedicated", Effect: api.NodeTaintEffectPreferNoSchedule},
	}
	if !reflect.DeepEqual(created.NodeTaints, wantStored) {
		t.Errorf("stored nodeTaints = %v, want %v", created.NodeTaints, wantStored)
	}
}

func TestCreateNodePoolFromStdin(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
	clusterID := fake.clusterIDFor(t, "alpha")

	cmd := NewCreateCmd(clientFn)
	cmd.SetIn(bytes.NewBufferString(fmt.Sprintf(
		"clusterID: %s\nname: from-stdin\nnodeNamePrefix: gpu\ndesiredNodeCount: 1\n", clusterID,
	)))

	out, err := run(t, cmd, "-f", "-")
	if err != nil {
		t.Fatalf("create: %v (output %q)", err, out)
	}
	if !strings.Contains(out, `Node pool "from-stdin" created`) {
		t.Errorf("output = %q", out)
	}
	if bodies := fake.createBodies(); len(bodies) != 1 || bodies[0]["name"] != "from-stdin" {
		t.Errorf("create bodies = %v", fake.createBodies())
	}
}

func TestCreateNodePoolFromJSON(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
	clusterID := fake.clusterIDFor(t, "alpha")

	spec := fmt.Sprintf(
		`{"clusterID": %q, "name": "json-pool", "nodeNamePrefix": "gpu", "desiredNodeCount": 3}`, clusterID,
	)

	out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pool.json", spec))
	if err != nil {
		t.Fatalf("create: %v (output %q)", err, out)
	}
	if !strings.Contains(out, `Node pool "json-pool" created`) {
		t.Errorf("output = %q", out)
	}
	if bodies := fake.createBodies(); len(bodies) != 1 || bodies[0]["desiredNodeCount"] != float64(3) {
		t.Errorf("create bodies = %v", fake.createBodies())
	}
}

func TestCreateNodePoolRejectsMultipleDocuments(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
	clusterID := fake.clusterIDFor(t, "alpha")

	spec := fmt.Sprintf(`clusterID: %[1]s
name: first
nodeNamePrefix: one
desiredNodeCount: 1
---
clusterID: %[1]s
name: second
nodeNamePrefix: two
desiredNodeCount: 1
`, clusterID)

	out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pools.yaml", spec))
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("error = %v, want it to mention multiple documents", err)
	}
	// Nothing may be created: silently taking the first document is the bug.
	if got := fake.seen(); len(got) != 0 {
		t.Errorf("requests = %v, want none", got)
	}
}

func TestCreateNodePoolMissingRequiredField(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
	clusterID := fake.clusterIDFor(t, "alpha")

	spec := fmt.Sprintf("clusterID: %s\nname: no-prefix\ndesiredNodeCount: 1\n", clusterID)

	out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pool.yaml", spec))
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "nodeNamePrefix") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
}

func TestCreateNodePoolUnknownField(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
	clusterID := fake.clusterIDFor(t, "alpha")

	spec := fmt.Sprintf(
		"clusterID: %s\nname: bad\nnodeNamePrefix: gpu\ndesiredNodeCount: 1\ntaints: []\n", clusterID,
	)

	out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pool.yaml", spec))
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "taints") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
	if got := fake.seen(); len(got) != 0 {
		t.Errorf("requests = %v, want none", got)
	}
}

func TestCreateNodePoolRequiresFilename(t *testing.T) {
	_, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})

	out, err := run(t, NewCreateCmd(clientFn))
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "filename") {
		t.Errorf("error = %v, want it to name the required flag", err)
	}
}

func TestCreateNodePoolMissingFile(t *testing.T) {
	_, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})

	path := filepath.Join(t.TempDir(), "absent.yaml")
	out, err := run(t, NewCreateCmd(clientFn), "-f", path)
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "opening") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the file it could not open", err)
	}
}

func TestCreateNodePoolServerErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   []string
		// fields is appended to the shared worker spec. A row that sets it
		// also asserts that the request carried them to the controller.
		fields string
		sent   map[string]any
	}{
		{
			name:   "bad request",
			status: 400,
			body:   `{"message": "desiredNodeCount must be positive"}`,
			want:   []string{"400", "desiredNodeCount must be positive"},
		},
		{
			// Rules the controller owns, such as no taints on a control-plane
			// pool, are not repeated in the CLI and reach the user verbatim.
			name:   "taints on a control-plane pool",
			status: 400,
			body:   `{"message": "nodeTaints are not allowed on a control-plane pool"}`,
			want:   []string{"400", "nodeTaints are not allowed on a control-plane pool"},
			fields: "controlPlane: true\nnodeTaints:\n  - key: dedicated\n    effect: NoSchedule\n",
			sent: map[string]any{
				"controlPlane": true,
				"nodeTaints":   []any{map[string]any{"key": "dedicated", "effect": "NoSchedule"}},
			},
		},
		{
			name:   "cluster deleting",
			status: 409,
			body:   `{"message": "cluster is in the process of being deleted"}`,
			want:   []string{"409", "cluster is being deleted", "cluster is in the process of being deleted"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
			clusterID := fake.clusterIDFor(t, "alpha")
			fake.failCreateWith(tc.status, tc.body)

			spec := fmt.Sprintf(
				"clusterID: %s\nname: gpu-workers\nnodeNamePrefix: gpu\ndesiredNodeCount: 1\n%s", clusterID, tc.fields,
			)

			out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pool.yaml", spec))
			if err == nil {
				t.Fatalf("expected an error, got output %q", out)
			}
			if len(tc.sent) > 0 {
				bodies := fake.createBodies()
				if len(bodies) != 1 {
					t.Fatalf("create requests = %d, want 1", len(bodies))
				}
				for k, want := range tc.sent {
					if !reflect.DeepEqual(bodies[0][k], want) {
						t.Errorf("sent %s = %v, want %v", k, bodies[0][k], want)
					}
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestCreateNodePoolRejectsInvalidLabelsAndTaints verifies that a spec which
// cannot decode into the create request fails before any request is sent:
// nodeLabels is a map of keys to values, so the old key=value list is refused,
// and a taint carries only key, value and effect. The values themselves, such
// as a taint effect, are validated by the controller and not repeated here.
func TestCreateNodePoolRejectsInvalidLabelsAndTaints(t *testing.T) {
	tests := []struct {
		name    string
		fields  string
		wantErr string
	}{
		{
			name:    "labels as the old key=value list",
			fields:  "nodeLabels:\n  - role=gpu\n",
			wantErr: "nodeLabels",
		},
		{
			name:    "taint with an unknown field",
			fields:  "nodeTaints:\n  - key: dedicated\n    effect: NoSchedule\n    timeAdded: now\n",
			wantErr: "timeAdded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "existing", desiredNodeCount: 1})
			clusterID := fake.clusterIDFor(t, "alpha")

			spec := fmt.Sprintf("clusterID: %s\nname: bad\nnodeNamePrefix: gpu\ndesiredNodeCount: 1\n%s", clusterID, tc.fields)

			out, err := run(t, NewCreateCmd(clientFn), "-f", writeSpec(t, "pool.yaml", spec))
			if err == nil {
				t.Fatalf("expected an error, got output %q", out)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if got := fake.seen(); len(got) != 0 {
				t.Errorf("requests = %v, want none", got)
			}
		})
	}
}
