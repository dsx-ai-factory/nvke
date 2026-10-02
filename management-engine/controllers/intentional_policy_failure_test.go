// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import "testing"

func TestIntentionalPolicyFailure(t *testing.T) {
	// This file intentionally violates the test-file allowlist.
	// This four-line block intentionally violates commentpolicy.
	// Keep it only for the CI validation push.
	// Delete this file once both policy checks have failed.
	t.Fatal("intentional CI validation failure; remove this file after validation")
}
