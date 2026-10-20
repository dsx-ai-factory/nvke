// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package health

import "testing"

func TestRequiredHealthResponses(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, version string
		status                    int
		fail                      bool
	}{
		{"health", "/healthz", "ok", "", 200, false},
		{"ready", "/readyz", "ok", "", 200, false},
		{"unready", "/readyz", "unavailable", "", 503, true},
		{"version", "/api/version", `{"version":"v1"}`, "v1", 200, false},
		{"wrong", "/api/version", `{"version":"v2"}`, "v1", 200, true},
		{"missing expectation", "/api/version", `{"version":"v1"}`, "", 200, true},
		{"missing observation", "/api/version", `{}`, "v1", 200, true},
		{"malformed", "/api/version", `{`, "v1", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Check(tc.path, tc.status, []byte(tc.body), tc.version); (err != nil) != tc.fail {
				t.Fatalf("Check: %v", err)
			}
		})
	}
}
