// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	api "github.com/dsx-ai-factory/nvke/controller/api"
)

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

// TestGetCmdNameFilterReachesClient verifies that a named cluster routes
// through the filtering path to the client call.
func TestGetCmdNameFilterReachesClient(t *testing.T) {
	cmd := NewGetCmd(clientFnErr)
	cmd.SetArgs([]string{"foo"})
	if err := cmd.Execute(); !isNotConfigured(err) {
		t.Errorf("expected no-site error, got: %v", err)
	}
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

// TestGetClustersOneNameIsOneObject mirrors kubectl and the operator get
// commands: with -o json or -o yaml, one name prints that cluster as a single
// object, while no name or several names print a list.
func TestGetClustersOneNameIsOneObject(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantList  []string
		wantOne   string
		wantError string
	}{
		{name: "one name as JSON is an object", args: []string{"beta", "-o", "json"}, wantOne: "beta"},
		{name: "one name as YAML is an object", args: []string{"beta", "-o", "yaml"}, wantOne: "beta"},
		{name: "no name as JSON is a list", args: []string{"-o", "json"}, wantList: []string{"alpha", "beta"}},
		{name: "two names as JSON are a list", args: []string{"beta", "alpha", "-o", "json"}, wantList: []string{"beta", "alpha"}},
		{name: "one missing name prints nothing", args: []string{"nope", "-o", "json"}, wantError: `cluster "nope" not found`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, clientFn := newFakeCWE(t, seedCluster("alpha"), seedCluster("beta"))
			cmd := NewGetCmd(clientFn)
			// The nvkectl root silences cobra's usage and error report, and
			// stderr is kept apart, so an empty stdout means the command
			// itself printed nothing for the missing name.
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			var stdout, stderr strings.Builder
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			out := stdout.String()

			if tt.wantError != "" {
				if err == nil || err.Error() != tt.wantError {
					t.Fatalf("error = %v, want %q", err, tt.wantError)
				}
				if out != "" {
					t.Errorf("stdout = %q, want nothing for a name that does not exist", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("get clusters %v: %v", tt.args, err)
			}
			// YAML is a superset of JSON, so one decoder reads both formats.
			if tt.wantOne != "" {
				var got map[string]any
				if err := yaml.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("output is not a single object: %v\n%s", err, out)
				}
				if got["name"] != tt.wantOne {
					t.Errorf("name = %v, want %q", got["name"], tt.wantOne)
				}
				return
			}
			var got []map[string]any
			if err := yaml.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not a list: %v\n%s", err, out)
			}
			names := make([]string, 0, len(got))
			for _, c := range got {
				name, _ := c["name"].(string)
				names = append(names, name)
			}
			if strings.Join(names, ",") != strings.Join(tt.wantList, ",") {
				t.Errorf("names = %v, want %v", names, tt.wantList)
			}
		})
	}
}

// The clientFnErr double can show that a name reached the client. It cannot
// show what the client then put on the wire, which is the part that decides
// whether filtering works at all.
func TestGetClustersFilterSendsTheNameQuery(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seedCluster("alpha"), seedCluster("beta"))

	out, err := run(t, NewGetCmd(clientFn), "beta")
	if err != nil {
		t.Fatalf("get clusters beta: %v", err)
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

func TestGetClustersAcceptsSeveralNames(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seedCluster("alpha"), seedCluster("beta"), seedCluster("gamma"))

	out, err := run(t, NewGetCmd(clientFn), "gamma", "alpha")
	if err != nil {
		t.Fatalf("get clusters gamma alpha: %v", err)
	}

	got := rows(out)
	if len(got) != 3 {
		t.Fatalf("rows = %d, want a header and two clusters:\n%s", len(got), out)
	}
	// Argument order, not the order the controller returns them in.
	for i, want := range []string{"gamma", "alpha"} {
		if got[i+1][0] != want {
			t.Errorf("row %d = %q, want %q", i, got[i+1][0], want)
		}
	}
	if seen := fake.seen(); len(seen) != 2 {
		t.Errorf("requests = %v, want one per name", seen)
	}
}

// TestGetClustersPrintsWhatItFound mirrors kubectl: the clusters that matched
// are printed, and the names that did not are reported as an error.
func TestGetClustersPrintsWhatItFound(t *testing.T) {
	_, clientFn := newFakeCWE(t, seedCluster("alpha"))

	out, err := run(t, NewGetCmd(clientFn), "alpha", "nope")
	if err == nil {
		t.Fatal("want an error for the missing cluster")
	}
	if !strings.Contains(err.Error(), `cluster "nope" not found`) {
		t.Errorf("error = %q, want it to name the missing cluster", err)
	}
	table, _, _ := strings.Cut(out, "Error:")
	if got := rows(table); len(got) != 2 || got[1][0] != "alpha" {
		t.Errorf("output = %q, want the cluster that was found to still be printed", out)
	}
}

func TestGetClustersRejectsNamesThatMatchNothing(t *testing.T) {
	_, clientFn := newFakeCWE(t, seedCluster("alpha"))

	if _, err := run(t, NewGetCmd(clientFn), "one", "two"); err == nil ||
		!strings.Contains(err.Error(), `clusters "one", "two" not found`) {
		t.Errorf("error = %v, want both missing names reported", err)
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
