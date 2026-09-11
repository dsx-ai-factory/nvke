// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	api "github.com/NVIDIA/nke/controller/api"
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

// rows splits table output into whitespace-separated fields per line, so the
// assertions do not depend on tabwriter's column padding.
func rows(out string) [][]string {
	var got [][]string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			got = append(got, f)
		}
	}
	return got
}

func TestGetClustersPrintsATableRow(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seedCluster("alpha"), seedCluster("beta"))

	out, err := run(t, NewGetCmd(clientFn))
	if err != nil {
		t.Fatalf("get clusters: %v", err)
	}

	got := rows(out)
	if len(got) != 3 {
		t.Fatalf("want a header and two rows, got %d lines:\n%s", len(got), out)
	}
	if got[0][0] != "NAME" {
		t.Errorf("header = %v, want it to start with NAME", got[0])
	}
	// Name, version, CNI and status all come off the wire, so this fails if
	// any of them stops being decoded or stops being printed.
	for i, want := range []string{"alpha", "beta"} {
		row := got[i+1]
		if row[0] != want {
			t.Errorf("row %d name = %q, want %q", i, row[0], want)
		}
		if row[1] != "1.30.0" {
			t.Errorf("row %d kubernetes version = %q, want 1.30.0", i, row[1])
		}
		if row[2] != "calico" {
			t.Errorf("row %d cni = %q, want calico", i, row[2])
		}
		if row[3] != "Provisioned" {
			t.Errorf("row %d status = %q, want Provisioned", i, row[3])
		}
	}

	if seen := fake.seen(); len(seen) != 1 || seen[0] != "GET /api/clusters" {
		t.Errorf("requests = %v, want one GET /api/clusters", seen)
	}
}

func TestGetClustersJSONRoundTrips(t *testing.T) {
	_, clientFn := newFakeCWE(t, seedCluster("alpha"))

	out, err := run(t, NewGetCmd(clientFn), "-o", "json")
	if err != nil {
		t.Fatalf("get clusters -o json: %v", err)
	}

	var got []api.Cluster
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("decoded %d clusters %+v, want one named alpha", len(got), got)
	}
	if got[0].CniPlugin != "calico" {
		t.Errorf("cniPlugin = %q, want calico", got[0].CniPlugin)
	}
}

// The old clientFnErr double could show that --cluster reached the client. It
// could not show what the client then put on the wire, which is the part that
// decides whether filtering works at all.
func TestGetClustersFilterSendsTheNameQuery(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seedCluster("alpha"), seedCluster("beta"))

	out, err := run(t, NewGetCmd(clientFn), "--cluster", "beta")
	if err != nil {
		t.Fatalf("get clusters --cluster beta: %v", err)
	}

	if seen := fake.seen(); len(seen) != 1 || seen[0] != "GET /api/clusters?name=beta" {
		t.Fatalf("requests = %v, want one GET /api/clusters?name=beta", seen)
	}
	if strings.Contains(out, "alpha") {
		t.Errorf("filtered output names the other cluster:\n%s", out)
	}
	if !strings.Contains(out, "beta") {
		t.Errorf("filtered output does not name beta:\n%s", out)
	}
}

func TestCreateClusterPostsTheSpecAndReportsTheID(t *testing.T) {
	fake, clientFn := newFakeCWE(t)

	cmd := NewCreateCmd(clientFn)
	cmd.SetIn(strings.NewReader(`
name: my-cluster
controlPlaneKubernetesVersion: "1.30.0"
cniPlugin: calico
etcdEncrypted: false
`))
	out, err := run(t, cmd, "-f", "-")
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	if seen := fake.seen(); len(seen) != 1 || seen[0] != "POST /api/clusters" {
		t.Fatalf("requests = %v, want one POST /api/clusters", seen)
	}

	// The fake builds its record from the body it was sent, so this asserts
	// the CLI serialised the spec rather than dropping fields.
	stored := fake.stored()
	if len(stored) != 1 {
		t.Fatalf("fake holds %d clusters, want 1", len(stored))
	}
	if stored[0].Name != "my-cluster" {
		t.Errorf("posted name = %q, want my-cluster", stored[0].Name)
	}
	if stored[0].ControlPlaneKubernetesVersion != "1.30.0" {
		t.Errorf("posted kubernetes version = %q, want 1.30.0", stored[0].ControlPlaneKubernetesVersion)
	}
	if stored[0].CniPlugin != "calico" {
		t.Errorf("posted cni plugin = %q, want calico", stored[0].CniPlugin)
	}

	if !strings.Contains(out, `"my-cluster" created`) {
		t.Errorf("output does not confirm creation:\n%s", out)
	}
	if !strings.Contains(out, stored[0].ID.String()) {
		t.Errorf("output omits the new cluster id:\n%s", out)
	}
}

func TestPatchClusterSendsThePatchAndReportsSuccess(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	out, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch", "controlPlaneKubernetesVersion: 1.31.0")
	if err != nil {
		t.Fatalf("patch cluster: %v", err)
	}

	want := []string{
		"GET /api/clusters?name=my-cluster",
		"PATCH /api/clusters/" + seed.ID.String(),
	}
	seen := fake.seen()
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
	if !strings.Contains(out, `"my-cluster" patched`) {
		t.Errorf("output does not confirm the patch:\n%s", out)
	}
}

