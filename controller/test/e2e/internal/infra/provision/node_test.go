// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"net/http"
	"testing"
	"time"
)

// WithTimeout raises a timeout that is too short and leaves everything else
// alone. Zero is the case worth a test: it means "no timeout", so raising it
// to the floor would be lowering it from infinity, and a caller that left the
// field unset wants its context to be the only bound.
func TestWithTimeout(t *testing.T) {
	floor := 30 * time.Second

	for name, tc := range map[string]struct {
		client *http.Client
		want   time.Duration
	}{
		"nil client takes the floor":  {client: nil, want: floor},
		"absent timeout stays absent": {client: &http.Client{}, want: 0},
		"shorter timeout is raised":   {client: &http.Client{Timeout: time.Second}, want: floor},
		"longer timeout is kept":      {client: &http.Client{Timeout: time.Hour}, want: time.Hour},
		"equal timeout is kept":       {client: &http.Client{Timeout: floor}, want: floor},
	} {
		t.Run(name, func(t *testing.T) {
			if got := WithTimeout(tc.client, floor).Timeout; got != tc.want {
				t.Errorf("Timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// A copy, so the caller keeps its client and anything it injected into one.
func TestWithTimeoutCopiesRatherThanMutating(t *testing.T) {
	transport := http.DefaultTransport
	original := &http.Client{Timeout: time.Second, Transport: transport}

	raised := WithTimeout(original, time.Minute)

	if original.Timeout != time.Second {
		t.Errorf("the caller's client was mutated: Timeout = %v, want 1s", original.Timeout)
	}
	if raised == original {
		t.Error("WithTimeout returned the caller's client, want a copy")
	}
	if raised.Transport != transport {
		t.Error("the copy lost the injected transport")
	}
}
