// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"errors"
	nethttp "net/http"

	"github.com/NVIDIA/nke/controller/nico-sim/internal/state"
)

// handleGetCurrentTenant returns the tenant seeded for the request's org.
// CAPNICo hits this endpoint (SDK TenantAPI.GetCurrentTenant) to resolve its
// tenant ID before creating any instance; it reads the "id" field and fails
// when the response is missing or the id is empty.
func (s *Server) handleGetCurrentTenant(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgID := orgIDFromRequest(r)

	row, err := s.store.GetTenantByOrg(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Tenant for specified Org")
			return
		}
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, nethttp.StatusOK, toSDKTenant(row))
}
