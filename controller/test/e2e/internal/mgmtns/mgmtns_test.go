// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mgmtns

import "testing"

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fromEnv string
		want    string
	}{
		{name: "unset falls back to the chart default", want: Default},
		{name: "blank is not a namespace", fromEnv: "   ", want: Default},
		{name: "a legacy site is honored", fromEnv: "nke-system", want: "nke-system"},
		{name: "surrounding whitespace is trimmed", fromEnv: " platform-system\n", want: "platform-system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.fromEnv); got != tc.want {
				t.Errorf("resolve(%q) = %q, want %q", tc.fromEnv, got, tc.want)
			}
		})
	}
}
