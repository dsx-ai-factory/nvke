// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package errors

import (
	stderrors "errors"
	"fmt"
	"testing"
)

// TestExitCodeContract snapshots the published Code to exit-status map. Every
// code appears once, statuses are distinct and banded, and a change to any
// integer fails here because callers branch on it.
func TestExitCodeContract(t *testing.T) {
	snapshot := map[Code]int{
		CodeInternal:    1,
		CodeUsage:       2,
		CodeUnsupported: 10,
	}
	if len(AllCodes) != len(snapshot) || len(exitCodes) != len(snapshot) {
		t.Fatalf("AllCodes=%d exitCodes=%d snapshot=%d; add a new code to all three", len(AllCodes), len(exitCodes), len(snapshot))
	}
	seenCode := map[Code]bool{}
	seenStatus := map[int]Code{}
	for _, c := range AllCodes {
		if seenCode[c] {
			t.Errorf("%s appears twice in AllCodes", c)
		}
		seenCode[c] = true
		got := ExitCode(New(c, "x"))
		if got != snapshot[c] {
			t.Errorf("%s exits %d, snapshot says %d", c, got, snapshot[c])
		}
		if prev, dup := seenStatus[got]; dup {
			t.Errorf("exit status %d used by both %s and %s", got, prev, c)
		}
		seenStatus[got] = c
		inBand := got <= 3 || (got >= 10 && got <= 59)
		if got == 0 || !inBand {
			t.Errorf("%s exits %d, outside every band", c, got)
		}
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"coded", New(CodeUsage, "x"), 2},
		{"wrapped coded", fmt.Errorf("outer: %w", New(CodeUnsupported, "y")), 10},
		{"plain", stderrors.New("boom"), 1},
		{"unmapped code", &Error{Code: "NVKE-OS-E-NOPE"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}
