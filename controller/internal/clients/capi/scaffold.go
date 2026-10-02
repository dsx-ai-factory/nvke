// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package capi implements the CAPI provider's capability clients — clusters
// where the caller supplies the CAPI infrastructure template directly.
package capi

import (
	"encoding/json"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// Client is the CAPI provider's capability client.
type Client struct{}

// New returns a CAPI provider Client.
func New() *Client {
	return &Client{}
}

// GetClusterScaffoldManifest returns the caller-supplied CAPI infrastructure
// template for a CAPI-backed cluster.
func (*Client) GetClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error) {
	provider, err := cluster.CAPIProvider()
	if err != nil {
		return nil, err
	}
	return provider.InfraCluster, nil
}
