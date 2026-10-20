// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package artifactdiagnostic

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCommandCancellationAndCapture(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		cancel     bool
	}{{"failure", "exit 7", false}, {"timeout", "sleep 30", true}} {
		t.Run(tc.name, func(t *testing.T) {
			log, err := os.Create(filepath.Join(t.TempDir(), "log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			r := runner{log: log, options: Options{Output: t.TempDir()}}
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			if _, err := r.command(ctx, nil, "bash", "-c", tc.body); err == nil {
				t.Fatal("failure lost")
			}
			if tc.cancel && time.Since(start) > 5*time.Second {
				t.Fatal("canceled child was not joined promptly")
			}
		})
	}
	sink := boundedOutput{destination: os.Stdout, remaining: 0}
	if _, err := sink.Write([]byte("ignored")); err != nil || !sink.truncated {
		t.Fatal("capture truncation hidden")
	}
}
