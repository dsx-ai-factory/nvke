// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nvkecontroller adapts the existing NVKE controller cluster API for reconciliation.
// This will be removed with the migration to CAPI.
package nvkecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	api "github.com/dsx-ai-factory/nvke/controller/api"
)

const maxResponseBytes = 1 << 20

var (
	// ErrNotFound means the requested cluster does not exist.
	ErrNotFound = errors.New("NVKE controller cluster not found")
	// ErrInvalidResponse means a successful response cannot identify the requested cluster.
	ErrInvalidResponse = errors.New("NVKE controller returned an invalid cluster response")
)

// HTTPError reports a rejected request without exposing response contents.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("NVKE controller request returned HTTP %d", e.StatusCode)
}

// Config selects the operator-controlled NVKE controller endpoint and optional bearer token file.
type Config struct {
	Endpoint  string
	TokenFile string
}

// Client validates cluster identity and redacts failures from generated NVKE controller requests.
type Client struct {
	api      *api.Client
	endpoint string
}

// New constructs a client with bounded requests, verified TLS and no redirects.
func New(cfg Config) (*Client, error) {
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.TokenFile != "" {
		if _, err := readToken(cfg.TokenFile); err != nil {
			return nil, err
		}
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	raw, err := api.NewClient(endpoint, api.WithHTTPClient(httpClient), api.WithRequestEditorFn(
		func(_ context.Context, req *http.Request) error {
			if cfg.TokenFile == "" {
				return nil
			}
			// Reload projected tokens so credential rotation does not require a restart.
			token, err := readToken(cfg.TokenFile)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
	))
	if err != nil {
		return nil, errors.New("could not construct NVKE controller API client")
	}
	return &Client{api: raw, endpoint: endpoint}, nil
}

// Endpoint returns the normalized API URL for checking persisted backend identity.
func (c *Client) Endpoint() string { return c.endpoint }

// FindByName finds a single exact-name match. It does not establish ownership.
func (c *Client) FindByName(ctx context.Context, name string) (*api.Cluster, error) {
	if name == "" {
		return nil, errors.New("NVKE controller cluster name is required")
	}
	response, err := c.api.ListClusters(ctx, &api.ListClustersParams{Name: &name})
	var result api.ListClustersResponse
	if err := readResponse(response, err, http.StatusOK, &result); err != nil {
		return nil, err
	}
	if result.Clusters == nil {
		return nil, ErrInvalidResponse
	}
	if len(result.Clusters) == 0 {
		return nil, ErrNotFound
	}
	if len(result.Clusters) != 1 || result.Clusters[0].Name != name || result.Clusters[0].ID == uuid.Nil {
		return nil, ErrInvalidResponse
	}
	return &result.Clusters[0], nil
}

// Get reads the cluster with the given server-assigned UUID.
func (c *Client) Get(ctx context.Context, id string) (*api.Cluster, error) {
	clusterID, err := parseID(id)
	if err != nil {
		return nil, err
	}
	response, err := c.api.GetCluster(ctx, clusterID)
	var result api.Cluster
	if err := readResponse(response, err, http.StatusOK, &result); err != nil {
		return nil, err
	}
	if result.ID != clusterID || result.Name == "" {
		return nil, ErrInvalidResponse
	}
	return &result, nil
}

// Create submits the existing NVKE controller request. Callers must persist intent before calling it.
func (c *Client) Create(ctx context.Context, request api.CreateClusterRequest) (*api.Cluster, error) {
	if request.Name == "" {
		return nil, errors.New("NVKE controller cluster name is required")
	}
	response, err := c.api.CreateCluster(ctx, request)
	var result api.Cluster
	if err := readResponse(response, err, http.StatusCreated, &result); err != nil {
		return nil, err
	}
	if result.ID == uuid.Nil || result.Name != request.Name {
		return nil, ErrInvalidResponse
	}
	return &result, nil
}

// Delete requests asynchronous deletion. A missing cluster is already deleted.
func (c *Client) Delete(ctx context.Context, id string) error {
	clusterID, err := parseID(id)
	if err != nil {
		return err
	}
	response, err := c.api.DeleteCluster(ctx, clusterID)
	err = readResponse(response, err, http.StatusAccepted, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func readResponse(response *http.Response, requestErr error, expected int, target any) error {
	if requestErr != nil {
		// Transport errors can contain credentials. Preserve only safe context error identities.
		if errors.Is(requestErr, context.Canceled) {
			return fmt.Errorf("NVKE controller request canceled: %w", context.Canceled)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) {
			return fmt.Errorf("NVKE controller request timed out: %w", context.DeadlineExceeded)
		}
		return errors.New("NVKE controller request failed before a response was received")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != expected {
		return &HTTPError{StatusCode: response.StatusCode}
	}
	if target == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes || json.Unmarshal(body, target) != nil {
		return ErrInvalidResponse
	}
	return nil
}

func parseID(id string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, errors.New("NVKE controller cluster ID must be a nonzero UUID")
	}
	return parsed, nil
}

func readToken(filename string) (string, error) {
	contents, err := os.ReadFile(filename)
	if err != nil {
		return "", errors.New("could not read NVKE controller bearer token file")
	}
	token := strings.TrimSpace(string(contents))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("NVKE controller bearer token file is empty or malformed")
	}
	return token, nil
}

func normalizeEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("NVKE controller endpoint must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	parsed.Path = strings.TrimRight(path.Clean(parsed.Path), "/")
	if parsed.Path == "." {
		parsed.Path = ""
	}
	if !strings.HasSuffix(parsed.Path, "/api") {
		parsed.Path += "/api"
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}
