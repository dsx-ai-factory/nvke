// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package failuredomains

import (
	"time"
)

const (
	// UnplaceableTTL is how long placement avoids a domain after a control-plane node
	// could not get capacity there.
	UnplaceableTTL = 3 * time.Hour
)

// IsMarkedUnplaceable reports whether domain was marked unplaceable within UnplaceableTTL of now.
func IsMarkedUnplaceable(marked map[string]time.Time, domain string, now time.Time) bool {
	at, ok := marked[domain]
	return ok && now.Sub(at) < UnplaceableTTL
}

// Placeable returns eligible without the domains marked unplaceable. When that
// would leave none, it returns eligible so placement still proceeds.
func Placeable(eligible []string, marked map[string]time.Time, now time.Time) []string {
	placeable := make([]string, 0, len(eligible))
	for _, domain := range eligible {
		if !IsMarkedUnplaceable(marked, domain, now) {
			placeable = append(placeable, domain)
		}
	}
	if len(placeable) == 0 {
		return eligible
	}
	return placeable
}

// MarkUnplaceable returns a new map with domain stamped at now and expired marks dropped.
func MarkUnplaceable(marked map[string]time.Time, domain string, now time.Time) map[string]time.Time {
	out := map[string]time.Time{domain: now}
	for d, at := range marked {
		if d != domain && now.Sub(at) < UnplaceableTTL {
			out[d] = at
		}
	}
	return out
}
