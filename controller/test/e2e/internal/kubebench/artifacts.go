// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// resultsFile is what results.json holds: everything the scan saw and decided.
type resultsFile struct {
	Cluster         Cluster      `json:"cluster"`
	Benchmark       string       `json:"benchmark"`
	BenchmarkCommit string       `json:"benchmarkCommit"`
	Image           string       `json:"image"`
	Nodes           []NodeReport `json:"nodes"`
	Outcome         Outcome      `json:"outcome"`
}

// WriteArtifacts writes results.json, with every node's report and the
// evaluation, and summary.txt, the evaluation's summary, into dir.
func WriteArtifacts(dir string, nodes []NodeReport, out Outcome) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(resultsFile{
		Cluster:         out.Cluster,
		Benchmark:       Benchmark,
		BenchmarkCommit: BenchmarkCommit,
		Image:           Image,
		Nodes:           nodes,
		Outcome:         out,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the kube-bench results: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing results.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(out.Summary()), 0o600); err != nil {
		return fmt.Errorf("writing summary.txt: %w", err)
	}
	return nil
}
