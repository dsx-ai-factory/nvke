// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Report is one node's kube-bench --json output, reduced to what the scan
// reads.
type Report struct {
	Controls []Control `json:"Controls"`
}

// Control is one kube-bench target's results, such as master or node.
type Control struct {
	ID       string  `json:"id"`
	Version  string  `json:"version"`
	NodeType string  `json:"node_type"`
	Tests    []Group `json:"tests"`
}

// Group is one section of a control, such as 1.1.
type Group struct {
	Section string   `json:"section"`
	Desc    string   `json:"desc"`
	Results []Result `json:"results"`
}

// Result is kube-bench's verdict on one check.
type Result struct {
	TestNumber     string `json:"test_number"`
	TestDesc       string `json:"test_desc"`
	Audit          string `json:"audit"`
	ActualValue    string `json:"actual_value"`
	ExpectedResult string `json:"expected_result"`
	Reason         string `json:"reason"`
	Type           string `json:"type"`
	Status         Status `json:"status"`
	Scored         bool   `json:"scored"`
}

const (
	// reportPrefix starts the line kube-bench --json prints its report on.
	// kube-bench logs to the same stream, so the report is found by it.
	reportPrefix = `{"Controls":`

	// logTailLines is how many of a log's last lines an error quotes.
	logTailLines = 20

	// maxReportLine bounds the report line. A full control-plane report is a
	// few hundred kilobytes.
	maxReportLine = 16 << 20
)

// ParseLog finds the one report in a kube-bench pod's log and decodes it.
func ParseLog(log []byte) (Report, error) {
	var reports [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 0, 64<<10), maxReportLine)
	for scanner.Scan() {
		if line := scanner.Bytes(); bytes.HasPrefix(line, []byte(reportPrefix)) {
			reports = append(reports, bytes.Clone(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, fmt.Errorf("reading the kube-bench log: %w", err)
	}

	switch len(reports) {
	case 0:
		return Report{}, fmt.Errorf("no kube-bench report in the log; it ends:\n%s", logTail(log))
	case 1:
	default:
		return Report{}, fmt.Errorf("found %d kube-bench reports in the log, want 1", len(reports))
	}

	var report Report
	if err := json.Unmarshal(reports[0], &report); err != nil {
		return Report{}, fmt.Errorf("decoding the kube-bench report: %w", err)
	}
	if len(report.Controls) == 0 {
		return Report{}, errors.New("the kube-bench report has no controls")
	}
	return report, nil
}

// logTail is the last logTailLines lines of a log.
func logTail(log []byte) string {
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	return strings.Join(lines, "\n")
}
