// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/seed"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
)

func TestValidateVPCAndSubnetsPassesWhenContained(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	doc := baseNetworkingSeedDoc()
	doc.Subnets[0].CIDR = "10.42.0.0/24"
	doc.Subnets[0].PrefixLength = 24
	doc.VPCPrefixes[0].Prefix = "10.42.0.248/29"
	doc.VPCPrefixes[0].PrefixLength = 29
	if err := store.ApplySeed(ctx, doc); err != nil {
		t.Fatalf("ApplySeed failed: %v", err)
	}

	if err := ValidateVPCAndSubnets(ctx, store, "10.42.0.0/24"); err != nil {
		t.Fatalf("ValidateVPCAndSubnets failed: %v", err)
	}
}

func TestValidateVPCAndSubnetsFailsWhenSubnetOutOfRange(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	doc := baseNetworkingSeedDoc()
	doc.Subnets[0].CIDR = "10.50.0.0/24"
	doc.Subnets[0].PrefixLength = 24
	doc.VPCPrefixes[0].Prefix = "10.50.0.248/29"
	doc.VPCPrefixes[0].PrefixLength = 29
	if err := store.ApplySeed(ctx, doc); err != nil {
		t.Fatalf("ApplySeed failed: %v", err)
	}

	err := ValidateVPCAndSubnets(ctx, store, "10.51.0.0/24")
	if err == nil {
		t.Fatal("ValidateVPCAndSubnets expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not contained within bridge cidr") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateVPCAndSubnetsFailsWhenVPCPrefixOutOfRange(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	doc := baseNetworkingSeedDoc()
	doc.Subnets[0].CIDR = "10.61.0.0/24"
	doc.Subnets[0].PrefixLength = 24
	doc.VPCPrefixes[0].Prefix = "10.62.0.0/29"
	doc.VPCPrefixes[0].PrefixLength = 29
	if err := store.ApplySeed(ctx, doc); err != nil {
		t.Fatalf("ApplySeed failed: %v", err)
	}

	err := ValidateVPCAndSubnets(ctx, store, "10.61.0.0/24")
	if err == nil {
		t.Fatal("ValidateVPCAndSubnets expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not contained within bridge cidr") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func openTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "forge-vms.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func baseNetworkingSeedDoc() seed.Document {
	return seed.Document{
		Tenants: []seed.Tenant{
			{ID: "tenant-1", OrgID: "org-1", DisplayName: "tenant"},
		},
		Sites: []seed.Site{
			{ID: "site-1", Name: "site", TenantID: "tenant-1"},
		},
		VPCs: []seed.VPC{
			{ID: "vpc-1", Name: "vpc", TenantID: "tenant-1", SiteID: "site-1"},
		},
		Subnets: []seed.Subnet{
			{ID: "subnet-1", Name: "subnet", VPCID: "vpc-1", SiteID: "site-1", CIDR: "10.1.0.0/24", PrefixLength: 24},
		},
		VPCPrefixes: []seed.VPCPrefix{
			{ID: "pfx-1", Name: "prefix", VPCID: "vpc-1", SiteID: "site-1", PrefixLength: 29, Prefix: "10.1.0.248/29"},
		},
		InstanceTypes: []seed.InstanceType{
			{ID: "itype-1", Name: "itype", SiteID: "site-1", GPUCount: 1, CPUCount: 1, MemoryGiB: 1},
		},
	}
}
