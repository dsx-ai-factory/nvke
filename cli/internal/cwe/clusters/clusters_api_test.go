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

func TestUpdateClusterSendsThePatchAndReportsSuccess(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	out, err := run(t, NewUpdateCmd(clientFn), "my-cluster",
		"--set", "controlPlaneKubernetesVersion=1.31.0",
		"--set", "enableCPNodeRecycling=true",
	)
	if err != nil {
		t.Fatalf("update cluster: %v", err)
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

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want controlPlaneKubernetesVersion 1.31.0", stored)
	}

	if !strings.Contains(out, `"my-cluster" updated`) {
		t.Errorf("output does not confirm the update:\n%s", out)
	}
	if !strings.Contains(out, seed.ID.String()) {
		t.Errorf("output omits the cluster id:\n%s", out)
	}
}

func TestUpdateClusterFileLoadsThePatch(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewUpdateCmd(clientFn)
	cmd.SetIn(strings.NewReader(`controlPlaneKubernetesVersion: "1.31.0"`))
	_, err := run(t, cmd, "my-cluster", "-f", "-")
	if err != nil {
		t.Fatalf("update cluster -f -: %v", err)
	}

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want controlPlaneKubernetesVersion 1.31.0", stored)
	}
}

func TestUpdateClusterSetOverridesFile(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewUpdateCmd(clientFn)
	cmd.SetIn(strings.NewReader(`controlPlaneKubernetesVersion: "1.30.5"`))
	_, err := run(t, cmd, "my-cluster", "-f", "-", "--set", "controlPlaneKubernetesVersion=1.31.0")
	if err != nil {
		t.Fatalf("update cluster: %v", err)
	}

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want --set to win with 1.31.0", stored)
	}
}

// TestUpdateClusterSetAndFileMergeSiblingFields verifies that -f and --set
// touching different fields under the same nested object both survive in
// the PATCH body, instead of --set's object replacing -f's wholesale.
func TestUpdateClusterSetAndFileMergeSiblingFields(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewUpdateCmd(clientFn)
	cmd.SetIn(strings.NewReader("controlPlaneConfig:\n  nico:\n    osProfile: from-file\n"))
	_, err := run(t, cmd, "my-cluster", "-f", "-",
		"--set", "controlPlaneConfig.nico.configProfile=from-set",
	)
	if err != nil {
		t.Fatalf("update cluster: %v", err)
	}

	body := string(fake.lastRequestBody())
	if !strings.Contains(body, `"osProfile":"from-file"`) {
		t.Errorf("PATCH body = %s, want the -f field to survive the merge", body)
	}
	if !strings.Contains(body, `"configProfile":"from-set"`) {
		t.Errorf("PATCH body = %s, want the --set field to be present", body)
	}
}

// TestUpdateClusterFileFromRealPath verifies -f <path>, reading from an
// actual file rather than stdin.
func TestUpdateClusterFileFromRealPath(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	path := writeSpec(t, `controlPlaneKubernetesVersion: "1.31.0"`)
	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster", "-f", path)
	if err != nil {
		t.Fatalf("update cluster -f %s: %v", path, err)
	}

	stored := fake.stored()
	if len(stored) != 1 || stored[0].ControlPlaneKubernetesVersion != "1.31.0" {
		t.Fatalf("stored cluster = %+v, want controlPlaneKubernetesVersion 1.31.0", stored)
	}
}

// TestUpdateClusterEmptySetValueReachesTheWireAsALiteralEmptyArray verifies
// that --set controlPlaneConfig.nico.subnetIDs= sends a literal
// "subnetIDs":[] in the PATCH body.
func TestUpdateClusterEmptySetValueReachesTheWireAsALiteralEmptyArray(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster",
		"--set", "controlPlaneConfig.nico.subnetIDs=",
	)
	if err != nil {
		t.Fatalf("update cluster: %v", err)
	}

	body := fake.lastRequestBody()
	if !strings.Contains(string(body), `"subnetIDs":[]`) {
		t.Errorf("PATCH body = %s, want a literal \"subnetIDs\":[]", body)
	}
}

