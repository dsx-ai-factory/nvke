// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTraceIDsForWorkflow(t *testing.T) {
	const (
		workflowID = "provision-node-11111111-1111-1111-1111-111111111111"
		traceID    = "22222222222222222222222222222222"
	)
	logs := []byte(`not JSON
{"workflow_id":"other","trace_id":"33333333333333333333333333333333"}
{"workflow_id":"provision-node-11111111-1111-1111-1111-111111111111","trace_id":"invalid","event":"workflow.started"}
{"workflow_id":"provision-node-11111111-1111-1111-1111-111111111111","trace_id":"33333333333333333333333333333333","event":"workflow.returned"}
{"workflow_id":"provision-node-11111111-1111-1111-1111-111111111111","trace_id":"22222222222222222222222222222222","event":"workflow.started"}
`)

	assert.Equal(t, []string{traceID}, traceIDsForWorkflow(logs, workflowID))
}

func TestValidTraceID(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "valid", value: "0123456789abcdef0123456789abcdef", want: true},
		{name: "wrong length", value: "0123456789abcdef", want: false},
		{name: "not hex", value: "gggggggggggggggggggggggggggggggg", want: false},
		{name: "zero", value: "00000000000000000000000000000000", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, validTraceID(test.value))
		})
	}
}
