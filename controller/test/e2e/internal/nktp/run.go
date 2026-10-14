// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
)

// stage selects the checks whose hold matches and gives them one budget, and
// optionally a window they must then keep holding for.
type stage struct {
	name   string
	hold   bool
	budget time.Duration
	window time.Duration
}

// run runs the stage's checks concurrently and returns an error per check
// that did not hold, nil for the rest.
func run(ctx context.Context, log logr.Logger, t *target, checks []Check, s stage) []error {
	failures := make([]error, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		if c.hold != s.hold {
			continue
		}
		wg.Go(func() {
			if err := runCheck(ctx, t, s, c); err != nil {
				failures[i] = fmt.Errorf("%s: %s: %w", s.name, c.name, err)
				log.Info("NKTP check did not hold", "stage", s.name, "check", c.name, "error", err.Error())
				return
			}
			log.Info("NKTP check holds", "stage", s.name, "check", c.name)
		})
	}
	wg.Wait()
	return failures
}

// runCheck returns the probe's own last error rather than Gomega's, which
// renders an error as a type and struct dump.
func runCheck(ctx context.Context, t *target, s stage, c Check) error {
	var lastErr error
	probe := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
		defer cancel()
		lastErr = c.probe(ctx, t)
		return lastErr
	}
	g := gomega.NewGomega(func(string, ...int) {})

	budgetCtx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	if !g.Eventually(probe).WithContext(budgetCtx).WithTimeout(s.budget).WithPolling(pollInterval).Should(gomega.Succeed()) {
		return fmt.Errorf("did not hold within %s: %w", s.budget, cmp.Or(lastErr, budgetCtx.Err()))
	}
	if s.window > 0 && !g.Consistently(probe).WithContext(ctx).WithTimeout(s.window).WithPolling(pollInterval).Should(gomega.Succeed()) {
		return fmt.Errorf("held, then stopped holding within %s: %w", s.window, cmp.Or(lastErr, ctx.Err()))
	}
	return nil
}
