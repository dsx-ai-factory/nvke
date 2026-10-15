// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"slices"
	"sync"
)

// Missing runs the presence checks in checks once, concurrently, and returns a
// line per signal that has not arrived. Unwanted does the same for the absence
// checks and returns a line per signal that is there but must not be. Each is a
// value for a spec to poll until it is empty: Eventually over Missing until
// every signal has shown up, then Eventually and Consistently over Unwanted, so
// a failure from a rollout ages out of its window and then has to stay out.
func (v *Validator) Missing(ctx context.Context, checks []Check) []string {
	return v.unmet(ctx, checks, false)
}

// Unwanted is the absence checks' counterpart to Missing.
func (v *Validator) Unwanted(ctx context.Context, checks []Check) []string {
	return v.unmet(ctx, checks, true)
}

// unmet runs the checks of one kind concurrently and returns a line per check
// that does not hold.
func (v *Validator) unmet(ctx context.Context, checks []Check, absence bool) []string {
	unmet := make([]string, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		if c.absence != absence {
			continue
		}
		wg.Go(func() {
			attempt, cancel := context.WithTimeout(ctx, attemptTimeout)
			defer cancel()
			if err := c.Probe(attempt); err != nil {
				unmet[i] = c.Name + ": " + err.Error()
			}
		})
	}
	wg.Wait()
	return slices.DeleteFunc(unmet, func(line string) bool { return line == "" })
}
