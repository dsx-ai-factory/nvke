// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

const credentialAuditWritesName = "nvke_controller_credential_audit_writes_total"

func credentialAuditResultLabels(t *testing.T) []string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var results []string
	for _, family := range families {
		if family.GetName() != credentialAuditWritesName {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" {
					results = append(results, label.GetValue())
				}
			}
		}
	}
	return results
}

func TestCredentialAuditWritesExposeEveryResultBeforeFirstRecord(t *testing.T) {
	require.ElementsMatch(t, []string{"stored", "invalid_event", "sink_failed"}, credentialAuditResultLabels(t))
}

func TestRecordCredentialAuditWriteIgnoresUnknownResult(t *testing.T) {
	RecordCredentialAuditWrite("unexpected")
	require.ElementsMatch(t, []string{"stored", "invalid_event", "sink_failed"}, credentialAuditResultLabels(t))
}
