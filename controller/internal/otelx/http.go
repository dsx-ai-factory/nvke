// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTPClient returns a shallow copy of base whose Transport is wrapped with
// otelhttp so outbound requests emit client spans and propagate W3C context.
// If base is nil, a new client using http.DefaultTransport is returned.
func HTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	clone := *base
	clone.Transport = otelhttp.NewTransport(transport)
	return &clone
}
