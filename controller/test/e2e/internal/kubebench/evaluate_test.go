// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"testing"
	"time"

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

func entry(id string, scope Scope) Exception {
	return Exception{ID: id, Title: "title " + id, Scope: scope, Reason: "reason", Approver: "a.person", ReviewBy: "2027-05-04"}
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

func staleIDs(entries []StaleEntry) []string {
	return ids(entries, func(e StaleEntry) string { return e.Exception.ID })
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	today := time.Date(2026, time.November, 5, 15, 0, 0, 0, time.UTC)
	tenant := Scope{Clusters: []Cluster{ClusterTenant}}
	workers := Scope{Roles: []Role{RoleWorker}}
	unapproved := entry("1.2.11", Scope{})
	unapproved.Approver = ""
	pastReview := entry("1.2.11", Scope{})
	pastReview.ReviewBy = "2026-11-04"
	reviewedToday := entry("1.2.11", Scope{})
	reviewedToday.ReviewBy = "2026-11-05"

	tests := []struct {
		name             string
		cluster          Cluster
		nodes            []NodeReport
		entries          []Exception
		wantPassed       bool
		wantViolations   []string
		wantExcepted     []string
		wantStale        []string
		wantAwaiting     []string
		wantDueForReview []string
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
			entries:      []Exception{entry("1.2.11", Scope{})},
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
			name:           "an entry scoped to tenant does not except a site result",
			cluster:        ClusterSite,
			nodes:          []NodeReport{nodeReport("cp-a", RoleControlPlane, result("5.1.1", StatusWarn))},
			entries:        []Exception{entry("5.1.1", tenant)},
			wantViolations: []string{"5.1.1@cp-a"},
		},
		{
			name:    "an entry scoped to workers does not except a control-plane result",
			cluster: ClusterTenant,
			nodes: []NodeReport{
				nodeReport("cp-a", RoleControlPlane, result("4.2.9", StatusWarn)),
				nodeReport("w-1", RoleWorker, result("4.2.9", StatusWarn)),
			},
			entries:        []Exception{entry("4.2.9", workers)},
			wantViolations: []string{"4.2.9@cp-a"},
			wantExcepted:   []string{"4.2.9@w-1"},
		},
		{
			name:       "an entry whose check passes on every node it covers is stale",
			cluster:    ClusterTenant,
			nodes:      []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusPass))},
			entries:    []Exception{entry("1.2.11", Scope{})},
			wantPassed: false,
			wantStale:  []string{"1.2.11"},
		},
		{
			name:      "an entry for a check kube-bench did not report is stale",
			cluster:   ClusterTenant,
			nodes:     []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.11", StatusPass))},
			entries:   []Exception{entry("9.9.9", Scope{})},
			wantStale: []string{"9.9.9"},
		},
		{
			name:       "a worker-only entry on a cluster without workers is not judged",
			cluster:    ClusterSite,
			nodes:      []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.11", StatusPass))},
			entries:    []Exception{entry("4.2.9", workers)},
			wantPassed: true,
		},
		{
			name:       "an entry for the other cluster kind is not judged",
			cluster:    ClusterSite,
			nodes:      []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.11", StatusPass))},
			entries:    []Exception{entry("5.1.1", tenant)},
			wantPassed: true,
		},
		{
			name:    "a WARN on one node and PASS on another keeps the entry in use",
			cluster: ClusterTenant,
			nodes: []NodeReport{
				nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn)),
				nodeReport("cp-b", RoleControlPlane, result("1.2.11", StatusPass)),
			},
			entries:      []Exception{entry("1.2.11", Scope{})},
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
		{
			name:         "an entry with no approver is listed and passes",
			cluster:      ClusterTenant,
			nodes:        []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn))},
			entries:      []Exception{unapproved},
			wantPassed:   true,
			wantExcepted: []string{"1.2.11@cp-a"},
			wantAwaiting: []string{"1.2.11"},
		},
		{
			name:             "an entry past reviewBy is listed and passes",
			cluster:          ClusterTenant,
			nodes:            []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn))},
			entries:          []Exception{pastReview},
			wantPassed:       true,
			wantExcepted:     []string{"1.2.11@cp-a"},
			wantDueForReview: []string{"1.2.11"},
		},
		{
			name:         "an entry reviewed today is not due",
			cluster:      ClusterTenant,
			nodes:        []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusWarn))},
			entries:      []Exception{reviewedToday},
			wantPassed:   true,
			wantExcepted: []string{"1.2.11@cp-a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := Evaluate(tc.cluster, tc.nodes, Register{Exceptions: tc.entries}, today)

			assert.Equal(t, tc.cluster, out.Cluster)
			assert.Equal(t, tc.wantPassed, out.Passed(), out.Summary())
			assert.ElementsMatch(t, tc.wantViolations, findingIDs(out.Violations), "violations")
			assert.ElementsMatch(t, tc.wantExcepted, findingIDs(out.Excepted), "excepted")
			assert.ElementsMatch(t, tc.wantStale, staleIDs(out.Stale), "stale")
			assert.ElementsMatch(t, tc.wantAwaiting, entryIDs(out.AwaitingApproval), "awaiting approval")
			assert.ElementsMatch(t, tc.wantDueForReview, entryIDs(out.DueForReview), "due for review")
			for _, f := range out.Excepted {
				assert.NotNil(t, f.Exception, "an excepted finding names its entry")
			}
		})
	}
}

func TestEvaluateExplainsStaleEntries(t *testing.T) {
	t.Parallel()

	out := Evaluate(ClusterTenant,
		[]NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusPass))},
		Register{Exceptions: []Exception{entry("1.2.11", Scope{}), entry("9.9.9", Scope{})}},
		time.Now())

	why := map[string]string{}
	for _, s := range out.Stale {
		why[s.Exception.ID] = s.Why
	}
	assert.Equal(t, "passes on every node it covers", why["1.2.11"])
	assert.Equal(t, "kube-bench reported no check with this ID", why["9.9.9"])
}
