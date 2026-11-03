// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestTenantKubeconfigRejectedExposesEverySourceBeforeFirstRecord(t *testing.T) {
	require.Equal(t, 2, testutil.CollectAndCount(tenantKubeconfigRejected))
}

func TestRecordTenantKubeconfigRejected(t *testing.T) {
	before := testutil.ToFloat64(tenantKubeconfigRejected.WithLabelValues(TenantKubeconfigSourceSecret))
	RecordTenantKubeconfigRejected(TenantKubeconfigSourceSecret)
	require.InDelta(t, before+1, testutil.ToFloat64(tenantKubeconfigRejected.WithLabelValues(TenantKubeconfigSourceSecret)), 0)

	RecordTenantKubeconfigRejected("unexpected")
	require.Equal(t, 2, testutil.CollectAndCount(tenantKubeconfigRejected))
}
