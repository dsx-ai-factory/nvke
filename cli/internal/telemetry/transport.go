// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"log/slog"
	"net/http"
)

// userAgentTransport sets the nvkectl User-Agent and logs each request at debug
// level.
type userAgentTransport struct {
	base http.RoundTripper
}

func (transport userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", currentUserAgent())
	slog.DebugContext(req.Context(), "sending request", "method", req.Method, "path", req.URL.Path)
	return transport.base.RoundTrip(req)
}
