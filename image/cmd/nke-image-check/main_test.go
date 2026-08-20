// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
)

func TestRunRequiresEndpoint(t *testing.T) {
	code := run(nil)
	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}

func TestRunVersion(t *testing.T) {
	if code := run([]string{"-version"}); code != 0 {
		t.Fatalf("run(-version) = %d, want 0", code)
	}
}
