// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nke

import (
	"testing"

	"github.com/NVIDIA/nke/controller/api"
)

func TestApplyNetworkConfig(t *testing.T) {
	tests := []struct {
		name string
		spec *ClusterNetworkSpec
		want bool
	}{
		{name: "nil", spec: nil, want: false},
		{name: "empty", spec: &ClusterNetworkSpec{}, want: false},
		{name: "blank", spec: &ClusterNetworkSpec{PodCIDR: " ", ServiceCIDR: "\t"}, want: false},
		{name: "configured", spec: &ClusterNetworkSpec{PodCIDR: "10.250.0.0/16"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req api.CreateClusterRequest
			applyNetworkConfig(&req, tt.spec)
			if (req.Network != nil) != tt.want {
				t.Fatalf("Network configured = %t, want %t", req.Network != nil, tt.want)
			}
		})
	}
}