// TestUpdateClusterFileEmptyValueReachesTheWireAsALiteralEmptyObject
// verifies that an empty value in a -f YAML patch document sends a literal
// "machineTemplate":{} in the PATCH body.
func TestUpdateClusterFileEmptyValueReachesTheWireAsALiteralEmptyObject(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	cmd := NewUpdateCmd(clientFn)
	cmd.SetIn(strings.NewReader("controlPlaneConfig:\n  infra:\n    machineTemplate: {}\n"))
	_, err := run(t, cmd, "my-cluster", "-f", "-")
	if err != nil {
		t.Fatalf("update cluster -f -: %v", err)
	}

	body := fake.lastRequestBody()
	if !strings.Contains(string(body), `"machineTemplate":{}`) {
		t.Errorf("PATCH body = %s, want a literal \"machineTemplate\":{}", body)
	}
}

// TestUpdateClusterSetStringBypassesYAMLCoercion verifies --set-string's
// motivating case: --set controlPlaneKubernetesVersion=1.31 fails, because
// bare 1.31 parses as a YAML float and controlPlaneKubernetesVersion is a
// string field. --set-string keeps it a literal string, so the same value
// succeeds and reaches the wire as "1.31", not the number 1.31.
func TestUpdateClusterSetStringBypassesYAMLCoercion(t *testing.T) {
	seed := seedCluster("my-cluster")
	_, clientFn := newFakeCWE(t, seed)
	if _, err := run(t, NewUpdateCmd(clientFn), "my-cluster", "--set", "controlPlaneKubernetesVersion=1.31"); err == nil {
		t.Fatal("update cluster --set controlPlaneKubernetesVersion=1.31 unexpectedly succeeded")
	}

	fake, clientFn := newFakeCWE(t, seed)
	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster", "--set-string", "controlPlaneKubernetesVersion=1.31")
	if err != nil {
		t.Fatalf("update cluster --set-string controlPlaneKubernetesVersion=1.31: %v", err)
	}
	body := fake.lastRequestBody()
	if !strings.Contains(string(body), `"controlPlaneKubernetesVersion":"1.31"`) {
		t.Errorf("PATCH body = %s, want a literal string \"1.31\"", body)
	}
}

// TestUpdateClusterSetStringWinsOverSet verifies --set-string's precedence
// over --set on the same key, regardless of flag order.
func TestUpdateClusterSetStringWinsOverSet(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)

	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster",
		"--set-string", "controlPlaneKubernetesVersion=1.31",
		"--set", "controlPlaneKubernetesVersion=1.32.0",
	)
	if err != nil {
		t.Fatalf("update cluster: %v", err)
	}
	body := fake.lastRequestBody()
	if !strings.Contains(string(body), `"controlPlaneKubernetesVersion":"1.31"`) {
		t.Errorf("PATCH body = %s, want --set-string's value to win", body)
	}
}

// The name resolves successfully; only the PATCH is rejected. GET/list
// aren't blocked by an in-progress mutation on CWE, only a new PATCH racing
// it is. A 409 here also covers 400, since the client's error path doesn't
// branch on status code.
func TestUpdateClusterSurfacesServerRejection(t *testing.T) {
	seed := seedCluster("my-cluster")
	fake, clientFn := newFakeCWE(t, seed)
	fake.failUpdate(409, "k8s_upgrade is already in progress")

	_, err := run(t, NewUpdateCmd(clientFn), "my-cluster", "--set", "teleportEnabled=true")
	if err == nil {
		t.Fatal("update against a 409 server returned no error")
	}
	if !strings.Contains(err.Error(), "k8s_upgrade is already in progress") {
		t.Errorf("error = %v, want it to carry the server's message", err)
	}

	want := []string{
		"GET /api/clusters?name=my-cluster",
		"PATCH /api/clusters/" + seed.ID.String(),
	}
	if seen := fake.seen(); len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("requests = %v, want %v", seen, want)
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

func TestUpdateUnknownClusterReportsNotFound(t *testing.T) {
	_, clientFn := newFakeCWE(t)

	_, err := run(t, NewUpdateCmd(clientFn), "ghost", "--set", "teleportEnabled=true")
	if err == nil {
		t.Fatal("updating an unknown cluster returned no error")
	}
	if !strings.Contains(err.Error(), `cluster "ghost" not found`) {
		t.Errorf("error = %v, want a not-found error naming the cluster", err)
	}
}
