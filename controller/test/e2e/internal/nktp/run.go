// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"slices"
	"sync"
)

// Failing runs every check once, concurrently, and returns a line per check
// that did not hold, for a spec to poll: Eventually over Arrive until every
// signal has shown up, then Eventually and Consistently over Hold, so a
// failure from a rollout ages out of its window and then has to stay out.
func (v *Validator) Failing(ctx context.Context, checks []Check) []string {
	failing := make([]string, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Go(func() {
			attempt, cancel := context.WithTimeout(ctx, attemptTimeout)
			defer cancel()
			if err := c.Probe(attempt); err != nil {
				failing[i] = c.Name + ": " + err.Error()
			}
		})
	}
	wg.Wait()
	return slices.DeleteFunc(failing, func(line string) bool { return line == "" })
}

// Arrive selects the checks that wait for a signal to show up.
func Arrive(checks []Check) []Check { return withHold(checks, false) }

// Hold selects the checks that must keep holding once the signals have arrived.
func Hold(checks []Check) []Check { return withHold(checks, true) }

func withHold(checks []Check, hold bool) []Check {
	return slices.DeleteFunc(slices.Clone(checks), func(c Check) bool { return c.Hold != hold })
}