// TestPatchClusterMultipleFieldsInOnePatch verifies that a single --patch
// document combining a top-level scalar, a top-level bool, and nested
// object fields sends all of them together, and that the ones the fake
// tracks are actually applied to the stored cluster.
func TestPatchClusterMultipleFieldsInOnePatch(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	patch := `
controlPlaneKubernetesVersion: "1.31.0"
teleportEnabled: true
controlPlaneConfig:
  desiredCount: 3
  maxSurge: 1
`
	_, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch", patch)
	if err != nil {
		t.Fatalf("patch cluster: %v", err)
	}

	body := fake.lastRequestBody()
	for _, want := range []string{
		`"controlPlaneKubernetesVersion":"1.31.0"`,
		`"teleportEnabled":true`,
		`"desiredCount":3`,
		`"maxSurge":1`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("PATCH body = %s, want it to contain %s", body, want)
		}
	}

	stored := fake.stored()
	if len(stored) != 1 {
		t.Fatalf("fake holds %d clusters, want 1", len(stored))
	}
	if stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Errorf("stored controlPlaneKubernetesVersion = %q, want 1.31.0", stored[0].ControlPlaneKubernetesVersion)
	}
	if stored[0].TeleportEnabled == nil || !*stored[0].TeleportEnabled {
		t.Errorf("stored teleportEnabled = %v, want true", stored[0].TeleportEnabled)
	}
}

// TestPatchClusterFileLoadsThePatch verifies --patch-file reads a real file
// path (not just stdin) and its fields reach the wire.
func TestPatchClusterFileLoadsThePatch(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	path := writeSpec(t, `controlPlaneKubernetesVersion: "1.31.0"`)
	_, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch-file", path)
	if err != nil {
		t.Fatalf("patch cluster --patch-file %s: %v", path, err)
	}

	body := fake.lastRequestBody()
	if !bytes.Contains(body, []byte(`"controlPlaneKubernetesVersion":"1.31.0"`)) {
		t.Errorf("PATCH body = %s, want controlPlaneKubernetesVersion 1.31.0", body)
	}
}

// TestPatchClusterStdin verifies --patch-file - reads the patch from stdin.
func TestPatchClusterStdin(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewPatchCmd(clientFn)
	cmd.SetIn(strings.NewReader("teleportEnabled: true"))
	_, err := run(t, cmd, "my-cluster", "--patch-file", "-")
	if err != nil {
		t.Fatalf("patch cluster --patch-file -: %v", err)
	}

	body := fake.lastRequestBody()
	if !bytes.Contains(body, []byte(`"teleportEnabled":true`)) {
		t.Errorf("PATCH body = %s, want teleportEnabled true", body)
	}
}

// TestPatchClusterEmptyValueReachesTheWireAsALiteralEmptyArray verifies that
// an empty array literal in the patch document sends a literal
// "preKubeadmCommands":[] in the PATCH body, not silently dropped by
// omitempty during a re-marshal.
func TestPatchClusterEmptyValueReachesTheWireAsALiteralEmptyArray(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	_, err := run(t, NewPatchCmd(clientFn), "my-cluster",
		"--patch", "controlPlaneConfig: {infra: {preKubeadmCommands: []}}",
	)
	if err != nil {
		t.Fatalf("patch cluster: %v", err)
	}

	body := fake.lastRequestBody()
	if !bytes.Contains(body, []byte(`"preKubeadmCommands":[]`)) {
		t.Errorf("PATCH body = %s, want a literal \"preKubeadmCommands\":[]", body)
	}
}

// The name resolves successfully; only the PATCH is rejected. GET/list
// aren't blocked by an in-progress mutation on CWE, only a new PATCH racing
// it is.
func TestPatchClusterSurfacesServerRejection(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)
	fake.failPatch(409, "another operation is already in progress")

	_, err := run(t, NewPatchCmd(clientFn), "my-cluster", "--patch", "teleportEnabled: true")
	if err == nil {
		t.Fatal("expected the server rejection to surface, got nil")
	}
	if !strings.Contains(err.Error(), "another operation is already in progress") {
		t.Errorf("error = %v, want it to carry the server's message", err)
	}
}

func TestPatchUnknownClusterReportsNotFound(t *testing.T) {
	_, clientFn := newFakeCWE(t)

	_, err := run(t, NewPatchCmd(clientFn), "nope", "--patch", "teleportEnabled: true")
	if err == nil {
		t.Fatal("expected a not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to mention not found", err)
	}
}

func TestDeleteClusterSendsDeleteForTheResolvedID(t *testing.T) {
	seed := seedCluster("doomed")
	fake, clientFn := newFakeCWE(t, seed)

	out, err := run(t, NewDeleteCmd(clientFn), "doomed")
	if err != nil {
		t.Fatalf("delete cluster doomed: %v", err)
	}

	want := []string{
		"GET /api/clusters?name=doomed",
		"DELETE /api/clusters/" + seed.ID.String(),
	}
	seen := fake.seen()
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
	if left := fake.stored(); len(left) != 0 {
		t.Errorf("fake still holds %d clusters, want none", len(left))
	}
	if !strings.Contains(out, `"doomed" deletion started`) {
		t.Errorf("output does not confirm deletion:\n%s", out)
	}
}

// The client parses {"message": ...} out of an error body. Nothing exercised
// that before, so a regression that printed the raw body, or the status alone,
// was invisible.
func TestServerErrorSurfacesTheAPIMessage(t *testing.T) {
	clientFn := newBrokenCWE(t, 500, `{"message":"database is on fire"}`)

	_, err := run(t, NewGetCmd(clientFn))
	if err == nil {
		t.Fatal("get clusters against a failing server returned no error")
	}
	if !strings.Contains(err.Error(), "database is on fire") {
		t.Errorf("error = %v, want it to carry the server's message", err)
	}
}
