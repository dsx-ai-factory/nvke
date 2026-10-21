// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nico implements the NICo provider's capability clients — starting
// with clients.ClusterScaffold — so provider-specific behavior lives in its
// own package rather than behind a switch shared with other providers.
package nico

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// Client is the NICo provider's capability client. It is stateless: the
// CreateMachine pre-flight's verdict source arrives per call on
// shared.MachineCreateDeps.ImageVerifier, like every other dependency
// Activities supplies.
type Client struct{}

// New returns a NICo provider Client.
func New() *Client {
	return &Client{}
}

// GetClusterScaffoldManifest builds the NicoCluster manifest for a NICo-backed cluster.
func (*Client) GetClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error) {
	provider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}
	nicoCluster := nicov1.NicoCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoCluster",
		},
		Spec: nicov1.NicoClusterSpec{
			SiteID:                provider.SiteID.String(),
			FailureDomainLabelKey: "failure_domain",
		},
	}
	manifest, err := json.Marshal(nicoCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to build NicoCluster manifest for cluster %s: %w", cluster.ID, err)
	}
	return manifest, nil
}
