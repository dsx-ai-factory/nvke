// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"fmt"
	"strings"
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

// Outcome is a scan judged against the register.
type Outcome struct {
	Cluster    Cluster     `json:"cluster"`
	Violations []Finding   `json:"violations"`
	Excepted   []Finding   `json:"excepted"`
	Stale      []Exception `json:"stale"`
	Unneeded   []Exception `json:"unneeded"`
}

// Evaluate judges the nodes' reports against the register:
//   - a FAIL or WARN is excepted when an entry has its ID, and is a violation
//     otherwise;
//   - an entry is stale when no node reported its ID;
//   - an entry is unneeded when its check passed on every node. Another
//     cluster may still need it, so it does not fail the scan.
func Evaluate(cluster Cluster, nodes []NodeReport, reg Register) Outcome {
	byID := make(map[string]Exception, len(reg.Exceptions))
	for _, e := range reg.Exceptions {
		byID[e.ID] = e
	}
	out := Outcome{Cluster: cluster}
	reported, used := map[string]bool{}, map[string]bool{}
	for _, node := range nodes {
		for _, r := range results(node.Report) {
			reported[r.TestNumber] = true
			if r.Status != StatusFail && r.Status != StatusWarn {
				continue
			}
			used[r.TestNumber] = true
			finding := Finding{Node: node.Node, Role: node.Role, Result: r}
			if e, ok := byID[r.TestNumber]; ok {
				finding.Exception = &e
				out.Excepted = append(out.Excepted, finding)
				continue
			}
			out.Violations = append(out.Violations, finding)
		}
	}
	for _, e := range reg.Exceptions {
		switch {
		case !reported[e.ID]:
			out.Stale = append(out.Stale, e)
		case !used[e.ID]:
			out.Unneeded = append(out.Unneeded, e)
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

// Passed reports whether the scan has no violations and no stale entries.
// Unneeded entries do not fail it.
func (o Outcome) Passed() bool {
	return len(o.Violations) == 0 && len(o.Stale) == 0
}

// Summary is the verdict, then each violation with kube-bench's evidence, then
// the stale entries and the unneeded ones.
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
	listEntries(&b, "Stale register entries: kube-bench reported no check with these IDs. Correct or remove them.", o.Stale)
	listEntries(&b, "Unneeded on this cluster: these checks passed on every node. Remove an entry once no cluster needs it.", o.Unneeded)
	return b.String()
}

func listEntries(b *strings.Builder, heading string, entries []Exception) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", heading)
	for _, e := range entries {
		fmt.Fprintf(b, "  %s (%s)\n", e.ID, e.Title)
	}
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
