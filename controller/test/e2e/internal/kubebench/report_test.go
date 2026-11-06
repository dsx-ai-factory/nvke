// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureLine is a testdata report compacted to the one line kube-bench prints.
func fixtureLine(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var line bytes.Buffer
	require.NoError(t, json.Compact(&line, raw))
	return line.String()
}

func findResult(report Report, id string) (Result, bool) {
	for _, control := range report.Controls {
		for _, group := range control.Tests {
			for _, result := range group.Results {
				if result.TestNumber == id {
					return result, true
				}
			}
		}
	}
	return Result{}, false
}

func TestParseLog(t *testing.T) {
	t.Parallel()

	report := fixtureLine(t, "report_control_plane.json")
	glog := "I1105 10:00:00.000000       1 util.go:100] Kubernetes version: 1.36\n" +
		"W1105 10:00:00.000001       1 util.go:200] Using benchmark version cis-2.0\n"

	tests := []struct {
		name    string
		log     string
		wantErr []string
		check   func(t *testing.T, got Report)
	}{
		{
			name: "a report among log lines",
			log:  glog + report + "\nI1105 10:00:01.000000       1 run.go:1] done\n",
			check: func(t *testing.T, got Report) {
				t.Helper()
				require.Len(t, got.Controls, 2)
				assert.Equal(t, "1", got.Controls[0].ID)
				assert.Equal(t, "cis-2.0", got.Controls[0].Version)
				assert.Equal(t, "master", got.Controls[0].NodeType)
				result, ok := findResult(got, "1.1.12")
				require.True(t, ok)
				assert.Equal(t, StatusFail, result.Status)
				assert.True(t, result.Scored)
				assert.Equal(t, "root:root", result.ActualValue)
			},
		},
		{
			name:    "no report",
			log:     glog + "E1105 10:00:02.000000       1 run.go:2] could not read the kubelet config\n",
			wantErr: []string{"no kube-bench report", "could not read the kubelet config"},
		},
		{
			name:    "two reports",
			log:     report + "\n" + report + "\n",
			wantErr: []string{"2 kube-bench reports"},
		},
		{
			name:    "a cut-off report",
			log:     glog + report[:len(report)/2] + "\n",
			wantErr: []string{"kube-bench report"},
		},
		{
			name: "reason null",
			log:  report + "\n",
			check: func(t *testing.T, got Report) {
				t.Helper()
				result, ok := findResult(got, "1.1.12")
				require.True(t, ok)
				assert.Empty(t, result.Reason)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLog([]byte(tc.log))
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, fragment := range tc.wantErr {
					assert.Contains(t, err.Error(), fragment)
				}
				return
			}
			require.NoError(t, err)
			tc.check(t, got)
		})
	}
}

func TestParseLogKeepsOnlyTheLastLinesOfALongLog(t *testing.T) {
	t.Parallel()

	var log strings.Builder
	for i := range 100 {
		log.WriteString("line ")
		log.WriteString(strings.Repeat("x", i%7))
		log.WriteString("\n")
	}
	log.WriteString("the last line\n")

	_, err := ParseLog([]byte(log.String()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the last line")
	assert.LessOrEqual(t, strings.Count(err.Error(), "\n"), logTailLines+1)
}
