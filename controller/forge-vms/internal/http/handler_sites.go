// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	nethttp "net/http"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func (s *Server) handleListSites(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgID := orgIDFromRequest(r)
	page := parsePage(r)

	sites, err := s.store.ListSites(r.Context(), orgID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]forgesdk.Site, 0, len(sites))
	for _, site := range sites {
		resp = append(resp, toSDKSite(site))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}
