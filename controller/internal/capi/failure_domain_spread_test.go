// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
)

func TestLeastUsedFailureDomainSpreadSelect(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		eligibleDomains []string
		assignments     []string
		want            string
	}{
		"no eligible domains leaves placement empty": {
			want: "",
		},
		"unused domain sorts before other unused domains": {
			eligibleDomains: []string{"fd-c", "fd-a", "fd-b"},
			want:            "fd-a",
		},
		"unused domain wins over used domains": {
			eligibleDomains: []string{"fd-a", "fd-b", "fd-c"},
			assignments:     []string{"fd-a", "fd-b"},
			want:            "fd-c",
		},
		"least-used domain wins when all domains are used": {
			eligibleDomains: []string{"fd-a", "fd-b"},
			assignments:     []string{"fd-a", "fd-a", "fd-b"},
			want:            "fd-b",
		},
		"lexical order breaks equal-count ties": {
			eligibleDomains: []string{"fd-b", "fd-a"},
			assignments:     []string{"fd-a", "fd-b"},
			want:            "fd-a",
		},
		"assignments outside the eligible set are ignored": {
			eligibleDomains: []string{"fd-a", "fd-b"},
			assignments:     []string{"retired-domain", "fd-a"},
			want:            "fd-b",
		},
	}

	algorithm := capi.LeastUsedFailureDomainSpread{}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := algorithm.Select(tc.eligibleDomains, tc.assignments)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
