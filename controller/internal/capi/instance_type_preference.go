// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"errors"

	"github.com/google/uuid"
)

// ErrPreferenceCurrentNotFound means the current candidate is not present in the
// ordered preference list, so the walk cannot determine what comes next.
var ErrPreferenceCurrentNotFound = errors.New("current candidate is not in the configured preference list")

// NextInPreference walks an ordered preference list of UUID candidates and
// returns the entry immediately after current, wrapping back to the first entry
// after the last. Candidates are matched against current by their string form
// (uuid.UUID.String()), which is how NicoMachine specs carry them.
//
// The wrap-around means it never reports exhaustion (the only error is
// ErrPreferenceCurrentNotFound): a caller that keeps polling converges on the
// highest-preference candidate that eventually becomes usable, because the walk
// always advances in list order and cycles back to index 0, so the most-preferred
// entry is retried once per cycle and wins as soon as it has capacity rather than
// getting stuck on a lower-preference fallback. A single-entry list returns that
// same entry (retry in place).
func NextInPreference(preference []uuid.UUID, current string) (uuid.UUID, error) {
	currentIndex := indexInPreference(preference, current)
	if currentIndex == -1 {
		return uuid.Nil, ErrPreferenceCurrentNotFound
	}
	return preference[(currentIndex+1)%len(preference)], nil
}

func indexInPreference(preference []uuid.UUID, current string) int {
	for i, candidate := range preference {
		if candidate.String() == current {
			return i
		}
	}
	return -1
}
