// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootCommandWritesToSetOut proves the command tree routes output through
// cmd.OutOrStdout() rather than os.Stdout. Without that a test can only read
// command output by swapping the process's os.Stdout for a pipe, which is what
// this package used to do.
//
// The assertion is exact, not a substring: a stray fmt.Println anywhere on this
// path still reaches the real stdout and leaves this buffer short.
func TestRootCommandWritesToSetOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "short",
			args: []string{"version", "--output", "short"},
			want: "v1.2.3\n",
		},
		{
			name: "default",
			args: []string{"version"},
			want: "nvkectl v1.2.3 (commit: abc1234, built: 2026-01-01T00:00:00Z)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			root := NewRootCmd("v1.2.3", "abc1234", "clean", "2026-01-01T00:00:00Z")
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)

			if err := root.Execute(); err != nil {
				t.Fatalf("nvkectl %s: %v", strings.Join(tc.args, " "), err)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("captured output = %q, want %q", got, tc.want)
			}
		})
	}
}
