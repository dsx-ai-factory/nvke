// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package cache

import (
	"log/slog"
	"time"
)

const (
	cacheRefreshIntervalFactor = 0.8
)

type BaseOperation struct {
	logger *slog.Logger

	// nextExecutionTime is the time when the operation is scheduled to run next
	nextExecutionTime time.Time

	// executionInterval is the interval between executions of the operation
	executionInterval time.Duration

	// retryPolicy defines the retry strategy for the operation
	retryPolicy *RetryPolicy

	// retryCount is the number of times the operation has been retried
	retryCount uint
}

func NewBaseOperation(logger *slog.Logger) *BaseOperation {
	return &BaseOperation{
		logger:            logger,
		nextExecutionTime: time.Now(),
		executionInterval: time.Minute,
		retryPolicy: &RetryPolicy{
			Multiplier:      2.0,
			InitialInterval: time.Second,
			MaxInterval:     time.Minute,
		},
		retryCount: 0,
	}
}

func (op *BaseOperation) GetNextExecutionTime() time.Time {
	return op.nextExecutionTime
}

// cacheRefreshInterval returns an interval that schedules a refresh after 80% of the cache TTL,
// leaving the remaining 20% for the refresh to complete before the cache expires.
func cacheRefreshInterval(cacheTTL time.Duration) time.Duration {
	return time.Duration(float64(cacheTTL) * cacheRefreshIntervalFactor)
}

func (op *BaseOperation) onExecutionFinished(err error) {
	if err != nil {
		op.retryCount++
		op.nextExecutionTime = op.retryPolicy.ApplyTo(time.Now(), op.retryCount)
		op.logger.Error("Execution failed; queuing retry", "error", err, "retryCount", op.retryCount, "nextExecution", op.nextExecutionTime)

		return
	}

	op.retryCount = 0
	op.nextExecutionTime = time.Now().Add(op.executionInterval)
}
