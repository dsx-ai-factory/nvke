// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"errors"
	nethttp "net/http"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
)

const (
	// fakeInstanceTypeTotal is the simulated total capacity per instance type.
	fakeInstanceTypeTotal = int32(100)
	// fakeIBCapabilityName is the simulated InfiniBand device name.
	fakeIBCapabilityName = "mlx5"
	// fakeIBCapabilityCount is the simulated InfiniBand device count.
	fakeIBCapabilityCount = int32(1)
)

func (s *Server) handleListInstanceTypes(w nethttp.ResponseWriter, r *nethttp.Request) {
	page := parsePage(r)
	siteID := r.URL.Query().Get("siteId")

	rows, err := s.store.ListInstanceTypes(r.Context(), siteID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	activeByType, err := s.store.CountActiveInstancesByType(r.Context(), siteID)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]forgesdk.InstanceType, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, toSDKInstanceType(row, activeByType[row.ID]))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}

// handleGetInstanceType returns a single instance type by ID with allocation
// stats populated. CAPNICo's create preflight (SDK InstanceTypeAPI.GetInstanceType
// with includeAllocationStats=true) requires allocationStats.unusedUsable > 0
// before it will proceed to create an instance; an unknown ID yields a 404 so
// the controller reports InstanceTypeNotFound. The lookup is scoped to the org
// in the request path, so an instance type owned by another org 404s with the
// same body as an unknown ID.
func (s *Server) handleGetInstanceType(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgID := orgIDFromRequest(r)
	id := r.PathValue("instanceTypeId")

	row, err := s.store.GetInstanceType(r.Context(), orgID, id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Instance Type with specified ID")
			return
		}
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	activeByType, err := s.store.CountActiveInstancesByType(r.Context(), row.SiteID)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, nethttp.StatusOK, toSDKInstanceType(row, activeByType[row.ID]))
}
