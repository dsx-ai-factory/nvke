// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProbeRetriesUntilHealthy(t *testing.T) {
	calls := 0
	_, err := waitHealthy(context.Background(), time.Second, time.Millisecond, time.Second,
		func(context.Context) (string, error) {
			calls++
			if calls < 3 {
				return "", errUnhealthy
			}
			return "v1.2.3", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("probed %d times, want 3", calls)
	}
}

func TestProbeReportsNeverHealthy(t *testing.T) {
	_, err := waitHealthy(context.Background(), 20*time.Millisecond, time.Millisecond, time.Millisecond,
		func(context.Context) (string, error) { return "", errUnhealthy })
	if !errors.Is(err, errUnhealthy) || !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("err = %v", err)
	}
}

// TestProbeAttemptHangs covers the attempt that connects and then never
// answers, which is how a site looks while the controller listens but its database and
// Temporal checks have not passed yet. The wait has to outlive that attempt
// and come round again, or the site is called dead on the strength of one
// request.
func TestProbeAttemptHangs(t *testing.T) {
	calls := 0
	probe := func(ctx context.Context) (string, error) {
		calls++
		<-ctx.Done()
		return "", ctx.Err()
	}

	const (
		within = 200 * time.Millisecond
		each   = 20 * time.Millisecond
	)
	_, err := waitHealthy(context.Background(), within, time.Millisecond, each, probe)
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("err = %v", err)
	}
	if calls < 5 {
		t.Errorf("probed %d times in %s with a %s attempt timeout, want the loop to keep polling",
			calls, within, each)
	}
}
