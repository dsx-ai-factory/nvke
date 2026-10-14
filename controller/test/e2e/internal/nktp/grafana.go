// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
)

const (
	datasourcePrometheus = "prometheus"
	datasourceLoki       = "loki"
	datasourceTempo      = "tempo"

	errorExcerpt = 512
)

// grafana reaches the backend's datasources through Grafana's proxy, so every
// query speaks the datasource's own API.
type grafana struct {
	client *http.Client
	base   string

	mu sync.Mutex
	// proxy URL by datasource type, listed on first use: the UIDs are the
	// backend image's to choose.
	proxies map[string]string
}

func newGrafana(client *http.Client, base string) *grafana {
	return &grafana{client: client, base: strings.TrimSuffix(base, "/")}
}

func (g *grafana) promQL(ctx context.Context, expr string) (model.Vector, error) {
	return g.vector(ctx, datasourcePrometheus, "/api/v1/query", expr)
}

func (g *grafana) logQL(ctx context.Context, expr string) (model.Vector, error) {
	return g.vector(ctx, datasourceLoki, "/loki/api/v1/query", expr)
}

// vector runs an instant query; Prometheus and Loki answer in the same format.
func (g *grafana) vector(ctx context.Context, datasourceType, path, expr string) (model.Vector, error) {
	proxy, err := g.proxy(ctx, datasourceType)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			ResultType string       `json:"resultType"`
			Result     model.Vector `json:"result"`
		} `json:"data"`
	}
	if err := g.getJSON(ctx, proxy+path+"?"+url.Values{"query": {expr}}.Encode(), &response); err != nil {
		return nil, fmt.Errorf("%s query %s: %w", datasourceType, expr, err)
	}
	if response.Data.ResultType != model.ValVector.String() {
		return nil, fmt.Errorf("%s query %s returned a %q, not a vector", datasourceType, expr, response.Data.ResultType)
	}
	return response.Data.Result, nil
}

// traceQL counts the traces a search finds among those started within window.
func (g *grafana) traceQL(ctx context.Context, query string, window time.Duration) (int, error) {
	proxy, err := g.proxy(ctx, datasourceTempo)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	params := url.Values{
		"q":     {query},
		"start": {strconv.FormatInt(now.Add(-window).Unix(), 10)},
		"end":   {strconv.FormatInt(now.Add(time.Minute).Unix(), 10)},
	}
	var response struct {
		Traces []json.RawMessage `json:"traces"`
	}
	if err := g.getJSON(ctx, proxy+"/api/search?"+params.Encode(), &response); err != nil {
		return 0, fmt.Errorf("TraceQL %s: %w", query, err)
	}
	return len(response.Traces), nil
}

func (g *grafana) proxy(ctx context.Context, datasourceType string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.proxies == nil {
		var listed []struct {
			UID  string `json:"uid"`
			Type string `json:"type"`
		}
		if err := g.getJSON(ctx, g.base+"/api/datasources", &listed); err != nil {
			return "", fmt.Errorf("listing Grafana datasources: %w", err)
		}
		proxies := make(map[string]string, len(listed))
		for _, ds := range listed {
			if _, seen := proxies[ds.Type]; !seen {
				proxies[ds.Type] = g.base + "/api/datasources/proxy/uid/" + url.PathEscape(ds.UID)
			}
		}
		g.proxies = proxies
	}
	proxy, ok := g.proxies[datasourceType]
	if !ok {
		return "", fmt.Errorf("grafana has no %s datasource, only %s",
			datasourceType, strings.Join(slices.Sorted(maps.Keys(g.proxies)), ", "))
	}
	return proxy, nil
}

func (g *grafana) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, excerpt(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding the response: %w: %s", err, excerpt(body))
	}
	return nil
}

func excerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > errorExcerpt {
		return text[:errorExcerpt] + "..."
	}
	return text
}
