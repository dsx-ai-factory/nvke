// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func TestUnit_HandleGetInstanceType(t *testing.T) {
	// The handler returns 404 for IDs absent from instanceTypeCapacity.

	s := &server{
		cfg: config{InstanceTypeID: defaultInstanceTypeID},
		instanceTypeCapacity: map[string]int32{
			defaultInstanceTypeID: int32(defaultInstanceTypeUnusedUsable),
		},
	}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	t.Run("returnsInstanceType", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/org/local-dev-org/carbide/instance/type/"+defaultInstanceTypeID+"?includeAllocationStats=true", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
		}
		var got nicosdk.InstanceType
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if got.GetId() != defaultInstanceTypeID {
			t.Fatalf("instance type ID = %q, want %q", got.GetId(), defaultInstanceTypeID)
		}
		if got.AllocationStats == nil {
			t.Fatal("allocationStats is nil")
		}
		if got.AllocationStats.GetUnusedUsable() != int32(defaultInstanceTypeUnusedUsable) {
			t.Fatalf("unusedUsable = %d, want %d", got.AllocationStats.GetUnusedUsable(), defaultInstanceTypeUnusedUsable)
		}
	})

	t.Run("returnsNotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/org/local-dev-org/carbide/instance/type/00000000-0000-0000-0000-000000000999", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status code = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}

func TestUnit_HandleGetVPC(t *testing.T) {
	s := &server{
		cfg: config{
			OrgID:    defaultOrgID,
			TenantID: defaultTenantID,
			SiteID:   defaultSiteID,
			VPCID:    defaultVPCID,
		},
	}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	t.Run("returnsVPC", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/org/local-dev-org/carbide/vpc/"+defaultVPCID, nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
		}
		var got nicosdk.VPC
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if got.GetId() != defaultVPCID {
			t.Fatalf("vpc ID = %q, want %q", got.GetId(), defaultVPCID)
		}
		if got.GetName() != defaultVPCName {
			t.Fatalf("vpc name = %q, want %q", got.GetName(), defaultVPCName)
		}
	})

	t.Run("returnsNotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/org/local-dev-org/carbide/vpc/00000000-0000-0000-0000-000000000999", nil)
		rec := httptest.NewRecorder()

		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status code = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}
