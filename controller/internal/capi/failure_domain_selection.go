// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"slices"
)

// FailureDomainSelectionAlgorithm selects a failure domain from the eligible
// domains using existing and reserved assignments.
type FailureDomainSelectionAlgorithm func(eligibleDomains, assignments []string) (string, error)

// LeastUsedFailureDomainSelection returns the eligible domain with the fewest
// existing assignments, breaking ties by domain name, or an empty string when
// no eligible domains are available.
func LeastUsedFailureDomainSelection(eligibleDomains, assignments []string) (string, error) {
	if len(eligibleDomains) == 0 {
		return "", nil
	}

	counts := make(map[string]int, len(eligibleDomains))
	for _, domain := range eligibleDomains {
		counts[domain] = 0
	}

	for _, assignment := range assignments {
		if _, eligible := counts[assignment]; eligible {
			counts[assignment]++
		}
	}

	domains := slices.Clone(eligibleDomains)
	slices.Sort(domains)

	selected := domains[0]
	for _, domain := range domains[1:] {
		if counts[domain] < counts[selected] {
			selected = domain
		}
	}
	return selected, nil
}
