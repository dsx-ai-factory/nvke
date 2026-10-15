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
	"time"

	"github.com/prometheus/common/model"
)

const (
	datasourcePrometheus = "prometheus"
	datasourceLoki       = "loki"
	datasourceTempo      = "tempo"
)

// grafana reaches the backend's datasources through Grafana's proxy, so every
// query speaks the datasource's own API. The proxies are listed once, by
// datasource type: the UIDs are the backend image's to choose.
type grafana struct {
	client  *http.Client
	proxies map[string]string
}

func newGrafana(ctx context.Context, client *http.Client, base string) (*grafana, error) {
	base = strings.TrimSuffix(base, "/")
	var listed []struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
	}
	if err := getJSON(ctx, client, base+"/api/datasources", &listed); err != nil {
		return nil, fmt.Errorf("listing Grafana datasources: %w", err)
	}
	g := &grafana{client: client, proxies: make(map[string]string, len(listed))}
	for _, ds := range listed {
		if _, seen := g.proxies[ds.Type]; !seen {
			g.proxies[ds.Type] = base + "/api/datasources/proxy/uid/" + url.PathEscape(ds.UID)
		}
	}
	for _, datasource := range []string{datasourcePrometheus, datasourceLoki, datasourceTempo} {
		if _, ok := g.proxies[datasource]; !ok {
			return nil, fmt.Errorf("grafana has no %s datasource, only %s", datasource, strings.Join(slices.Sorted(maps.Keys(g.proxies)), ", "))
		}
	}
	return g, nil
}

// vector runs an instant query against Prometheus or Loki, which answer in the
// same format.
func (g *grafana) vector(ctx context.Context, datasource, expr string) (model.Vector, error) {
	path := "/api/v1/query"
	if datasource == datasourceLoki {
		path = "/loki/api/v1/query"
	}
	var response struct {
		Data struct {
			ResultType string       `json:"resultType"`
			Result     model.Vector `json:"result"`
		} `json:"data"`
	}
	if err := getJSON(ctx, g.client, g.proxies[datasource]+path+"?"+url.Values{"query": {expr}}.Encode(), &response); err != nil {
		return nil, fmt.Errorf("%s query %s: %w", datasource, expr, err)
	}
	if response.Data.ResultType != model.ValVector.String() {
		return nil, fmt.Errorf("%s query %s returned a %q, not a vector", datasource, expr, response.Data.ResultType)
	}
	return response.Data.Result, nil
}

// traces counts the traces a TraceQL search finds among those started within
// window.
func (g *grafana) traces(ctx context.Context, query string, window time.Duration) (int, error) {
	now := time.Now()
	params := url.Values{
		"q":     {query},
		"start": {strconv.FormatInt(now.Add(-window).Unix(), 10)},
		"end":   {strconv.FormatInt(now.Add(time.Minute).Unix(), 10)},
	}
	var response struct {
		Traces []json.RawMessage `json:"traces"`
	}
	if err := getJSON(ctx, g.client, g.proxies[datasourceTempo]+"/api/search?"+params.Encode(), &response); err != nil {
		return 0, fmt.Errorf("TraceQL %s: %w", query, err)
	}
	return len(response.Traces), nil
}

func getJSON(ctx context.Context, client *http.Client, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %.512s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding the response: %w: %.512s", err, body)
	}
	return nil
}
