// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func TestGetNetworkByIDFromAPIBypassesCache(t *testing.T) {
	const (
		orgID       = "test-org"
		siteID      = "test-site"
		subnetID    = "test-subnet"
		vpcPrefixID = "test-vpc-prefix"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("siteId"); got != siteID {
			t.Errorf("siteId = %q, want %q", got, siteID)
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v2/org/" + orgID + "/forge/subnet":
			subnet := forgesdk.NewSubnet()
			subnet.SetId(subnetID)
			if err := json.NewEncoder(w).Encode([]forgesdk.Subnet{*subnet}); err != nil {
				t.Errorf("encoding subnet response: %v", err)
			}
		case "/v2/org/" + orgID + "/forge/vpc-prefix":
			vpcPrefix := forgesdk.NewVpcPrefix()
			vpcPrefix.SetId(vpcPrefixID)
			if err := json.NewEncoder(w).Encode([]forgesdk.VpcPrefix{*vpcPrefix}); err != nil {
				t.Errorf("encoding VPC prefix response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	provider := NewForgeProvider(config.Config{
		ForgeEnvironment: config.ForgeEnvironmentTest,
		ForgeURL:         server.URL,
	}, nil, slog.New(slog.DiscardHandler))

	subnet, err := provider.GetSubnetByIDFromAPI(context.Background(), orgID, siteID, subnetID)
	require.NoError(t, err)
	require.Equal(t, subnetID, subnet.GetId())

	vpcPrefix, err := provider.GetVpcPrefixByIDFromAPI(context.Background(), orgID, siteID, vpcPrefixID)
	require.NoError(t, err)
	require.Equal(t, vpcPrefixID, vpcPrefix.GetId())
}
