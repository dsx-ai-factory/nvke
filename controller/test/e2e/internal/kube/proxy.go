// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"k8s.io/client-go/rest"
)

// ServiceProxy issues plain HTTP requests to a Service through the Kubernetes
// API server's service-proxy subresource, reusing the REST config's auth and
// TLS transport — the same recipe internal/nke.NewClient uses for the CWE API.
// A plain *http.Client is used instead of the typed RESTClient because the
// typed client folds non-2xx proxied responses into errors and drops their
// status code; here any response comes back as data with its code and body
// intact, so callers can assert on e.g. a readyz 503 and surface its JSON body.
type ServiceProxy struct {
	httpClient *http.Client
	baseURL    string
}

// NewServiceProxy builds a proxy client for namespace/service:port.
func NewServiceProxy(restConfig *rest.Config, namespace, service, port string) (*ServiceProxy, error) {
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building HTTP client: %w", err)
	}

	baseURL := strings.TrimRight(restConfig.Host, "/") +
		"/api/v1/namespaces/" + namespace + "/services/" + service + ":" + port + "/proxy"

	return &ServiceProxy{httpClient: httpClient, baseURL: baseURL}, nil
}

// Get performs a GET of path through the proxy and returns the HTTP status
// code and response body. Any response, 2xx or not, is returned as data; the
// error is non-nil only for transport-level failures.
func (p *ServiceProxy) Get(ctx context.Context, path string) (int, []byte, error) {
	url := p.baseURL + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, nil, fmt.Errorf("building request for %s: %w", path, err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("proxying GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response body for %s: %w", path, err)
	}

	return resp.StatusCode, body, nil
}
