// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	nethttp "net/http"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
)

func (s *Server) handleListSubnets(w nethttp.ResponseWriter, r *nethttp.Request) {
	page := parsePage(r)
	siteID, ok := s.siteIDForList(w, r)
	if !ok {
		return
	}

	rows, err := s.store.ListSubnets(r.Context(), siteID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]nicosdk.Subnet, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, toSDKSubnet(row))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}
