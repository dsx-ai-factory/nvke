// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	api "github.com/NVIDIA/nke/controller/api"
)

// fields splits a table row into its columns, so an assertion does not depend
// on the widths tabwriter chose.
func fields(line string) []string { return strings.Fields(line) }

// rows returns the non-empty lines of a table, header first.
func rows(out string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			got = append(got, line)
		}
	}
	return got
}

func TestGetNodePoolsPrintsATable(t *testing.T) {
	fake, clientFn := newFakeCWE(t,
		seed{clusterName: "alpha", name: "control-plane", desiredNodeCount: 3, controlPlane: true},
		seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8},
		seed{clusterName: "beta", name: "gpu-workers", desiredNodeCount: 1},
	)

	out, err := run(t, NewGetCmd(clientFn))
	if err != nil {
		t.Fatalf("get nodepools: %v", err)
	}

	got := rows(out)
	if len(got) != 4 {
		t.Fatalf("rows = %d, want a header and three pools:\n%s", len(got), out)
	}
	wantHeader := []string{"NAME", "CLUSTER", "DESIRED", "K8S", "VERSION", "CP", "STATUS", "ID"}
	if header := fields(got[0]); !equal(header, wantHeader) {
		t.Errorf("header = %v, want %v", header, wantHeader)
	}

	stored := fake.stored()
	want := [][]string{
		{"control-plane", "alpha", "3", "1.30.0", "true", "Provisioned", stored[0].ID.String()},
		{"gpu-workers", "alpha", "8", "1.30.0", "false", "Provisioned", stored[1].ID.String()},
		{"gpu-workers", "beta", "1", "1.30.0", "false", "Provisioned", stored[2].ID.String()},
	}
	for i, wantRow := range want {
		if row := fields(got[i+1]); !equal(row, wantRow) {
			t.Errorf("row %d = %v, want %v", i, row, wantRow)
		}
	}

	// One extra call however many pools: a per-pool lookup would be three.
	if seen := fake.seen(); len(seen) != 2 ||
		!strings.HasPrefix(seen[0], "GET /api/nodePools") || seen[1] != "GET /api/clusters" {
		t.Errorf("requests = %v, want one node pool list and one cluster list", fake.seen())
	}
}

