// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetTenants(dockerTest bool) []*forgesdk.Tenant {
	tenants := []*forgesdk.Tenant{
		{
			Id:             forgesdk.PtrString("d282929f-49d5-4dec-8d27-21ceb30ade19"),
			Org:            forgesdk.PtrString("novxwn552ssc"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-NKE"),
		},
		{
			Id:             forgesdk.PtrString("1a2f59f3-3422-4b8b-a279-50390af917b6"),
			Org:            forgesdk.PtrString("dipfedrcvjiw"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-DGXC-Dev"),
		},
		{
			Id:             forgesdk.PtrString("07c17329-6f30-4e7e-83f5-dc0c61844255"),
			Org:            forgesdk.PtrString("hznrvgqi8cxz"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-OSMO"),
		},
		{
			Id:             forgesdk.PtrString("29c07909-91dc-465e-a35e-8ca4f9d760a8"),
			Org:            forgesdk.PtrString("hdudgxmcpv1l"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-NVCF"),
		},
		{
			Id:             forgesdk.PtrString("1f8e4020-55f2-43c7-983c-ba14adc09800"),
			Org:            forgesdk.PtrString("0724087087997721"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-Lepton"),
		},
		{
			Id:             forgesdk.PtrString("cbda0bf2-ad00-4e03-8b50-1adf1d34e06b"),
			Org:            forgesdk.PtrString("0738478184205004"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-VMaaS"),
		},
		{
			Id:             forgesdk.PtrString("a3a62602-d74e-44e3-bc68-d524d11eb467"),
			Org:            forgesdk.PtrString("0996499333036436"),
			OrgDisplayName: forgesdk.PtrString("Forge-Tenant-RelEng"),
		},
	}

	if dockerTest {
		tenants = append(tenants, &forgesdk.Tenant{
			Id:             forgesdk.PtrString("00000000-0000-0000-0000-000000000010"),
			Org:            forgesdk.PtrString("local-dev-org"),
			OrgDisplayName: forgesdk.PtrString("Local-Dev"),
		})
	}

	return tenants
}
