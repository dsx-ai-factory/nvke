// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"maps"
	"testing"
)

func TestParse(t *testing.T) {
	s, err := Parse("controller=v1.0.0,cli=v0.9.0", "arch=amd64,kube=1.34")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"controller": "v1.0.0", "cli": "v0.9.0"}; !maps.Equal(s.Versions, want) {
		t.Errorf("versions: got %v, want %v", s.Versions, want)
	}
	if want := map[string]string{"arch": "amd64", "kube": "1.34"}; !maps.Equal(s.Capabilities, want) {
		t.Errorf("capabilities: got %v, want %v", s.Capabilities, want)
	}
}

func TestParseNothingIsNotAnError(t *testing.T) {
	// A plain `make test-e2e` sets none of these and must keep working
	// exactly as it did.
	s, err := Parse("", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Requested() {
		t.Errorf("got %v, want no selection", s)
	}
}

func TestParseRejectsMalformedPairs(t *testing.T) {
	// These arrive from the qualify tool, not a person. A pair that does not
	// parse means the two sides disagree about the format, and dropping it
	// would qualify a different selection than the one recorded.
	for _, in := range []string{
		"controller",
		"controller=",
		"=v1.0.0",
		"controller=v1.0.0,",
		"controller=v1.0.0,controller=v2.0.0",
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := Parse(in, ""); err == nil {
				t.Errorf("want an error for %q", in)
			}
		})
	}
}

func TestParseNamesWhichFlagWasWrong(t *testing.T) {
	_, err := Parse("", "bad")
	if err == nil {
		t.Fatal("want an error")
	}
	if got := err.Error(); got[:len("-e2e.capabilities")] != "-e2e.capabilities" {
		t.Errorf("got %q, want the flag named first", got)
	}
}

func TestStringIsOrdered(t *testing.T) {
	// It lands in the suite's log, which is read alongside the plan.
	s, err := Parse("cli=v0.9.0,controller=v1.0.0", "kube=1.34,arch=amd64")
	if err != nil {
		t.Fatal(err)
	}
	s.Environment = "forge"
	want := "environment=forge versions=cli=v0.9.0,controller=v1.0.0 capabilities=arch=amd64,kube=1.34"
	if got := s.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStringWithNoSelection(t *testing.T) {
	if got := (Selection{}).String(); got == "" {
		t.Error("the log line must say something even when nothing was asked for")
	}
}
