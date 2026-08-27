// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"errors"
	nethttp "net/http"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/nico-vms/internal/state"
)

func (s *Server) handleListVPCs(w nethttp.ResponseWriter, r *nethttp.Request) {
	page := parsePage(r)
	siteID := r.URL.Query().Get("siteId")

	rows, err := s.store.ListVPCs(r.Context(), siteID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]forgesdk.VPC, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, toSDKVPC(row))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}

// handleGetVPC returns a single VPC by ID. CAPNICo's NicoMachine reconcile
// fetches the VPC (SDK VPCAPI.GetVpc) to read its name for machine-topology
// labels/status; an unknown ID yields a 404 in the Forge error shape. The lookup
// is scoped to the org in the request path, so a VPC owned by another org 404s
// with the same body as an unknown ID.
func (s *Server) handleGetVPC(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgID := orgIDFromRequest(r)
	id := r.PathValue("vpcId")

	row, err := s.store.GetVPC(r.Context(), orgID, id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find VPC with specified ID")
			return
		}
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, nethttp.StatusOK, toSDKVPC(row))
}
