// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"errors"
	nethttp "net/http"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/state"
)

func (s *Server) handleListSites(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgID := orgIDFromRequest(r)
	page := parsePage(r)

	sites, err := s.store.ListSites(r.Context(), orgID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]nicosdk.Site, 0, len(sites))
	for _, site := range sites {
		resp = append(resp, toSDKSite(site, orgID))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}

func (s *Server) handleGetSite(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgID := orgIDFromRequest(r)
	site, err := s.store.GetSiteForOrg(r.Context(), orgID, r.PathValue("siteId"))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeForgeError(w, nethttp.StatusNotFound, "Could not find Site with specified ID")
			return
		}
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nethttp.StatusOK, toSDKSite(site, orgID))
}