// TestGetNodePoolsSendsFiltersAsQueryParams pins that the filters go to CWE.
// Filtering locally would pass these assertions while fetching every pool.
func TestGetNodePoolsSendsFiltersAsQueryParams(t *testing.T) {
	seeds := []seed{
		{clusterName: "alpha", name: "control-plane", desiredNodeCount: 3, controlPlane: true},
		{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8},
		{clusterName: "beta", name: "gpu-workers", desiredNodeCount: 1},
	}
	tests := []struct {
		name      string
		args      []string
		wantQuery []string
		wantPools [][2]string // name and cluster of each printed row
	}{
		{
			name:      "no filter",
			wantQuery: nil,
			wantPools: [][2]string{{"control-plane", "alpha"}, {"gpu-workers", "alpha"}, {"gpu-workers", "beta"}},
		},
		{
			name:      "by cluster",
			args:      []string{"--cluster", "alpha"},
			wantQuery: []string{"clusterName=alpha"},
			wantPools: [][2]string{{"control-plane", "alpha"}, {"gpu-workers", "alpha"}},
		},
		{
			name:      "by node pool",
			args:      []string{"gpu-workers"},
			wantQuery: []string{"name=gpu-workers"},
			wantPools: [][2]string{{"gpu-workers", "alpha"}, {"gpu-workers", "beta"}},
		},
		{
			name:      "by cluster and node pool",
			args:      []string{"gpu-workers", "--cluster", "beta"},
			wantQuery: []string{"clusterName=beta", "name=gpu-workers"},
			wantPools: [][2]string{{"gpu-workers", "beta"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, clientFn := newFakeCWE(t, seeds...)

			out, err := run(t, NewGetCmd(clientFn), tt.args...)
			if err != nil {
				t.Fatalf("get nodepools: %v", err)
			}

			// The node pool list, which is not necessarily the first request:
			// resolving --cluster hits /clusters first.
			var list string
			for _, req := range fake.seen() {
				if strings.Contains(req, "/nodePools") {
					list = req
					break
				}
			}
			if list == "" {
				t.Fatalf("no /nodePools request was made, saw %v", fake.seen())
			}
			for _, q := range tt.wantQuery {
				if !strings.Contains(list, q) {
					t.Errorf("list request = %q, want it to carry %q", list, q)
				}
			}
			if tt.wantQuery == nil && strings.Contains(list, "?") {
				t.Errorf("list request = %q, want no query parameters", list)
			}

			got := rows(out)
			if len(got) != len(tt.wantPools)+1 {
				t.Fatalf("rows = %d, want %d pools and a header:\n%s", len(got), len(tt.wantPools), out)
			}
			for i, want := range tt.wantPools {
				row := fields(got[i+1])
				if row[0] != want[0] || row[1] != want[1] {
					t.Errorf("row %d = %v, want pool %q of cluster %q", i, row, want[0], want[1])
				}
			}
		})
	}
}

func TestGetNodePoolsDoesNotListEveryClusterWhenFiltered(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})

	out, err := run(t, NewGetCmd(clientFn), "--cluster", "alpha")
	if err != nil {
		t.Fatalf("get nodepools: %v", err)
	}
	if row := fields(rows(out)[1]); row[1] != "alpha" {
		t.Errorf("cluster column = %q, want alpha", row[1])
	}

	// One cluster request, to resolve the name. The unfiltered "list every
	// cluster" call must not also happen.
	seen := fake.seen()
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want a cluster lookup and a node pool list", seen)
	}
	if !strings.Contains(seen[0], "/clusters?name=alpha") {
		t.Errorf("first request = %q, want the cluster resolved by name", seen[0])
	}
	for _, req := range seen {
		if strings.Contains(req, "/clusters") && !strings.Contains(req, "name=") {
			t.Errorf("request %q lists every cluster; the filter already names it", req)
		}
	}
}

func TestGetNodePoolsOutputFormats(t *testing.T) {
	tests := []struct {
		name   string
		format string
		decode func([]byte, any) error
	}{
		{name: "json", format: "json", decode: json.Unmarshal},
		{name: "yaml", format: "yaml", decode: unmarshalYAML},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, clientFn := newFakeCWE(t,
				seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})

			out, err := run(t, NewGetCmd(clientFn), "-o", tt.format)
			if err != nil {
				t.Fatalf("get nodepools -o %s: %v", tt.format, err)
			}

			var pools []api.NodePool
			if err := tt.decode([]byte(out), &pools); err != nil {
				t.Fatalf("decoding %s output: %v\n%s", tt.format, err, out)
			}
			if len(pools) != 1 {
				t.Fatalf("pools = %d, want 1:\n%s", len(pools), out)
			}
			stored := fake.stored()[0]
			if pools[0].ID != stored.ID || pools[0].Name != stored.Name {
				t.Errorf("pool = %+v, want the stored pool %+v", pools[0], stored)
			}
			// The structured formats print the API object whole, including
			// fields the table leaves out.
			if pools[0].ClusterID != stored.ClusterID || pools[0].NodeNamePrefix != stored.NodeNamePrefix {
				t.Errorf("pool = %+v, want every field of %+v", pools[0], stored)
			}
			// Nothing is resolved for a structured format, so no cluster list.
			if seen := fake.seen(); len(seen) != 1 {
				t.Errorf("requests = %v, want the node pool list alone", seen)
			}
		})
	}
}

