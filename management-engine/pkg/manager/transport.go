// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// originTransport checks every request, including redirects, before it is sent.
type originTransport struct {
	http.RoundTripper
	origin string
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || apiOrigin(req.URL) != t.origin {
		return nil, fmt.Errorf("management API request must stay on its HTTPS origin")
	}
	return t.RoundTripper.RoundTrip(req)
}

func (t *originTransport) WrappedRoundTripper() http.RoundTripper { return t.RoundTripper }

func apiOrigin(endpoint *url.URL) string {
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(endpoint.Hostname()), port)
}
