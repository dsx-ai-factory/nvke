// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"fmt"
	"strings"
	"time"
)

// NodeReport is one node's kube-bench report.
type NodeReport struct {
	Node   string `json:"node"`
	Role   Role   `json:"role"`
	Report Report `json:"report"`
}

// Finding is a FAIL or WARN on one node, and the entry that excepts it, if
// any.
type Finding struct {
	Node      string     `json:"node"`
	Role      Role       `json:"role"`
	Result    Result     `json:"result"`
	Exception *Exception `json:"exception,omitempty"`
}

// StaleEntry is a register entry the scan no longer needs, and why.
type StaleEntry struct {
	Exception Exception `json:"exception"`
	Why       string    `json:"why"`
}

// Outcome is a scan judged against the register.
type Outcome struct {
	Cluster          Cluster      `json:"cluster"`
	Violations       []Finding    `json:"violations"`
	Excepted         []Finding    `json:"excepted"`
	Stale            []StaleEntry `json:"stale"`
	AwaitingApproval []Exception  `json:"awaitingApproval"`
	DueForReview     []Exception  `json:"dueForReview"`
}

const (
	staleNotReported = "kube-bench reported no check with this ID"
	stalePasses      = "passes on every node it covers"
)

// Evaluate judges the nodes' reports against the register:
//   - a FAIL or WARN is excepted when an entry has its ID and covers the
//     node's cluster and role, and is a violation otherwise;
//   - an entry that covers some node of the cluster is stale when none of the
//     nodes it covers reported its ID, or when they did and none reported FAIL
//     or WARN; an entry that covers no node is not judged;
//   - an entry that excepted something and has no approver is awaiting
//     approval, and an entry that covers some node is due for review once
//     today is after its reviewBy date.
func Evaluate(cluster Cluster, nodes []NodeReport, reg Register, today time.Time) Outcome {
	out := Outcome{Cluster: cluster}
	for _, node := range nodes {
		for _, r := range results(node.Report) {
			if r.Status != StatusFail && r.Status != StatusWarn {
				continue
			}
			finding := Finding{Node: node.Node, Role: node.Role, Result: r}
			if e, ok := exceptionFor(reg, r.TestNumber, cluster, node.Role); ok {
				finding.Exception = &e
				out.Excepted = append(out.Excepted, finding)
				continue
			}
			out.Violations = append(out.Violations, finding)
		}
	}

	todayDate := today.UTC().Format(reviewByLayout)
	for _, e := range reg.Exceptions {
		judged, reported, used := false, false, false
		for _, node := range nodes {
			if !e.Scope.Covers(cluster, node.Role) {
				continue
			}
			judged = true
			for _, r := range results(node.Report) {
				if r.TestNumber != e.ID {
					continue
				}
				reported = true
				used = used || r.Status == StatusFail || r.Status == StatusWarn
			}
		}
		if !judged {
			continue
		}
		switch {
		case !reported:
			out.Stale = append(out.Stale, StaleEntry{Exception: e, Why: staleNotReported})
		case !used:
			out.Stale = append(out.Stale, StaleEntry{Exception: e, Why: stalePasses})
		case e.Approver == "":
			out.AwaitingApproval = append(out.AwaitingApproval, e)
		}
		if todayDate > e.ReviewBy {
			out.DueForReview = append(out.DueForReview, e)
		}
	}
	return out
}

// results flattens a report's results.
func results(report Report) []Result {
	var all []Result
	for _, control := range report.Controls {
		for _, group := range control.Tests {
			all = append(all, group.Results...)
		}
	}
	return all
}

// exceptionFor is the entry that excepts check id on a node of role in a
// cluster of kind cluster. Validate rules out two such entries.
func exceptionFor(reg Register, id string, cluster Cluster, role Role) (Exception, bool) {
	for _, e := range reg.Exceptions {
		if e.ID == id && e.Scope.Covers(cluster, role) {
			return e, true
		}
	}
	return Exception{}, false
}

// Passed reports whether the scan has no violations and no stale entries.
// Entries awaiting approval or due for review do not fail it.
func (o Outcome) Passed() bool {
	return len(o.Violations) == 0 && len(o.Stale) == 0
}

// Summary is the verdict, then each violation with kube-bench's evidence, then
// the stale entries, the entries awaiting approval and those due for review.
func (o Outcome) Summary() string {
	var b strings.Builder
	verdict := "passed"
	if !o.Passed() {
		verdict = fmt.Sprintf("failed: %s, %s", count(len(o.Violations), "violation", "violations"), count(len(o.Stale), "stale entry", "stale entries"))
	}
	fmt.Fprintf(&b, "CIS benchmark scan of the %s cluster (kube-bench %s): %s\n", o.Cluster, Benchmark, verdict)
	fmt.Fprintf(&b, "%s excepted by the register.\n", count(len(o.Excepted), "finding", "findings"))

	if len(o.Violations) > 0 {
		b.WriteString("\nViolations: add a register entry with a reason, or fix the cluster.\n")
		for _, f := range o.Violations {
			fmt.Fprintf(&b, "  %s %s on %s (%s): %s\n", f.Result.TestNumber, f.Result.Status, f.Node, f.Role, f.Result.TestDesc)
			if f.Result.ActualValue != "" {
				fmt.Fprintf(&b, "      actual: %s\n", f.Result.ActualValue)
			}
			if f.Result.Reason != "" {
				fmt.Fprintf(&b, "      kube-bench: %s\n", f.Result.Reason)
			}
		}
	}
	if len(o.Stale) > 0 {
		b.WriteString("\nStale register entries: remove them.\n")
		for _, s := range o.Stale {
			fmt.Fprintf(&b, "  %s (%s): %s\n", s.Exception.ID, s.Exception.Title, s.Why)
		}
	}
	listEntries(&b, "Awaiting approval", o.AwaitingApproval)
	listEntries(&b, "Due for review", o.DueForReview)
	return b.String()
}

func listEntries(b *strings.Builder, heading string, entries []Exception) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", heading)
	for _, e := range entries {
		fmt.Fprintf(b, "  %s (%s), review by %s\n", e.ID, e.Title, e.ReviewBy)
	}
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