func TestGetNodePoolsRejectsAnUnknownFormat(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})

	_, err := run(t, NewGetCmd(clientFn), "-o", "toml")
	if err == nil {
		t.Fatal("get nodepools -o toml succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "toml") {
		t.Errorf("error = %v, want it to name the rejected format", err)
	}
	// The format is checked before anything is fetched.
	if seen := fake.seen(); len(seen) != 0 {
		t.Errorf("requests = %v, want none", seen)
	}
}

// TestGetNodePoolsRejectsNamesThatMatchNothing keeps a mistyped name an error,
// as it is for "get clusters", rather than an empty table.
func TestGetNodePoolsRejectsNamesThatMatchNothing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "cluster does not exist",
			args:    []string{"--cluster", "nonexistent"},
			wantErr: `cluster "nonexistent" not found`,
		},
		{
			name:    "several names, none of which exist",
			args:    []string{"one", "two"},
			wantErr: `node pools "one", "two" not found`,
		},
		{
			name:    "node pool does not exist",
			args:    []string{"nonexistent"},
			wantErr: `node pool "nonexistent" not found`,
		},
		{
			name:    "node pool does not exist in that cluster",
			args:    []string{"nonexistent", "--cluster", "alpha"},
			wantErr: `node pool "nonexistent" not found in cluster "alpha"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})

			out, err := run(t, NewGetCmd(clientFn), tt.args...)
			if err == nil {
				t.Fatalf("want an error, got output:\n%s", out)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestGetNodePoolsAllowsAnEmptyListing is the counterpart: unfiltered, no
// pools is a legitimate answer.
func TestGetNodePoolsAllowsAnEmptyListing(t *testing.T) {
	_, clientFn := newFakeCWE(t)

	out, err := run(t, NewGetCmd(clientFn))
	if err != nil {
		t.Fatalf("get nodepools: %v", err)
	}
	if got := rows(out); len(got) != 1 || !strings.HasPrefix(got[0], "NAME") {
		t.Errorf("output = %q, want the header alone", out)
	}
}

func TestGetNodePoolsReportsAnAPIError(t *testing.T) {
	clientFn := newBrokenCWE(t, http.StatusInternalServerError, `{"message":"boom"}`)

	_, err := run(t, NewGetCmd(clientFn))
	if err == nil {
		t.Fatal("get nodepools succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to carry the message CWE sent", err)
	}
}

// TestGetNodePoolsReportsAClusterLookupError checks the second call's failure
// is reported rather than swallowed into an ID-only column.
func TestGetNodePoolsReportsAClusterLookupError(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})
	fake.failClustersWith(http.StatusInternalServerError, `{"message":"clusters unavailable"}`)

	_, err := run(t, NewGetCmd(clientFn))
	if err == nil {
		t.Fatal("get nodepools succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "clusters unavailable") {
		t.Errorf("error = %v, want it to carry the message CWE sent", err)
	}
}

// TestGetNodePoolsFallsBackToTheClusterID covers a pool whose cluster the
// cluster list does not carry, which is what a cluster deleted between the two
// calls looks like.
func TestGetNodePoolsFallsBackToTheClusterID(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})
	clusterID := fake.stored()[0].ClusterID
	delete(fake.clusterNames, clusterID)

	out, err := run(t, NewGetCmd(clientFn))
	if err != nil {
		t.Fatalf("get nodepools: %v", err)
	}
	if row := fields(rows(out)[1]); row[1] != clusterID.String() {
		t.Errorf("cluster column = %q, want the cluster ID %s", row[1], clusterID)
	}
}

func TestGetNodePoolsStatusColumn(t *testing.T) {
	trueCondition := api.Condition{Status: api.ConditionStatusTrue}
	falseCondition := api.Condition{Status: api.ConditionStatusFalse}

	tests := []struct {
		name       string
		conditions api.NodePoolConditions
		want       string
	}{
		{
			name:       "provisioned",
			conditions: api.NodePoolConditions{Provisioned: trueCondition},
			want:       "Provisioned",
		},
		{
			name:       "provisioning",
			conditions: api.NodePoolConditions{Provisioning: trueCondition},
			want:       "Provisioning",
		},
		{
			name:       "deleting",
			conditions: api.NodePoolConditions{Deleting: trueCondition},
			want:       "Deleting",
		},
		{
			// Deletion outranks whatever the pool was doing before it started.
			name: "deleting outranks provisioned",
			conditions: api.NodePoolConditions{
				Deleting: trueCondition, Provisioned: trueCondition,
			},
			want: "Deleting",
		},
		{
			name: "provisioning outranks provisioned",
			conditions: api.NodePoolConditions{
				Provisioning: trueCondition, Provisioned: trueCondition,
			},
			want: "Provisioning",
		},
		{
			name: "no condition true",
			conditions: api.NodePoolConditions{
				Deleting: falseCondition, Provisioning: falseCondition, Provisioned: falseCondition,
			},
			want: "Unknown",
		},
		{
			name:       "no condition set",
			conditions: api.NodePoolConditions{},
			want:       "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions := tt.conditions
			_, clientFn := newFakeCWE(t, seed{
				clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8,
				conditions: &conditions,
			})

			out, err := run(t, NewGetCmd(clientFn))
			if err != nil {
				t.Fatalf("get nodepools: %v", err)
			}
			// STATUS is the second-to-last column, ahead of ID.
			row := fields(rows(out)[1])
			if got := row[len(row)-2]; got != tt.want {
				t.Errorf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

// unmarshalYAML decodes YAML into v through JSON, so that the json: struct
// tags the generated API types carry are the ones used for field names.
func unmarshalYAML(data []byte, v any) error {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return err
	}
	b, err := json.Marshal(generic)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// equal reports whether two string slices hold the same values in order.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGetNodePoolsAcceptsSeveralNames(t *testing.T) {
	seeds := []seed{
		{clusterName: "alpha", name: "control-plane", desiredNodeCount: 3, controlPlane: true},
		{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8},
		{clusterName: "alpha", name: "cpu-workers", desiredNodeCount: 1},
	}
	fake, clientFn := newFakeCWE(t, seeds...)

	out, err := run(t, NewGetCmd(clientFn), "gpu-workers", "control-plane", "--cluster", "alpha")
	if err != nil {
		t.Fatalf("get nodepools: %v", err)
	}

	got := rows(out)
	if len(got) != 3 {
		t.Fatalf("rows = %d, want a header and two pools:\n%s", len(got), out)
	}
	// Argument order, not the order CWE returns them in.
	for i, want := range []string{"gpu-workers", "control-plane"} {
		if name := fields(got[i+1])[0]; name != want {
			t.Errorf("row %d = %q, want %q", i, name, want)
		}
	}

	var lists int
	for _, req := range fake.seen() {
		if strings.Contains(req, "/nodePools") {
			lists++
			if !strings.Contains(req, "name=") {
				t.Errorf("request %q fetches every pool; each name is filtered by CWE", req)
			}
		}
	}
	if lists != 2 {
		t.Errorf("node pool requests = %d, want one per name", lists)
	}
}

// TestGetNodePoolsPrintsWhatItFound mirrors kubectl: the pools that matched are
// printed, and the names that did not are reported as an error.
func TestGetNodePoolsPrintsWhatItFound(t *testing.T) {
	_, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 8})

	out, err := run(t, NewGetCmd(clientFn), "gpu-workers", "nope")
	if err == nil {
		t.Fatal("want an error for the missing pool")
	}
	if !strings.Contains(err.Error(), `node pool "nope" not found`) {
		t.Errorf("error = %q, want it to name the missing pool", err)
	}
	// run() captures stdout and stderr in one buffer, so read the table back
	// from the lines before the error.
	table, _, _ := strings.Cut(out, "Error:")
	if got := rows(table); len(got) != 2 || fields(got[1])[0] != "gpu-workers" {
		t.Errorf("output = %q, want the pool that was found to still be printed", out)
	}
}
