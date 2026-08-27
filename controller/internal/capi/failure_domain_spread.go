// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"slices"
)

// FailureDomainSpreadAlgorithm selects a failure domain from the eligible
// domains using the assignments of existing Machines.
type FailureDomainSpreadAlgorithm interface {
	Select(eligibleDomains, assignments []string) (string, error)
}

// LeastUsedFailureDomainSpread selects the eligible domain with the fewest
// existing assignments and breaks ties by domain name.
type LeastUsedFailureDomainSpread struct{}

// Select returns the least-used eligible domain, or an empty string when no
// eligible domains are available.
func (LeastUsedFailureDomainSpread) Select(eligibleDomains, assignments []string) (string, error) {
	counts := make(map[string]int, len(eligibleDomains))
	for _, domain := range eligibleDomains {
		if domain != "" {
			counts[domain] = 0
		}
	}
	if len(counts) == 0 {
		return "", nil
	}

	for _, assignment := range assignments {
		if _, eligible := counts[assignment]; eligible {
			counts[assignment]++
		}
	}

	domains := make([]string, 0, len(counts))
	for domain := range counts {
		domains = append(domains, domain)
	}
	slices.Sort(domains)

	selected := domains[0]
	for _, domain := range domains[1:] {
		if counts[domain] < counts[selected] {
			selected = domain
		}
	}
	return selected, nil
}
