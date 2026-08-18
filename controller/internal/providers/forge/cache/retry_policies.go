// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"math"
	"time"
)

type RetryPolicy struct {
	// The backoff multiplier to apply when retrying a failed operation
	Multiplier float64

	// The initial interval to use for backoff
	InitialInterval time.Duration

	// The maximum interval to use for backoff; 0 = unlimited
	MaxInterval time.Duration
}

// ApplyTo calculates the next retry time based on the initial time, the number of retries,
// and the retry policy's backoff parameters.
func (p *RetryPolicy) ApplyTo(initialTime time.Time, retryCount uint) time.Time {
	interval := float64(p.InitialInterval)

	for range retryCount {
		interval *= p.Multiplier

		// Clamp the interval to the maximum values
		if p.MaxInterval > 0 && interval > float64(p.MaxInterval) {
			interval = float64(p.MaxInterval)
			break
		}

		// When there isn't a MaxInterval, make sure we won't overflow
		// the time.Duration conversion
		if p.MaxInterval == 0 && interval > float64(math.MaxInt64) {
			interval = float64(math.MaxInt64)
			break
		}
	}

	return initialTime.Add(time.Duration(interval))
}
