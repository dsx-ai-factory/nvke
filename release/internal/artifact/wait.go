// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// DefaultPoll is how often an unpublished artifact is asked for again. A
// publish takes minutes, so asking much more often only adds requests.
const DefaultPoll = 30 * time.Second

// Location is one thing to check and what asked for it. Wanted is carried so a
// failure names the components a reader has to go and look at, rather than only
// a URL they then have to trace back.
type Location struct {
	URL    string
	Wanted []string
}

// WaitFor asks for every location until each is published or the budget runs
// out.
//
// A location that answers is never asked again, so a long wait for one slow
// publisher does not keep re-fetching the artifacts that are already there.
//
// With no budget this is one pass, which is what a run that resolved a release
// wants: the artifacts were published when the release was cut, and a missing
// one is a fault to report now rather than something to wait out.
func (c Checker) WaitFor(
	ctx context.Context,
	out io.Writer,
	want []Location,
	budget, every time.Duration,
) error {
	deadline := time.Now().Add(budget)
	pending := want

	for attempt := 1; ; attempt++ {
		var missing []Location
		var reasons []string

		var settled []string
		for _, l := range pending {
			if err := c.Exists(ctx, l.URL); err != nil {
				reason := fmt.Sprintf("  %s (%s)\n    %v", l.URL, strings.Join(l.Wanted, ", "), err)
				if errors.Is(err, ErrSettled) {
					settled = append(settled, reason)
					continue
				}
				missing = append(missing, l)
				reasons = append(reasons, reason)
				continue
			}
			fmt.Fprintf(out, "published: %s (%s)\n", l.URL, strings.Join(l.Wanted, ", "))
		}

		// Waiting cannot turn these into a different answer, so the budget is
		// spent for nothing and the run learns an hour and a half late that a
		// credential was wrong.
		if len(settled) > 0 {
			return fmt.Errorf("%d of the %d artifacts the plan names cannot be confirmed, and waiting "+
				"will not change that:\n%s", len(settled), len(want), strings.Join(settled, "\n"))
		}

		if len(missing) == 0 {
			fmt.Fprintf(out, "all %d artifacts the plan names are published\n", len(want))
			return nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%d of the %d artifacts the plan names %s:\n%s",
				len(missing), len(want), notPublished(budget), strings.Join(reasons, "\n"))
		}

		pending = missing
		fmt.Fprintf(out, "attempt %d: %d of %d not published yet, asking again in %v (%v left)\n",
			attempt, len(missing), len(want), every, remaining.Round(time.Second))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(every, remaining)):
		}
	}
}

// notPublished says what the failure means, which differs by whether anything
// was waited for. Without a wait the artifacts were expected to be there
// already, and with one they were given time and never arrived.
func notPublished(budget time.Duration) string {
	if budget <= 0 {
		return "are not published"
	}
	return fmt.Sprintf("were still not published after %v", budget)
}
