// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestResolveClusterNetwork(t *testing.T) {
	tests := []struct {
		name        string
		config      *api.ClusterNetworkCreateConfig
		wantPod     string
		wantService string
		wantError   string
	}{
		{
			name:        "defaults",
			wantPod:     defaultClusterPodCIDR,
			wantService: defaultClusterServiceCIDR,
		},
		{
			name: "canonicalizes host bits",
			config: &api.ClusterNetworkCreateConfig{
				PodCIDR:     "10.233.65.7/18",
				ServiceCIDR: "10.233.1.5/18",
			},
			wantPod:     "10.233.64.0/18",
			wantService: "10.233.0.0/18",
		},
		{
			name: "canonicalizes multiple pod CIDRs",
			config: &api.ClusterNetworkCreateConfig{
				PodCIDR:     "10.240.1.1/16, 10.241.1.1/16",
				ServiceCIDR: "10.120.0.0/16",
			},
			wantPod:     "10.240.0.0/16,10.241.0.0/16",
			wantService: "10.120.0.0/16",
		},
		{
			name: "defaults an omitted field",
			config: &api.ClusterNetworkCreateConfig{
				ServiceCIDR: "172.20.0.0/16",
			},
			wantPod:     defaultClusterPodCIDR,
			wantService: "172.20.0.0/16",
		},
		{
			name:      "rejects malformed CIDR",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "not-a-cidr"},
			wantError: "not a valid CIDR",
		},
		{
			name:      "rejects empty pod CIDR list entries",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "10.240.0.0/16,"},
			wantError: "empty CIDR entry",
		},
		{
			name:      "rejects IPv6",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "fd00::/64"},
			wantError: "must be an IPv4 CIDR",
		},
		{
			name:      "rejects public address space",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "8.8.0.0/16"},
			wantError: "RFC 1918 private address space",
		},
		{
			name:      "rejects prefix not fully contained in private address space",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "192.168.0.0/15"},
			wantError: "RFC 1918 private address space",
		},
		{
			name:      "rejects range larger than supported",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "10.0.0.0/8"},
			wantError: "prefix length between /12 and /23",
		},
		{
			name:      "rejects node-sized pod CIDR",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "10.240.0.0/24"},
			wantError: "prefix length between /12 and /23",
		},
		{
			name:      "rejects range smaller than supported",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "10.0.0.0/25"},
			wantError: "prefix length between /12 and /23",
		},
		{
			name:      "rejects OVN host subnet suffix",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "10.240.0.0/16/24"},
			wantError: "not a valid CIDR",
		},
		{
			name:      "rejects overlapping pod CIDRs",
			config:    &api.ClusterNetworkCreateConfig{PodCIDR: "10.240.0.0/16,10.240.128.0/17"},
			wantError: "overlaps",
		},
		{
			name: "accepts a /24 service CIDR",
			config: &api.ClusterNetworkCreateConfig{
				PodCIDR:     "10.240.0.0/23",
				ServiceCIDR: "10.120.0.0/24",
			},
			wantPod:     "10.240.0.0/23",
			wantService: "10.120.0.0/24",
		},
		{
			name: "rejects overlapping pod and service ranges",
			config: &api.ClusterNetworkCreateConfig{
				PodCIDR:     "10.233.0.0/16",
				ServiceCIDR: "10.233.0.0/18",
			},
			wantError: "must not overlap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network, err := resolveClusterNetwork(tt.config)
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPod, network.podCIDR)
			assert.Equal(t, tt.wantService, network.serviceCIDR.String())
		})
	}
}

func TestResolvedClusterNetworkValidateCNI(t *testing.T) {
	defaultNetwork, err := resolveClusterNetwork(nil)
	require.NoError(t, err)
	assert.NoError(t, defaultNetwork.validateCNI(db.CNICalico))

	customNetwork, err := resolveClusterNetwork(&api.ClusterNetworkCreateConfig{
		PodCIDR:     "10.240.0.0/16,10.241.0.0/16",
		ServiceCIDR: "10.120.0.0/16",
	})
	require.NoError(t, err)
	assert.NoError(t, customNetwork.validateCNI(db.CNIOvnKubernetes))
	require.ErrorContains(t, customNetwork.validateCNI(db.CNICalico), "custom pod or service CIDRs require")
}

