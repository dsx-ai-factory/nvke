// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestLeastUsedFailureDomainSelection(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		counts map[string]int
		want   string
	}{
		"single domain is always the selection": {
			counts: map[string]int{"fd-a": 7},
			want:   "fd-a",
		},
		"unused domain sorts before other unused domains": {
			counts: map[string]int{"fd-c": 0, "fd-a": 0, "fd-b": 0},
			want:   "fd-a",
		},
		"unused domain wins over used domains": {
			counts: map[string]int{"fd-a": 1, "fd-b": 1, "fd-c": 0},
			want:   "fd-c",
		},
		"least-used domain wins when every domain is used": {
			counts: map[string]int{"fd-a": 2, "fd-b": 1},
			want:   "fd-b",
		},
		"lexical order breaks equal-count ties": {
			counts: map[string]int{"fd-b": 1, "fd-a": 1},
			want:   "fd-a",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, leastUsedFailureDomainSelection(tc.counts))
		})
	}
}

func TestDomainAllocatorTake(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		eligible []string
		recorded []string
		want     string
	}{
		"no eligible domains leaves placement empty": {
			want: "",
		},
		"recording into an allocator with no domains changes nothing": {
			recorded: []string{"fd-a"},
			want:     "",
		},
		"unnamed eligible domains are dropped": {
			eligible: []string{""},
			want:     "",
		},
		"recorded placements steer the next one": {
			eligible: []string{"fd-a", "fd-b"},
			recorded: []string{"fd-a"},
			want:     "fd-b",
		},
		"recorded domains outside the eligible set are ignored": {
			eligible: []string{"fd-a", "fd-b"},
			recorded: []string{"retired-domain", "fd-a"},
			want:     "fd-b",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			allocator := newDomainAllocator(leastUsedFailureDomainSelection, tc.eligible)
			for _, domain := range tc.recorded {
				allocator.record(domain)
			}
			assert.Equal(t, tc.want, allocator.take())
		})
	}
}

func TestDomainAllocatorTakeCountsItsOwnHandouts(t *testing.T) {
	t.Parallel()

	var seen []map[string]int
	stub := func(counts map[string]int) string {
		seen = append(seen, maps.Clone(counts))
		return "fd-a"
	}
	allocator := newDomainAllocator(stub, []string{"fd-a", "fd-b"})

	assert.Equal(t, "fd-a", allocator.take())
	assert.Equal(t, "fd-a", allocator.take())

	require.Len(t, seen, 2)
	assert.Equal(t, map[string]int{"fd-a": 0, "fd-b": 0}, seen[0])
	assert.Equal(t, map[string]int{"fd-a": 1, "fd-b": 0}, seen[1])
}

func TestDomainAllocatorSpreadsSuccessiveTakes(t *testing.T) {
	t.Parallel()
	allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-b", "fd-a", "fd-c"})
	got := []string{allocator.take(), allocator.take(), allocator.take(), allocator.take()}
	assert.Equal(t, []string{"fd-a", "fd-b", "fd-c", "fd-a"}, got)
}

func TestDomainAllocatorTakeWithoutEligibleDomainsIsRepeatable(t *testing.T) {
	t.Parallel()
	allocator := newDomainAllocator(nil, nil)
	assert.Empty(t, allocator.take())
	assert.Empty(t, allocator.take())
}

func TestZeroValueDomainAllocatorRequestsNoSpecificDomain(t *testing.T) {
	t.Parallel()
	var allocator domainAllocator
	assert.Empty(t, allocator.take())
	assert.Empty(t, allocator.take())
}

func TestDomainAllocatorCollapsesDuplicateEligibleDomains(t *testing.T) {
	t.Parallel()
	allocator := newDomainAllocator(leastUsedFailureDomainSelection, []string{"fd-a", "fd-a", "fd-b"})
	got := []string{allocator.take(), allocator.take(), allocator.take(), allocator.take()}
	assert.Equal(t, []string{"fd-a", "fd-b", "fd-a", "fd-b"}, got)
}

func TestNewControlPlaneDomainAllocator(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		nodes    []db.Node
		observed map[string]string
		eligible []string
		counts   map[string]int
		wantNext string
	}{
		"CAPI placement is counted for an existing node without a DB decision": {
			nodes:    []db.Node{{Name: "cp-old"}},
			observed: map[string]string{"cp-old": "fd-a"},
			eligible: []string{"fd-a", "fd-b"},
			counts:   map[string]int{"fd-a": 1, "fd-b": 0},
			wantNext: "fd-b",
		},
		"CAPI observation overrides the stored request and is counted once": {
			nodes:    []db.Node{{Name: "cp-0", FailureDomain: "fd-a"}},
			observed: map[string]string{"cp-0": "fd-b"},
			eligible: []string{"fd-a", "fd-b"},
			counts:   map[string]int{"fd-a": 0, "fd-b": 1},
			wantNext: "fd-a",
		},
		"stored decision counts before a new Machine exists": {
			nodes:    []db.Node{{Name: "cp-new", FailureDomain: "fd-a"}},
			eligible: []string{"fd-a", "fd-b"},
			counts:   map[string]int{"fd-a": 1, "fd-b": 0},
			wantNext: "fd-b",
		},
		"unobserved legacy node is omitted without blocking placement": {
			nodes:    []db.Node{{Name: "cp-old"}, {Name: "cp-placed", FailureDomain: "fd-a"}},
			eligible: []string{"fd-a", "fd-b"},
			counts:   map[string]int{"fd-a": 1, "fd-b": 0},
			wantNext: "fd-b",
		},
		"node marked for deletion is not counted": {
			nodes: []db.Node{{Name: "cp-deleting", FailureDomain: "fd-a", Conditions: db.NodeConditions{
				Deleting: db.NewCondition(true, "deleting"),
			}}},
			observed: map[string]string{"cp-deleting": "fd-a"},
			eligible: []string{"fd-a", "fd-b"},
			counts:   map[string]int{"fd-a": 0, "fd-b": 0},
			wantNext: "fd-a",
		},
		"ineligible observed domain is ignored": {
			nodes:    []db.Node{{Name: "cp-0"}},
			observed: map[string]string{"cp-0": "retired"},
			eligible: []string{"fd-a", "fd-b"},
			counts:   map[string]int{"fd-a": 0, "fd-b": 0},
			wantNext: "fd-a",
		},
		"provider without failure domains selects empty string": {
			nodes:    []db.Node{{Name: "cp-old"}},
			counts:   map[string]int{},
			wantNext: "",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			allocator := newControlPlaneDomainAllocator(tc.nodes, &failureDomains{
				eligible:           tc.eligible,
				observedByNodeName: tc.observed,
			})
			assert.Equal(t, tc.counts, allocator.machinesPerDomain)
			assert.Equal(t, tc.wantNext, allocator.take())
		})
	}
}

func TestNewControlPlaneDomainAllocatorDoesNotBackfillExistingNode(t *testing.T) {
	t.Parallel()
	nodes := []db.Node{{Name: "cp-old"}}
	allocator := newControlPlaneDomainAllocator(nodes, &failureDomains{
		eligible:           []string{"fd-a", "fd-b"},
		observedByNodeName: map[string]string{"cp-old": "fd-a"},
	})
	assert.Empty(t, nodes[0].FailureDomain)
	assert.Equal(t, "fd-b", allocator.take())
}
