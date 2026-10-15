// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func TestLocationsAreEveryOneTheplanNames(t *testing.T) {
	plan := &qualification.Plan{Combinations: []qualification.Combination{{
		Components: []qualification.ComponentVersion{
			{Name: "controller", Version: "v1", URL: "oci://r.example/nvke-controller:v1"},
			// A component resolved from tags and published nowhere this names.
			{Name: "os-image", Version: "v1"},
		},
		Dependencies: []qualification.ComponentVersion{
			{Name: "os-image-bootloader-amd64", URL: "https://cdn.example/efi.efi"},
			// Read from a lock file, so there is nothing to ask for.
			{Name: "kube", Version: "1.36.3"},
		},
		Streams: []qualification.Stream{
			{Name: "a", Dependencies: []qualification.ComponentVersion{
				{Name: "os-image-amd64", URL: "https://cdn.example/image.qcow2"},
			}},
			// The same image on a second stream, which is one thing to ask for
			// and not two.
			{Name: "b", Dependencies: []qualification.ComponentVersion{
				{Name: "os-image-amd64", URL: "https://cdn.example/image.qcow2"},
			}},
		},
	}}}

	got := locations(plan)
	want := []string{
		"https://cdn.example/efi.efi",
		"https://cdn.example/image.qcow2",
		"oci://r.example/nvke-controller:v1",
	}
	if len(got) != len(want) {
		t.Fatalf("checked %d locations, want %d: %v", len(got), len(want), got)
	}
	for i, l := range got {
		if l.URL != want[i] {
			t.Errorf("location %d = %q, want %q", i, l.URL, want[i])
		}
	}
	if names := got[2].Wanted; len(names) != 1 || names[0] != "controller" {
		t.Errorf("the controller url was wanted by %v", names)
	}
}