func TestValidatePodCIDRCapacity(t *testing.T) {
	tests := []struct {
		name            string
		podCIDR         string
		desiredNodes    int
		configuredSurge int
		wantError       string
	}{
		{
			name:         "/24 has no scaling headroom",
			podCIDR:      "10.240.0.0/24",
			desiredNodes: 1,
			wantError:    "provides 1 /24 node blocks, but 2 are required",
		},
		{
			name:         "/23 supports one node and one spare",
			podCIDR:      "10.240.0.0/23",
			desiredNodes: 1,
		},
		{
			name:         "/22 supports three nodes and one spare",
			podCIDR:      "10.240.0.0/22",
			desiredNodes: 3,
		},
		{
			name:         "/22 rejects four nodes without headroom",
			podCIDR:      "10.240.0.0/22",
			desiredNodes: 4,
			wantError:    "provides 4 /24 node blocks, but 5 are required",
		},
		{
			name:            "/21 supports configured surge greater than minimum headroom",
			podCIDR:         "10.240.0.0/21",
			desiredNodes:    4,
			configuredSurge: 2,
		},
		{
			name:         "multiple ranges contribute to capacity",
			podCIDR:      "10.240.0.0/23,10.242.0.0/23",
			desiredNodes: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := strings.Split(tt.podCIDR, ",")
			prefixes := make([]netip.Prefix, 0, len(parts))
			for _, part := range parts {
				prefixes = append(prefixes, netip.MustParsePrefix(part))
			}
			err := validatePodCIDRCapacity(
				prefixes,
				tt.desiredNodes,
				tt.configuredSurge,
			)
			if tt.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestRequiredPodCIDRNodeBlocks(t *testing.T) {
	nodePools := []db.NodePool{
		{DesiredCount: 3, MaxSurge: 1},
		{DesiredCount: 1, MaxSurge: 0},
	}

	assert.Equal(t, 6, requiredPodCIDRNodeBlocks(nodePools, 1))
}

func TestResolvedClusterNetworkValidateControlPlaneVIP(t *testing.T) {
	network, err := resolveClusterNetwork(&api.ClusterNetworkCreateConfig{
		PodCIDR:     "10.232.64.0/18,10.233.64.0/18",
		ServiceCIDR: "10.233.0.0/18",
	})
	require.NoError(t, err)

	require.ErrorContains(t, network.validateControlPlaneVIP("10.233.64.10"), "network.podCIDR")
	require.ErrorContains(t, network.validateControlPlaneVIP("10.233.0.10"), "network.serviceCIDR")
	assert.NoError(t, network.validateControlPlaneVIP("172.31.0.10"))
}

type fakeForgeClusterNetworkProvider struct {
	subnet    *forgesdk.Subnet
	vpcPrefix *forgesdk.VpcPrefix
}

func (f *fakeForgeClusterNetworkProvider) GetSubnetByIDFromAPI(
	_ context.Context,
	_, _, _ string,
) (*forgesdk.Subnet, error) {
	return f.subnet, nil
}

func (f *fakeForgeClusterNetworkProvider) GetVpcPrefixByIDFromAPI(
	_ context.Context,
	_, _, _ string,
) (*forgesdk.VpcPrefix, error) {
	return f.vpcPrefix, nil
}

func TestValidateSelectedForgeNetworkCIDRs(t *testing.T) {
	network, err := resolveClusterNetwork(&api.ClusterNetworkCreateConfig{
		PodCIDR:     "10.233.64.0/18",
		ServiceCIDR: "10.233.0.0/18",
	})
	require.NoError(t, err)

	t.Run("skips generic CAPI cluster without Forge provider", func(t *testing.T) {
		service := &clusterService{}
		err := service.validateSelectedForgeNetworkCIDRs(context.Background(), &api.CreateClusterRequest{
			SiteID: uuid.Nil,
		}, network)
		assert.NoError(t, err)
	})

	subnetID := uuid.New()
	vpcPrefixID := uuid.New()

	overlappingSubnet := forgesdk.NewSubnet()
	overlappingSubnet.SetIpv4Prefix("10.233.64.0")
	overlappingSubnet.SetPrefixLength(24)
	service := &clusterService{
		forgeNetworkProvider: &fakeForgeClusterNetworkProvider{subnet: overlappingSubnet},
	}
	err = service.validateSelectedForgeNetworkCIDRs(context.Background(), &api.CreateClusterRequest{
		OrgID:  "test-org",
		SiteID: uuid.New(),
		NodePools: []api.NodePoolCreateConfig{{
			SubnetIDs: []uuid.UUID{subnetID},
		}},
	}, network)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlaps selected Forge subnet")

	overlappingVpcPrefix := forgesdk.NewVpcPrefix()
	overlappingVpcPrefix.SetPrefix("10.233.0.0/24")
	service.forgeNetworkProvider = &fakeForgeClusterNetworkProvider{vpcPrefix: overlappingVpcPrefix}
	err = service.validateSelectedForgeNetworkCIDRs(context.Background(), &api.CreateClusterRequest{
		OrgID:  "test-org",
		SiteID: uuid.New(),
		NodePools: []api.NodePoolCreateConfig{{
			VpcPrefixIDs: []uuid.UUID{vpcPrefixID},
		}},
	}, network)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlaps selected Forge VPC prefix")

	safeSubnet := forgesdk.NewSubnet()
	safeSubnet.SetIpv4Prefix("172.20.0.0")
	safeSubnet.SetPrefixLength(16)
	service.forgeNetworkProvider = &fakeForgeClusterNetworkProvider{subnet: safeSubnet}
	err = service.validateSelectedForgeNetworkCIDRs(context.Background(), &api.CreateClusterRequest{
		OrgID:  "test-org",
		SiteID: uuid.New(),
		NodePools: []api.NodePoolCreateConfig{{
			SubnetIDs: []uuid.UUID{subnetID},
		}},
	}, network)
	assert.NoError(t, err)
}
