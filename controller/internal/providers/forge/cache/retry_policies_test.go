// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestExponentialBackoff(t *testing.T) {
	fromTime := time.Now()
	retryPolicy := RetryPolicy{
		InitialInterval: time.Second,
		Multiplier:      2.0,
	}

	tests := []struct {
		retryCount uint
		want       time.Time
		wantErr    bool
	}{
		{
			retryCount: 0,
			want:       fromTime.Add(retryPolicy.InitialInterval),
		},
		{
			retryCount: 1,
			want:       fromTime.Add(retryPolicy.InitialInterval * 2),
		},
		{
			retryCount: 2,
			want:       fromTime.Add(retryPolicy.InitialInterval * 4),
		},
		{
			retryCount: 3,
			want:       fromTime.Add(retryPolicy.InitialInterval * 8),
		},
		{
			retryCount: 4,
			want:       fromTime.Add(retryPolicy.InitialInterval * 16),
		},
		{
			retryCount: 5,
			want:       fromTime.Add(retryPolicy.InitialInterval * 32),
		},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("Retry Count %d", tt.retryCount), func(t *testing.T) {
			got := retryPolicy.ApplyTo(fromTime, tt.retryCount)
			if !got.Equal(tt.want) {
				t.Errorf("Expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestMaxInterval(t *testing.T) {
	fromTime := time.Now()
	retryPolicy := RetryPolicy{
		InitialInterval: time.Second,
		Multiplier:      5.0,
		MaxInterval:     time.Minute,
	}

	tests := []struct {
		retryCount uint
		want       time.Time
	}{
		{
			retryCount: 0,
			want:       fromTime.Add(retryPolicy.InitialInterval),
		},
		{
			retryCount: 1,
			want:       fromTime.Add(retryPolicy.InitialInterval * 5),
		},
		{
			retryCount: 2,
			want:       fromTime.Add(retryPolicy.InitialInterval * 25),
		},
		{
			retryCount: 3,
			want:       fromTime.Add(retryPolicy.MaxInterval),
		},
		{
			retryCount: 50,
			want:       fromTime.Add(retryPolicy.MaxInterval),
		},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("Retry Count %d", tt.retryCount), func(t *testing.T) {
			got := retryPolicy.ApplyTo(fromTime, tt.retryCount)
			if !got.Equal(tt.want) {
				t.Errorf("Expected %v, got %v", tt.want, got)
			}
		})
	}
}
