// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func result(id string, status Status) Result {
	return Result{TestNumber: id, TestDesc: "synthetic " + id, Status: status, ActualValue: "value of " + id}
}

func nodeReport(node string, role Role, results ...Result) NodeReport {
	return NodeReport{Node: node, Role: role, Report: Report{Controls: []Control{{
		ID: "1", Version: Benchmark, Tests: []Group{{Section: "1.1", Results: results}},
	}}}}
}

func entry(id string) Exception {
	return Exception{ID: id, Title: "title " + id, Reason: "reason"}
}

func ids[T any](items []T, id func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, id(item))
	}
	return out
}

func findingIDs(findings []Finding) []string {
	return ids(findings, func(f Finding) string { return f.Result.TestNumber + "@" + f.Node })
}

func entryIDs(entries []Exception) []string {
	return ids(entries, func(e Exception) string { return e.ID })
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		cluster        Cluster
		nodes          []NodeReport
		entries        []Exception
		wantPassed     bool
		wantViolations []string
		wantExcepted   []string
		wantStale      []string
		wantUnneeded   []string
	}{
		{
			name:           "an unlisted FAIL is a violation",
			cluster:        ClusterTenant,
			nodes:          []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.12", StatusFail))},
			wantViolations: []string{"1.1.12@cp-a"},
		},
		{
			name:           "an unlisted WARN is a violation",
			cluster:        ClusterTenant,
			nodes:          []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn))},
			wantViolations: []string{"1.2.11@cp-a"},
		},
		{
			name:         "a listed WARN is excepted",
			cluster:      ClusterTenant,
			nodes:        []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn))},
			entries:      []Exception{entry("1.2.11")},
			wantPassed:   true,
			wantExcepted: []string{"1.2.11@cp-a"},
		},
		{
			name:       "PASS and INFO need no entry",
			cluster:    ClusterTenant,
			nodes:      []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.11", StatusPass), result("1.1.21", StatusInfo))},
			wantPassed: true,
		},
		{
			name:    "an entry excepts its check on every node",
			cluster: ClusterTenant,
			nodes: []NodeReport{
				nodeReport("cp-a", RoleControlPlane, result("4.2.9", StatusWarn)),
				nodeReport("w-1", RoleWorker, result("4.2.9", StatusWarn)),
			},
			entries:      []Exception{entry("4.2.9")},
			wantPassed:   true,
			wantExcepted: []string{"4.2.9@cp-a", "4.2.9@w-1"},
		},
		{
			name:      "an entry for a check kube-bench did not report is stale",
			cluster:   ClusterTenant,
			nodes:     []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.11", StatusPass))},
			entries:   []Exception{entry("9.9.9")},
			wantStale: []string{"9.9.9"},
		},
		{
			name:         "an entry whose check passes on every node is listed and passes",
			cluster:      ClusterSite,
			nodes:        []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusPass))},
			entries:      []Exception{entry("1.2.11")},
			wantPassed:   true,
			wantUnneeded: []string{"1.2.11"},
		},
		{
			name:    "a WARN on one node and PASS on another keeps the entry in use",
			cluster: ClusterTenant,
			nodes: []NodeReport{
				nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn)),
				nodeReport("cp-b", RoleControlPlane, result("1.2.11", StatusPass)),
			},
			entries:      []Exception{entry("1.2.11")},
			wantPassed:   true,
			wantExcepted: []string{"1.2.11@cp-a"},
		},
		{
			name:    "a WARN on two nodes without an entry is two violations",
			cluster: ClusterTenant,
			nodes: []NodeReport{
				nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn)),
				nodeReport("cp-b", RoleControlPlane, result("1.2.11", StatusWarn)),
			},
			wantViolations: []string{"1.2.11@cp-a", "1.2.11@cp-b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := Evaluate(tc.cluster, tc.nodes, Register{Exceptions: tc.entries})

			assert.Equal(t, tc.cluster, out.Cluster)
			assert.Equal(t, tc.wantPassed, out.Passed(), out.Summary())
			assert.ElementsMatch(t, tc.wantViolations, findingIDs(out.Violations), "violations")
			assert.ElementsMatch(t, tc.wantExcepted, findingIDs(out.Excepted), "excepted")
			assert.ElementsMatch(t, tc.wantStale, entryIDs(out.Stale), "stale")
			assert.ElementsMatch(t, tc.wantUnneeded, entryIDs(out.Unneeded), "unneeded")
			for _, f := range out.Excepted {
				assert.NotNil(t, f.Exception, "an excepted finding names its entry")
			}
		})
	}
}
