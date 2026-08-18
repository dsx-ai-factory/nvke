// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	nethttp "net/http"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func (s *Server) handleListIPBlocks(w nethttp.ResponseWriter, r *nethttp.Request) {
	page := parsePage(r)
	siteID := r.URL.Query().Get("siteId")

	rows, err := s.store.ListIPBlocks(r.Context(), siteID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]forgesdk.IpBlock, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, toSDKIPBlock(row))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}
