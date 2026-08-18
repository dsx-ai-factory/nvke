// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
