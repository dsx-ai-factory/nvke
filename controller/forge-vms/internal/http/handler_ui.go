// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import nethttp "net/http"

func (s *Server) handleUIRoot(w nethttp.ResponseWriter, r *nethttp.Request) {
	nethttp.Redirect(w, r, "/ui/", nethttp.StatusFound)
}

func (s *Server) handleUIIndex(w nethttp.ResponseWriter, r *nethttp.Request) {
	nethttp.ServeFile(w, r, "ui/index.html")
}
