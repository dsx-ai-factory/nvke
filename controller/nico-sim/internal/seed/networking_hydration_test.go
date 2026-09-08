// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"net"
	"strings"
	"testing"
)

func TestHydrateNetworkingCIDRsFillsMissingRanges(t *testing.T) {
	doc := Document{
		Subnets: []Subnet{
			{ID: "subnet-1"},
		},
		VPCPrefixes: []VPCPrefix{
			{ID: "pfx-b", PrefixLength: 29},
			{ID: "pfx-a", PrefixLength: 29},
		},
	}

	hydrated, err := HydrateNetworkingCIDRs(doc, "10.42.0.0/24")
	if err != nil {
		t.Fatalf("HydrateNetworkingCIDRs failed: %v", err)
	}

	if hydrated.Subnets[0].CIDR != "10.42.0.0/24" {
		t.Fatalf("subnet cidr = %q, want 10.42.0.0/24", hydrated.Subnets[0].CIDR)
	}
	if hydrated.Subnets[0].PrefixLength != 24 {
		t.Fatalf("subnet prefix_length = %d, want 24", hydrated.Subnets[0].PrefixLength)
	}

	if strings.TrimSpace(hydrated.VPCPrefixes[0].Prefix) == "" || strings.TrimSpace(hydrated.VPCPrefixes[1].Prefix) == "" {
		t.Fatalf("expected hydrated vpc prefixes to be non-empty")
	}
	if hydrated.VPCPrefixes[0].Prefix == hydrated.VPCPrefixes[1].Prefix {
		t.Fatalf("hydrated prefixes should not overlap exactly: %q", hydrated.VPCPrefixes[0].Prefix)
	}
	if !cidrContainsCIDR(t, "10.42.0.0/24", hydrated.VPCPrefixes[0].Prefix) {
		t.Fatalf("prefix %q not contained in bridge", hydrated.VPCPrefixes[0].Prefix)
	}
	if !cidrContainsCIDR(t, "10.42.0.0/24", hydrated.VPCPrefixes[1].Prefix) {
		t.Fatalf("prefix %q not contained in bridge", hydrated.VPCPrefixes[1].Prefix)
	}
}

func TestHydrateNetworkingCIDRsPreservesExplicitRanges(t *testing.T) {
	doc := Document{
		Subnets: []Subnet{
			{ID: "subnet-1", CIDR: "10.50.0.0/24", PrefixLength: 24},
		},
		VPCPrefixes: []VPCPrefix{
			{ID: "pfx-1", PrefixLength: 29, Prefix: "10.50.0.248/29"},
		},
	}

	hydrated, err := HydrateNetworkingCIDRs(doc, "10.50.0.0/24")
	if err != nil {
		t.Fatalf("HydrateNetworkingCIDRs failed: %v", err)
	}

	if hydrated.Subnets[0].CIDR != "10.50.0.0/24" {
		t.Fatalf("subnet cidr changed unexpectedly: %q", hydrated.Subnets[0].CIDR)
	}
	if hydrated.VPCPrefixes[0].Prefix != "10.50.0.248/29" {
		t.Fatalf("vpc prefix changed unexpectedly: %q", hydrated.VPCPrefixes[0].Prefix)
	}
}

func TestHydrateNetworkingCIDRsFailsWhenPrefixSpaceExhausted(t *testing.T) {
	doc := Document{
		Subnets: []Subnet{
			{ID: "subnet-1"},
		},
		VPCPrefixes: []VPCPrefix{
			{ID: "pfx-1", PrefixLength: 24},
			{ID: "pfx-2", PrefixLength: 24},
		},
	}

	_, err := HydrateNetworkingCIDRs(doc, "10.99.0.0/24")
	if err == nil {
		t.Fatal("HydrateNetworkingCIDRs expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unable to place vpc prefix") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func cidrContainsCIDR(t *testing.T, parentCIDR, childCIDR string) bool {
	t.Helper()
	_, parent, err := net.ParseCIDR(parentCIDR)
	if err != nil {
		t.Fatalf("parse parent cidr: %v", err)
	}
	_, child, err := net.ParseCIDR(childCIDR)
	if err != nil {
		t.Fatalf("parse child cidr: %v", err)
	}
	contained, err := cidrContainedBy(parent, child)
	if err != nil {
		t.Fatalf("cidrContainedBy failed: %v", err)
	}
	return contained
}
