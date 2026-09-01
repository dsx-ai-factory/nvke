// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"fmt"

	api "github.com/NVIDIA/nke/controller/api"
)

// RegisterSite registers a site with CWE and returns the stored site.
func (c *CWEClient) RegisterSite(ctx context.Context, req api.RegisterSiteJSONRequestBody) (*api.Site, error) {
	resp, err := c.raw.RegisterSiteWithResponse(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("registering site: %w", err)
	}
	if resp.StatusCode() != 201 {
		return nil, fmt.Errorf("registering site: %w", apiError(resp.Status(), resp.Body))
	}
	if resp.JSON201 == nil {
		return nil, fmt.Errorf("registering site: empty response body")
	}
	return resp.JSON201, nil
}
