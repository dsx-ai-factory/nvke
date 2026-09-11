//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nicovms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// nicoClient implements only the instance lifecycle calls used by this test.
// Keeping this wire-level facade local avoids making the controller module
// depend on the full NICo SDK (which it no longer uses) for a build-tagged test helper.
type nicoClient struct {
	baseURL    string
	httpClient *http.Client
}

type instanceCreateRequest struct {
	Name             string                   `json:"name"`
	TenantID         string                   `json:"tenantId"`
	InstanceTypeID   string                   `json:"instanceTypeId"`
	VPCID            string                   `json:"vpcId"`
	Labels           map[string]string        `json:"labels"`
	Interfaces       []interfaceCreateRequest `json:"interfaces"`
	UserData         string                   `json:"userData"`
	PhoneHomeEnabled bool                     `json:"phoneHomeEnabled"`
	IpxeScript       *string                  `json:"ipxeScript,omitempty"`
}

type interfaceCreateRequest struct {
	SubnetID string `json:"subnetId"`
}

type instanceResponse struct {
	ID         string              `json:"id"`
	Status     string              `json:"status"`
	Interfaces []interfaceResponse `json:"interfaces"`
}

type interfaceResponse struct {
	IPAddresses []string `json:"ipAddresses"`
}

func newNicoClient(baseURL string) *nicoClient {
	return &nicoClient{
		baseURL:    baseURL,
		httpClient: &http.Client{},
	}
}

func (c *nicoClient) createInstance(
	ctx context.Context,
	request instanceCreateRequest,
) (instanceResponse, error) {
	var instance instanceResponse
	_, err := c.doJSON(ctx, http.MethodPost, c.instancePath(""), request, &instance)
	return instance, err
}

func (c *nicoClient) getInstance(ctx context.Context, instanceID string) (instanceResponse, int, error) {
	var instance instanceResponse
	statusCode, err := c.doJSON(ctx, http.MethodGet, c.instancePath(instanceID), nil, &instance)
	return instance, statusCode, err
}

func (c *nicoClient) deleteInstance(ctx context.Context, instanceID string) error {
	_, err := c.doJSON(ctx, http.MethodDelete, c.instancePath(instanceID), nil, nil)
	return err
}

func (c *nicoClient) instancePath(instanceID string) string {
	path := c.baseURL + "/v1/orgs/" + url.PathEscape(DefaultOrgID) + "/instances"
	if instanceID != "" {
		path += "/" + url.PathEscape(instanceID)
	}
	return path
}

func (c *nicoClient) doJSON(
	ctx context.Context,
	method string,
	endpoint string,
	payload any,
	result any,
) (int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+DefaultBearerToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, fmt.Errorf("%s %s returned %s: %s",
			method, endpoint, resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if result != nil && len(responseBody) != 0 {
		if err := json.Unmarshal(responseBody, result); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}
