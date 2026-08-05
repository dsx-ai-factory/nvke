// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package http

import (
	nethttp "net/http"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

func (s *Server) handleListVPCPrefixes(w nethttp.ResponseWriter, r *nethttp.Request) {
	page := parsePage(r)
	siteID := r.URL.Query().Get("siteId")

	rows, err := s.store.ListVPCPrefixes(r.Context(), siteID, page)
	if err != nil {
		writeForgeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]forgesdk.VpcPrefix, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, toSDKVPCPrefix(row))
	}
	writeJSON(w, nethttp.StatusOK, resp)
}
